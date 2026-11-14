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
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStateUpdatesFromTwoStoresKeepBothFields runs the update pattern of the
// API process and the supervisor at once: two stores on one directory, as two
// processes have. Each writer changes its own field with a read-modify-write.
// Every change must survive.
func TestStateUpdatesFromTwoStoresKeepBothFields(t *testing.T) {
	dir := t.TempDir()
	api, supervisor := newContainerStore(dir), newContainerStore(dir)
	id := strings.Repeat("a", 64)
	if err := api.create(containerRecord{ID: id, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	const rounds = 300
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 1; i <= rounds; i++ {
			_ = api.updateState(id, func(st *containerState) { st.ExitCode = i })
		}
	}()
	go func() {
		defer wg.Done()
		for i := 1; i <= rounds; i++ {
			_ = supervisor.updateState(id, func(st *containerState) { st.Pid = i })
		}
	}()
	wg.Wait()
	rec, err := api.get(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State.ExitCode != rounds || rec.State.Pid != rounds {
		t.Fatalf("state = exit %d pid %d, want both %d: one writer lost the update of the other", rec.State.ExitCode, rec.State.Pid, rounds)
	}
}
