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
