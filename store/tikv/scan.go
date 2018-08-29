// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"fmt"
	"math/rand"

	"github.com/juju/errors"
	pb "github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

// Scanner support tikv scan
type Scanner struct {
	snapshot     *tikvSnapshot
	batchSize    int
	valid        bool
	cache        []*pb.KvPair
	idx          int
	nextStartKey []byte
	eof          bool
}

func newScanner(snapshot *tikvSnapshot, startKey []byte, batchSize int) (*Scanner, error) {
	// It must be > 1. Otherwise scanner won't skipFirst.
	if batchSize <= 1 {
		batchSize = scanBatchSize
	}
	scanner := &Scanner{
		snapshot:     snapshot,
		batchSize:    batchSize,
		valid:        true,
		nextStartKey: startKey,
	}
	err := scanner.Next()
	if kv.IsErrNotFound(err) {
		return scanner, nil
	}
	return scanner, errors.Trace(err)
}

// Valid return valid.
func (s *Scanner) Valid() bool {
	return s.valid
}

// Key return key.
func (s *Scanner) Key() kv.Key {
	if s.valid {
		return s.cache[s.idx].Key
	}
	return nil
}

// Value return value.
func (s *Scanner) Value() []byte {
	if s.valid {
		return s.cache[s.idx].Value
	}
	return nil
}

// Next return next element.
func (s *Scanner) Next() error {
	bo := NewBackoffer(context.Background(), scannerNextMaxBackoff)
	log.Warnf("Next Scanner Iterator")
	if !s.valid {
		return errors.New("scanner iterator is invalid")
	}
	for {
		s.idx++
		log.Warnf("idx = %d", s.idx)
		if s.idx >= len(s.cache) {
			log.Warnf("idx >= len(s.cache) %d", len(s.cache))
			if s.eof {
				s.Close()
				log.Warnf("eof, close scanner")
				return nil
			}
			err := s.getData(bo)
			log.Warnf("get data")
			if err != nil {
				s.Close()
				log.Warnf("error, close scanner")
				return errors.Trace(err)
			}
			if s.idx >= len(s.cache) {
				continue
			}
		}
		if err := s.resolveCurrentLock(bo); err != nil {
			s.Close()
			return errors.Trace(err)
		}
		if len(s.Value()) == 0 {
			// nil stands for NotExist, go to next KV pair.
			log.Warnf("not exist, continue")
			continue
		}
		log.Warnf("return")
		return nil
	}
}

// Close close iterator.
func (s *Scanner) Close() {
	s.valid = false
}

func (s *Scanner) startTS() uint64 {
	return s.snapshot.version.Ver
}

func (s *Scanner) resolveCurrentLock(bo *Backoffer) error {
	current := s.cache[s.idx]
	if current.GetError() == nil {
		return nil
	}
	val, err := s.snapshot.get(bo, kv.Key(current.Key))
	log.Warnf("get key from %s -> val = %s", current.Key, val)
	if err != nil {
		return errors.Trace(err)
	}
	current.Error = nil
	current.Value = val
	return nil
}

func (s *Scanner) getData(bo *Backoffer) error {
	gid := rand.Intn(10000)
	fmt.Printf("%d txn getData nextStartKey = %v, txn %d\n", gid, s.nextStartKey, s.startTS())
	sender := NewRegionRequestSender(s.snapshot.store.regionCache, s.snapshot.store.client)

	for {
		loc, err := s.snapshot.store.regionCache.LocateKey(bo, s.nextStartKey)
		if err != nil {
			return errors.Trace(err)
		}
		fmt.Printf("%d txn getData region %v\n", gid, loc)
		req := &tikvrpc.Request{
			Type: tikvrpc.CmdScan,
			Scan: &pb.ScanRequest{
				StartKey: s.nextStartKey,
				Limit:    uint32(s.batchSize),
				Version:  s.startTS(),
			},
			Context: pb.Context{
				IsolationLevel: pbIsolationLevel(s.snapshot.isolationLevel),
				Priority:       s.snapshot.priority,
				NotFillCache:   s.snapshot.notFillCache,
			},
		}
		fmt.Printf("%d txn getData scanreq[%v] startKey = %v\n", gid, req.Scan, req.Scan.StartKey)
		resp, err := sender.SendReq(bo, req, loc.Region, ReadTimeoutMedium)
		if err != nil {
			return errors.Trace(err)
		}
		regionErr, err := resp.GetRegionError()
		if err != nil {
			return errors.Trace(err)
		}
		if regionErr != nil {
			log.Debugf("scanner getData failed: %s", regionErr)
			err = bo.Backoff(BoRegionMiss, errors.New(regionErr.String()))
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}
		cmdScanResp := resp.Scan
		if cmdScanResp == nil {
			return errors.Trace(ErrBodyMissing)
		}

		err = s.snapshot.store.CheckVisibility(s.startTS())
		if err != nil {
			return errors.Trace(err)
		}

		kvPairs := cmdScanResp.Pairs
		// Check if kvPair contains error, it should be a Lock.
		for _, pair := range kvPairs {
			if keyErr := pair.GetError(); keyErr != nil {
				lock, err := extractLockFromKeyErr(keyErr)
				if err != nil {
					return errors.Trace(err)
				}
				fmt.Printf("%d txn getData lock encountered[%v]\n", gid, lock.Key)
				pair.Key = lock.Key
			}
		}

		s.cache, s.idx = kvPairs, 0
		if len(kvPairs) < s.batchSize {
			// No more data in current Region. Next getData() starts
			// from current Region's endKey.
			s.nextStartKey = loc.EndKey
			if len(loc.EndKey) == 0 {
				// Current Region is the last one.
				s.eof = true
			}
			return nil
		}
		// next getData() starts from the last key in kvPairs (but skip
		// it by appending a '\x00' to the key). Note that next getData()
		// may get an empty response if the Region in fact does not have
		// more data.
		lastKey := kvPairs[len(kvPairs)-1].GetKey()
		s.nextStartKey = kv.Key(lastKey).Next()
		return nil
	}
}
