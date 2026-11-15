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
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// cacheFixture is a state directory with containers on one network. The
// writer store stands for another process: the API process or another
// supervisor. The network view reads through its own store, as a supervisor
// does.
type cacheFixture struct {
	dir    string
	writer *containerStore
	view   *networkView
	locks  map[string]supervisorLock
}

func newCacheFixture(t testing.TB, names ...string) *cacheFixture {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gvx-net-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &cacheFixture{dir: dir, writer: newContainerStore(dir), view: newNetworkView(newContainerStore(dir)), locks: map[string]supervisorLock{}}
	for i, name := range names {
		f.add(t, fmt.Sprintf("%064x", i+1), name, fmt.Sprintf("172.20.0.%d", i+2))
	}
	return f
}

func (f *cacheFixture) add(t testing.TB, id, name, ip string) {
	t.Helper()
	rec := containerRecord{ID: id, Name: name, Created: time.Now(), State: containerState{Running: true, Status: "running"},
		Networks: map[string]endpointSettings{"n": {NetworkID: "n1", IPAddress: ip, Subnet: "172.20.0.0/16"}}}
	if err := f.writer.create(rec); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireSupervisorLock(f.writer.dir(id))
	if err != nil {
		t.Fatal(err)
	}
	f.locks[id] = lock
	t.Cleanup(lock.release)
}

func (f *cacheFixture) resolve(t testing.TB, selfID, name string) []string {
	t.Helper()
	resp, _ := answerNetworkIn(context.Background(), f.dir, f.view, selfID, netRequest{Op: "resolve", Name: name})
	return resp.Addrs
}

var selfID = fmt.Sprintf("%064x", 1)

// TestNetworkViewSeesChangesOfOtherProcesses changes containers through a
// second store, as the API process does, after the view has cached them.
// Each change must show in the next lookup.
func TestNetworkViewSeesChangesOfOtherProcesses(t *testing.T) {
	f := newCacheFixture(t, "app", "db")
	if got := f.resolve(t, selfID, "db"); len(got) != 1 || got[0] != "172.20.0.3" {
		t.Fatalf("db = %v, want [172.20.0.3]", got)
	}

	// A new container.
	f.add(t, fmt.Sprintf("%064x", 9), "cache", "172.20.0.9")
	if got := f.resolve(t, selfID, "cache"); len(got) != 1 || got[0] != "172.20.0.9" {
		t.Fatalf("new container cache = %v, want [172.20.0.9]", got)
	}

	// A rename.
	dbID := fmt.Sprintf("%064x", 2)
	if _, err := f.writer.renameLocked(dbID, "postgres"); err != nil {
		t.Fatal(err)
	}
	if got := f.resolve(t, selfID, "postgres"); len(got) != 1 {
		t.Fatalf("renamed db = %v, want one address", got)
	}
	if got := f.resolve(t, selfID, "db"); len(got) != 0 {
		t.Fatalf("old name db = %v, want none", got)
	}

	// A new network address.
	if _, err := f.writer.update(dbID, func(r *containerRecord) error {
		ep := r.Networks["n"]
		ep.IPAddress = "172.20.0.30"
		r.Networks["n"] = ep
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.resolve(t, selfID, "postgres"); len(got) != 1 || got[0] != "172.20.0.30" {
		t.Fatalf("db after a network change = %v, want [172.20.0.30]", got)
	}

	// A stop.
	if err := f.writer.updateState(dbID, func(st *containerState) { st.Running, st.Status = false, "exited" }); err != nil {
		t.Fatal(err)
	}
	if got := f.resolve(t, selfID, "postgres"); len(got) != 0 {
		t.Fatalf("stopped db = %v, want none", got)
	}
}

// TestNetworkViewSeesADeadSupervisorAtOnce releases the supervisor lock of a
// container whose state still reads running, as a SIGKILL of the supervisor
// does. The next lookup must not return it.
func TestNetworkViewSeesADeadSupervisorAtOnce(t *testing.T) {
	f := newCacheFixture(t, "app", "db")
	dbID := fmt.Sprintf("%064x", 2)
	if got := f.resolve(t, selfID, "db"); len(got) != 1 {
		t.Fatalf("db = %v, want one address", got)
	}
	f.locks[dbID].release()
	if got := f.resolve(t, selfID, "db"); len(got) != 0 {
		t.Fatalf("db after its supervisor died = %v, want none", got)
	}
}

// TestNetworkViewDropsRemovedContainers removes a container and checks that
// lookups miss it and that the view keeps no entry for it.
func TestNetworkViewDropsRemovedContainers(t *testing.T) {
	f := newCacheFixture(t, "app", "db")
	dbID := fmt.Sprintf("%064x", 2)
	if got := f.resolve(t, selfID, "db"); len(got) != 1 {
		t.Fatalf("db = %v, want one address", got)
	}
	if err := f.writer.remove(dbID); err != nil {
		t.Fatal(err)
	}
	if got := f.resolve(t, selfID, "db"); len(got) != 0 {
		t.Fatalf("removed db = %v, want none", got)
	}
	if n := f.view.cachedEntries(); n != 1 {
		t.Fatalf("view holds %d entries after the removal, want 1", n)
	}
}

// TestNetworkViewServesConcurrentLookups runs lookups from many goroutines
// while another store changes a record, as parallel dial sessions do. Run it
// with -race.
func TestNetworkViewServesConcurrentLookups(t *testing.T) {
	f := newCacheFixture(t, "app", "db", "cache")
	dbID := fmt.Sprintf("%064x", 2)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, name := range []string{"db", "cache", "app"} {
					if got := f.resolve(t, selfID, name); len(got) > 1 {
						t.Errorf("%s = %v", name, got)
					}
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		ip := fmt.Sprintf("172.20.1.%d", i)
		if _, err := f.writer.update(dbID, func(r *containerRecord) error {
			ep := r.Networks["n"]
			ep.IPAddress = ip
			r.Networks["n"] = ep
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if got := f.resolve(t, selfID, "db"); len(got) != 1 || got[0] != "172.20.1.49" {
		t.Fatalf("db = %v, want the last address", got)
	}
	if strings.Count(fmt.Sprint(f.resolve(t, selfID, "app")), ".") != 3 {
		t.Fatal("app lost its address")
	}
}
