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

//go:build !windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
)

// networkView is the container store as the network requests of one
// supervisor read it: name lookups, dials, and sessions. It keeps the
// decoded config.json and state.json of each container and decodes a file
// again only when its key changed. The store replaces each file by a rename
// (writeJSONFile), so every write gives the file a new inode, and the key
// sees every change; nothing expires by time. The supervisor lock probe runs
// on each read, so a dead supervisor shows at once.
type networkView struct {
	store *containerStore

	mu      sync.Mutex
	entries map[string]*viewEntry
}

func newNetworkView(store *containerStore) *networkView {
	return &networkView{store: store, entries: map[string]*viewEntry{}}
}

// fileKey identifies the content of one store file.
type fileKey struct {
	ino   uint64
	size  int64
	mtime int64
}

type viewEntry struct {
	configKey, stateKey fileKey
	config              containerRecord
	state               containerState
}

// list returns the records of the store, newest first, as
// containerStore.list does. The returned records share their maps with the
// cache; callers must not change them.
func (v *networkView) list() ([]containerRecord, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	dirs, err := os.ReadDir(v.store.root)
	if errors.Is(err, os.ErrNotExist) {
		clear(v.entries)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	seen := make(map[string]bool, len(dirs))
	records := make([]containerRecord, 0, len(dirs))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		id := d.Name()
		e, err := v.refresh(id)
		if err != nil {
			// A container that is being created or removed has no
			// complete pair of files; containerStore.list skips it too.
			delete(v.entries, id)
			continue
		}
		seen[id] = true
		rec := e.config
		rec.State = e.state
		refreshProcessStateIn(v.store.dir(id), &rec.State)
		records = append(records, rec)
	}
	for id := range v.entries {
		if !seen[id] {
			delete(v.entries, id)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Created.After(records[j].Created) })
	return records, nil
}

// refresh brings the entry of container id up to date with its files.
func (v *networkView) refresh(id string) (*viewEntry, error) {
	e := v.entries[id]
	if e == nil {
		e = &viewEntry{}
	}
	var config containerRecord
	configKey, changed, err := readIfChanged(v.store.configPath(id), e.configKey, &config)
	if err != nil {
		return nil, err
	}
	if changed {
		e.config, e.configKey = config, configKey
	}
	var state containerState
	stateKey, changed, err := readIfChanged(v.store.statePath(id), e.stateKey, &state)
	if err != nil {
		return nil, err
	}
	if changed {
		e.state, e.stateKey = state, stateKey
	}
	v.entries[id] = e
	return e, nil
}

// readIfChanged decodes path into value when its key differs from old. A
// stat gives the key, so an unchanged file costs no open: an open and close
// costs about 12 us on macOS, a stat under 2 us. The read then takes the key
// again from its own descriptor, so a rename between the stat and the read
// cannot pair a key with other content.
func readIfChanged(path string, old fileKey, value any) (fileKey, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileKey{}, false, err
	}
	if key := keyOf(info); key == old && key.ino != 0 {
		return key, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fileKey{}, false, err
	}
	defer func() { _ = f.Close() }()
	info, err = f.Stat()
	if err != nil {
		return fileKey{}, false, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return fileKey{}, false, err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fileKey{}, false, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return keyOf(info), true, nil
}

// keyOf returns the key of a store file.
func keyOf(info os.FileInfo) fileKey {
	key := fileKey{size: info.Size(), mtime: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		key.ino = uint64(st.Ino)
	}
	return key
}

// cachedEntries returns the number of containers that the view holds.
func (v *networkView) cachedEntries() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.entries)
}
