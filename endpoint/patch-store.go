/*
 * Copyright 2019 Abstrium SAS
 *
 *  This file is part of Cells Sync.
 *
 *  Cells Sync is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU General Public License as published by
 *  the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  Cells Sync is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *  GNU General Public License for more details.
 *
 *  You should have received a copy of the GNU General Public License
 *  along with Cells Sync.  If not, see <https://www.gnu.org/licenses/>.
 */

// Package endpoint provides some specific factories to provide Endpoints to the sync library
package endpoint

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"github.com/pydio/cells/common/log"
	"github.com/pydio/cells/common/sync/merger"
	"github.com/pydio/cells/common/sync/model"
)

var (
	patchBucket    = []byte("patches")
	timeKey        = []byte("stamp")
	opsKey         = []byte("operations")
	patchErrKey    = []byte("patchError")
	patchSourceKey = []byte("source")
)

type patchSorter []merger.Patch

func (p patchSorter) Len() int {
	return len(p)
}
func (p patchSorter) Less(i, j int) bool {
	return p[i].GetStamp().After(p[j].GetStamp())
}
func (p patchSorter) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

// PatchStore is a persistence layer for storing patches. It is based on BoltDB
type PatchStore struct {
	patches  chan merger.Patch
	done     chan bool
	pipeDone chan bool

	source model.Endpoint
	target model.Endpoint

	db            *bbolt.DB
	folderPath    string
	lastHasErrors bool
}

// NewPatchStore opens a new PatchStore
func NewPatchStore(folderPath string, source model.Endpoint, target model.Endpoint) (*PatchStore, error) {
	p := &PatchStore{
		patches: make(chan merger.Patch),
		done:    make(chan bool, 1),
		source:  source,
		target:  target,
	}

	options := bbolt.DefaultOptions
	options.Timeout = 5 * time.Second
	p.folderPath = folderPath
	dbPath := filepath.Join(p.folderPath, "patches")
	db, err := bbolt.Open(dbPath, 0644, options)
	if err != nil {
		return nil, err
	}
	p.db = db

	// Load last known patch status (error or not)
	if last, e := p.Load(0, 1); e == nil && len(last) > 0 {
		_, p.lastHasErrors = last[0].HasErrors()
	}

	go func() {
		for patch := range p.patches {
			p.persist(patch)
		}
	}()
	return p, nil
}

// Store pushes the patch to the DB.
func (p *PatchStore) Store(patch merger.Patch) {
	p.patches <- patch
}

func (p *PatchStore) unmarshalConflict(data []byte, op merger.Operation) (merger.Operation, error) {
	if op.Type() != merger.OpConflict {
		return op, nil
	}
	n := op.GetNode()
	var cType merger.ConflictType
	var leftOp, rightOp merger.Operation
	var ii map[string]interface{}
	if err := json.Unmarshal(data, &ii); err != nil {
		return nil, err
	}
	if t, o := ii["ConflictType"]; o {
		cType = merger.ConflictType(int(t.(float64)))
	} else {
		return nil, fmt.Errorf("unmarshalling conflict: missing key ConflictType")
	}
	if left, o := ii["LeftOp"]; o {
		remarsh, _ := json.Marshal(left)
		leftOp = merger.NewOpForUnmarshall()
		if e := json.Unmarshal(remarsh, &leftOp); e != nil {
			return nil, e
		}
	} else {
		return nil, fmt.Errorf("unmarshalling conflict: missing key LeftOp")
	}
	if right, o := ii["RightOp"]; o {
		remarsh, _ := json.Marshal(right)
		rightOp = merger.NewOpForUnmarshall()
		if e := json.Unmarshal(remarsh, &rightOp); e != nil {
			return nil, e
		}
	} else {
		return nil, fmt.Errorf("unmarshalling conflict: missing key RightOp")
	}
	// replace op now
	conflict := merger.NewConflictOperation(n, cType, leftOp, rightOp)
	return conflict, nil
}

