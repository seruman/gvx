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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeShim serves one control request on a Unix socket: it writes the OK
// line and banner in one write, as a target whose guest speaks first
// (MySQL, SMTP, SSH) can do.
func fakeShim(t *testing.T, banner []byte) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gvx-shim-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil {
			return
		}
		_, _ = conn.Write(append([]byte("{\"ok\":true}\n"), banner...))
		if cw, ok := conn.(*net.UnixConn); ok {
			_ = cw.CloseWrite()
		}
		time.Sleep(time.Second)
	}()
	return sock
}

// TestDialPassesTheFirstBytesOfTheTarget proves that a dial to another
// container keeps the bytes that its guest sends before the caller writes.
// Those bytes can arrive in the read of the reply line; a buffered reader
// that the caller then drops loses them.
func TestDialPassesTheFirstBytesOfTheTarget(t *testing.T) {
	banner := bytes.Repeat([]byte("220 smtp ready "), 64<<10/15)
	sock := fakeShim(t, banner)
	conn, err := controlCallRaw(context.Background(), sock, controlRequest{Op: "dial", Port: 25})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, banner) {
		t.Fatalf("got %d bytes of the banner (starts %q), want all %d", len(got), strings.TrimSpace(string(got[:min(len(got), 20)])), len(banner))
	}
}

// TestSlowDialDoesNotBlockNameLookups proves that a dial to a target that
// does not answer does not hold up the name lookups of the same shim. Before
// the fix, every request waited for the one before it on the shared
// supervisor connection.
func TestSlowDialDoesNotBlockNameLookups(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gvx-net-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	store := newContainerStore(dir)
	app := containerRecord{ID: strings.Repeat("1", 64), Name: "app", Created: time.Now(), State: containerState{Running: true, Status: "running"},
		Networks: map[string]endpointSettings{"n": {NetworkID: "n1", IPAddress: "172.20.0.2", Subnet: "172.20.0.0/16"}}}
	db := containerRecord{ID: strings.Repeat("2", 64), Name: "db", Created: time.Now(), State: containerState{Running: true, Status: "running"},
		Networks: map[string]endpointSettings{"n": {NetworkID: "n1", IPAddress: "172.20.0.3", Subnet: "172.20.0.0/16"}}}
	for _, rec := range []containerRecord{app, db} {
		if err := store.create(rec); err != nil {
			t.Fatal(err)
		}
		lock, err := acquireSupervisorLock(store.dir(rec.ID))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(lock.release)
	}
	// The control socket of db accepts and never answers.
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", controlSocketPath(dir, db.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	supervisorEnd, shimEnd, err := socketPair()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go serveNetworkConn(ctx, supervisorEnd, dir, store, app.ID)
	shimConn, err := fileUnixConn(shimEnd)
	if err != nil {
		t.Fatal(err)
	}
	client := &netClient{conn: shimConn}
	lookup, dial := shimNetworkHooks(client, app, false)

	go func() { _, _, _ = dial(context.Background(), "tcp", "172.20.0.3:3306") }()
	time.Sleep(100 * time.Millisecond)
	finished := make(chan error, 1)
	go func() {
		for i := 0; i < 20; i++ {
			addrs, ok := lookup(context.Background(), "db")
			if !ok || len(addrs) != 1 || addrs[0].String() != "172.20.0.3" {
				finished <- fmt.Errorf("lookup %d of db = %v %v", i, addrs, ok)
				return
			}
		}
		finished <- nil
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("20 lookups did not finish in 1s behind a dial that does not answer")
	}
}

// TestControlCallsTimeOutOnASilentShim proves that a control call fails when
// the shim accepts and never answers. Without a deadline on the reply read,
// the dial session, its supervisor goroutine, and the connect of the guest
// wait until the target closes.
func TestControlCallsTimeOutOnASilentShim(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gvx-silent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	// A context deadline shorter than controlReplyTimeout bounds the reply
	// read, so the test does not wait 30 seconds.
	newCtx := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}

	for name, call := range map[string]func() error{
		"controlCall": func() error {
			c, _, err := controlCall(newCtx(), sock, controlRequest{Op: "stat"})
			if c != nil {
				_ = c.Close()
			}
			return err
		},
		"controlCallRaw": func() error {
			c, err := controlCallRaw(newCtx(), sock, controlRequest{Op: "dial"})
			if c != nil {
				_ = c.Close()
			}
			return err
		},
	} {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s to a silent shim succeeded", name)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s to a silent shim did not return in 3s", name)
		}
	}
}
