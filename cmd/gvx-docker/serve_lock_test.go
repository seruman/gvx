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
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSecondServeRefusesAStateDirectoryInUse starts serve twice on one state
// directory. The second one must fail and leave the socket of the first one
// in place. Before the fix, it removed that socket and bound its own.
func TestSecondServeRefusesAStateDirectoryInUse(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir, err := os.MkdirTemp("/tmp", "gvx-serve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "docker.sock")
	args := []string{"-state-dir", dir, "-socket", sock}

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan int, 1)
	go func() { firstDone <- serveCommand(ctx, args, &bytes.Buffer{}) }()
	t.Cleanup(func() { cancel(); <-firstDone })
	deadline := time.Now().Add(10 * time.Second)
	for !socketReady(sock) {
		if time.Now().After(deadline) {
			t.Fatal("the first serve did not open its socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
	first, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer secondCancel()
	if code := serveCommand(secondCtx, args, &stderr); code == 0 {
		t.Fatalf("the second serve returned 0, want a failure; stderr %q", stderr.String())
	}
	now, err := os.Stat(sock)
	if err != nil || !os.SameFile(first, now) {
		t.Fatalf("the socket of the first serve changed: %v", err)
	}
	client := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("the first serve does not answer: %v", err)
	}
	_ = resp.Body.Close()
}
