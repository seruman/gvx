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
	"os"
	"strings"
	"testing"
	"time"
)

// TestRemovalDeletesOldExitRecords removes a container while the exit
// records of two earlier removals exist. A waiter reads an exit record within
// a second of the removal, so a record older than exitRecordTTL has no
// reader left: the removal deletes it and keeps the recent one.
func TestRemovalDeletesOldExitRecords(t *testing.T) {
	store := newContainerStore(t.TempDir())
	oldID, recentID, id := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	for _, cid := range []string{oldID, recentID, id} {
		if err := store.create(containerRecord{ID: cid, Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.removeWithExit(oldID, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.removeWithExit(recentID, 2); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * exitRecordTTL)
	if err := os.Chtimes(store.exitPath(oldID), past, past); err != nil {
		t.Fatal(err)
	}

	if err := store.removeWithExit(id, 3); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.removedExitCode(oldID); ok {
		t.Error("the exit record of an old removal stays")
	}
	if code, ok := store.removedExitCode(recentID); !ok || code != 2 {
		t.Errorf("recent exit record = %d, %v; want 2, true", code, ok)
	}
	if code, ok := store.removedExitCode(id); !ok || code != 3 {
		t.Errorf("new exit record = %d, %v; want 3, true", code, ok)
	}
}

// TestSweepExitRecordsAtStart deletes the old exit records that an engine
// left, as serve does when it starts.
func TestSweepExitRecordsAtStart(t *testing.T) {
	store := newContainerStore(t.TempDir())
	id := strings.Repeat("4", 64)
	if err := store.create(containerRecord{ID: id, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.removeWithExit(id, 0); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * exitRecordTTL)
	if err := os.Chtimes(store.exitPath(id), past, past); err != nil {
		t.Fatal(err)
	}
	store.sweepExitRecords(time.Now())
	if _, err := os.Stat(store.exitPath(id)); !os.IsNotExist(err) {
		t.Fatalf("old exit record after the sweep: %v", err)
	}
}
