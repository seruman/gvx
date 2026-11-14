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
	"fmt"
	"net"
	"os"
	"syscall"
)

func unixRights(f *os.File) []byte { return syscall.UnixRights(int(f.Fd())) }

func parseRights(oob []byte) *os.File {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	for _, m := range msgs {
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		for i, fd := range fds {
			if i == 0 {
				return os.NewFile(uintptr(fd), "gvx-docker-dial")
			}
			_ = syscall.Close(fd)
		}
	}
	return nil
}

// socketPair returns the two ends of a connected Unix stream socket pair. The
// second end is a file for a child process.
func socketPair() (*net.UnixConn, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("create socket pair: %w", err)
	}
	syscall.CloseOnExec(fds[0])
	parent := os.NewFile(uintptr(fds[0]), "gvx-docker-net")
	child := os.NewFile(uintptr(fds[1]), "gvx-docker-net-child")
	conn, err := net.FileConn(parent)
	_ = parent.Close()
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	return conn.(*net.UnixConn), child, nil
}

func fileUnixConn(f *os.File) (*net.UnixConn, error) {
	conn, err := net.FileConn(f)
	if err != nil {
		return nil, err
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("inherited network connection is not a Unix socket")
	}
	return uc, nil
}

func lockFile(f *os.File) error   { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }
func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
