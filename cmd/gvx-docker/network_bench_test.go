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
	"testing"
	"time"
)

// BenchmarkAnswerResolve measures one name lookup that a shim sends to its
// supervisor, with containers on the host as in a Compose project.
func BenchmarkAnswerResolve(b *testing.B) {
	for _, n := range []int{2, 10, 30} {
		b.Run(fmt.Sprintf("containers=%d", n), func(b *testing.B) {
			dir, err := os.MkdirTemp("/tmp", "gvx-net-")
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = os.RemoveAll(dir) })
			store := newContainerStore(dir)
			var self containerRecord
			for i := 0; i < n; i++ {
				rec := containerRecord{ID: fmt.Sprintf("%064x", i+1), Name: fmt.Sprintf("svc%d", i), Created: time.Now(),
					State:    containerState{Running: true, Status: "running"},
					Networks: map[string]endpointSettings{"n": {NetworkID: "n1", IPAddress: fmt.Sprintf("172.20.0.%d", i+2), Subnet: "172.20.0.0/16"}}}
				if err := store.create(rec); err != nil {
					b.Fatal(err)
				}
				lock, err := acquireSupervisorLock(store.dir(rec.ID))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(lock.release)
				if i == 0 {
					self = rec
				}
			}
			req := netRequest{Op: "resolve", Name: fmt.Sprintf("svc%d", n-1)}
			b.ResetTimer()
			for range b.N {
				resp, _ := answerNetwork(context.Background(), dir, store, self.ID, req)
				if !resp.Found {
					b.Fatal("not found")
				}
			}
		})
	}
}
