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
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBuilderThatExitsFailsAtOnce starts a builder that exits without a
// socket. ensure must return an error soon, not after the 60 second socket
// wait, and a second ensure must start a new builder. Run it with -race: the
// exit status of the builder is written by the goroutine that waits for it
// and read by ensure.
func TestBuilderThatExitsFailsAtOnce(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "fake-gvx-docker")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := paths{stateDir: dir}
	p.normalize()
	b := &builderProcess{exe: exe, paths: p, log: io.Discard}
	for i := 0; i < 2; i++ {
		// The context outlasts any process start under load. ensure must end
		// because the builder exited, which its error names, and not because
		// the context or the 60 second socket wait ran out.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		err := b.ensure(ctx)
		cancel()
		if err == nil {
			t.Fatalf("ensure %d = nil for a builder that exited", i)
		}
		if !strings.Contains(err.Error(), "exited before it opened its socket") {
			t.Fatalf("ensure %d after %v = %v, want the error of a builder that exited", i, time.Since(start), err)
		}
	}
	b.stop()
}

// TestBuilderEnsureAndStopFromManyGoroutines runs ensure and stop at once,
// as parallel BuildKit connections do. Run it with -race. The old code read
// cmd.ProcessState while the goroutine that waits for the builder wrote it.
func TestBuilderEnsureAndStopFromManyGoroutines(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "fake-gvx-docker")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := paths{stateDir: dir}
	p.normalize()
	b := &builderProcess{exe: exe, paths: p, log: io.Discard}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
			defer cancel()
			_ = b.ensure(ctx)
			time.Sleep(50 * time.Millisecond)
			b.stop()
		}()
	}
	wg.Wait()
}
