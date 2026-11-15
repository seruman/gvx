// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package oci

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// The cache directory holds a lock file that orders writers against a prune
// across processes: gvx-docker and rungv share the cache. A writer publishes
// layer and blob files first and writes the image record that uses them
// after. A prune that runs in between sees unused files and deletes them; a
// blob from a commit or a load has no registry source, so the image is lost.
//
// A writer holds the lock shared from its first published file to its image
// record. A prune and a remove hold it exclusive for the whole sweep.
const cacheLockName = ".cache.lock"

// LockWrites takes the cache lock shared, for a writer that publishes layer
// or blob files and then writes the image record that uses them. Callers
// release it after the record is written. Writers do not block each other.
//
// The holds of one LayerCache share one descriptor and count references, so
// a nested hold, such as StoreImage under a caller that holds the lock since
// StoreBlob, never asks the kernel again. Whether the kernel grants a second
// shared request while an exclusive request waits depends on the system; a
// system that queues it behind the prune would deadlock.
func (c *LayerCache) LockWrites() (func(), error) {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	if c.writeHolds == 0 {
		f, err := c.lockCacheFile(false)
		if err != nil {
			return nil, err
		}
		c.writeLock = f
	}
	c.writeHolds++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.lockMu.Lock()
			defer c.lockMu.Unlock()
			c.writeHolds--
			if c.writeHolds == 0 {
				_ = unlockFile(c.writeLock)
				_ = c.writeLock.Close()
				c.writeLock = nil
			}
		})
	}, nil
}

// lockSweep takes the cache lock exclusive, for a prune or a remove. It uses
// its own descriptor, so it also waits for the writers of this process.
func (c *LayerCache) lockSweep() (func(), error) {
	f, err := c.lockCacheFile(true)
	if err != nil {
		return nil, err
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

func (c *LayerCache) lockCacheFile(exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(c.dir, cacheLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("oci: open cache lock: %w", err)
	}
	if err := lockFile(f, exclusive); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("oci: lock cache: %w", err)
	}
	return f, nil
}