// Load list all patches
func (p *PatchStore) Load(offset, limit int) (patches []merger.Patch, e error) {
	var stamps patchSorter

	e = p.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(patchBucket)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			patch := merger.NewPatch(p.source.(model.PathSyncSource), p.target.(model.PathSyncTarget), merger.PatchOptions{})
			// Set the UUID of the patch
			patch.SetUUID(string(k))
			// v is a bucket containing all operations
			patchBucket := bucket.Bucket(k)
			if errValue := patchBucket.Get(patchErrKey); errValue != nil {
				// Do this before unmarshalling tStamp otherwise it overwrites internal mtime
				patch.SetPatchError(fmt.Errorf(string(errValue)))
			}
			if src := patchBucket.Get(patchSourceKey); src != nil && string(src) != p.source.GetEndpointInfo().URI {
				// Invert target and source
				patch.Source(p.target.(model.PathSyncSource))
				patch.Target(p.source.(model.PathSyncTarget))
			}
			stamp := patchBucket.Get(timeKey)
			t := time.Now()
			if err := t.UnmarshalJSON(stamp); err == nil {
				patch.Stamp(t)
			}
			opsBucket := patchBucket.Bucket(opsKey)
			oc := opsBucket.Cursor()
			for _, v := oc.First(); v != nil; _, v = oc.Next() {
				operation := merger.NewOpForUnmarshall()
				if err := json.Unmarshal(v, &operation); err == nil {
					if operation, err = p.unmarshalConflict(v, operation); err != nil {
						log.Logger(context.Background()).Error("Cannot unmarshall conflict operation:" + err.Error())
					}
					patch.Enqueue(operation)
				} else {
					log.Logger(context.Background()).Error("Cannot unmarshall operation:" + err.Error())
				}
			}
			stamps = append(stamps, patch)
		}
		return nil
	})
	if e != nil {
		return patches, e
	}
	// Order patches by timestamp
	sort.Sort(stamps)
	var prunes []string
	if len(stamps) > 100 {
		for _, pr := range stamps[100:] {
			prunes = append(prunes, pr.GetUUID())
		}
	}
	for i, patch := range stamps {
		if i < offset {
			continue
		}
		patches = append(patches, patch)
		if i >= offset+limit-1 {
			break
		}
	}

	if len(prunes) > 0 {
		go func() {
			log.Logger(context.Background()).Info("Pruning patch store")
			p.db.Update(func(tx *bbolt.Tx) error {
				bucket := tx.Bucket(patchBucket)
				if bucket == nil {
					return nil
				}
				for _, uuid := range prunes {
					if e := bucket.DeleteBucket([]byte(uuid)); e != nil {
						log.Logger(context.Background()).Error("cannot delete bucket " + uuid + " - " + e.Error())
					}
				}
				return nil
			})
		}()
	}

	return
}

// Stop closes the DB.
func (p *PatchStore) Stop() {
	close(p.done)
	if p.pipeDone != nil {
		close(p.pipeDone)
	}
	p.db.Close()
}

// PublishPatch pushes patch to the persist queue
func (p *PatchStore) PublishPatch(patch merger.Patch) {
	p.patches <- patch
}

func (p *PatchStore) persist(patch merger.Patch) {
	_, has := patch.HasErrors()
	// Do not store empty/no-error patch, except if previous had error
	if patch.Size() == 0 && !has && !p.lastHasErrors {
		return
	}
	p.lastHasErrors = has
	p.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(patchBucket)
		if err != nil {
			return err
		}
		// Fully replace bucket content
		bName := []byte(patch.GetUUID())
		if opsBucket := bucket.Bucket(bName); opsBucket != nil {
			bucket.DeleteBucket(bName)
		}
		patchBucket, err := bucket.CreateBucketIfNotExists(bName)
		if err != nil {
			return err
		}
		mTime, _ := patch.GetStamp().MarshalJSON()
		patchBucket.Put(timeKey, mTime)
		if errs, ok := patch.HasErrors(); ok && len(errs) > 0 {
			patchBucket.Put(patchErrKey, []byte(errs[0].Error()))
		}
		patchBucket.Put(patchSourceKey, []byte(patch.Source().GetEndpointInfo().URI))
		opsBucket, _ := patchBucket.CreateBucket(opsKey)
		patch.WalkOperations([]merger.OperationType{}, func(operation merger.Operation) {
			if data, err := json.Marshal(operation); err == nil {
				id, _ := opsBucket.NextSequence()
				opsBucket.Put(itob(id), data)
			}
		})
		return nil
	})
}

// itob returns an 8-byte big endian representation of v.
func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}
