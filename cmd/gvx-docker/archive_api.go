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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"z.gfire.dev/gvx/pkg/gvx"
)

// The archive endpoints read and write the files of the container through
// the Sentry VFS, so they work without tar in the image. A container that does
// not run gets a short sandbox that runs the idle program, so the copy sees
// its root file system and its volumes, as in Docker.

func (s *server) routeArchive(w http.ResponseWriter, r *http.Request, id, action string) bool {
	if action != "archive" {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.getArchive(w, r, id)
	case http.MethodPut:
		s.putArchive(w, r, id)
	default:
		return false
	}
	return true
}

type dockerPathStat struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	Mtime      time.Time `json:"mtime"`
	LinkTarget string    `json:"linkTarget"`
}

// goFileMode converts a Linux st_mode to the bits of Go's fs.FileMode, which
// the Docker client decodes.
func goFileMode(m uint32) uint32 {
	const (
		modeDir     = 1 << 31
		modeSymlink = 1 << 27
		modeDevice  = 1 << 26
		modeNamed   = 1 << 25
		modeSocket  = 1 << 24
		modeSetuid  = 1 << 23
		modeSetgid  = 1 << 22
		modeChar    = 1 << 21
		modeSticky  = 1 << 20
	)
	out := m & 0o777
	switch m & 0o170000 {
	case 0o040000:
		out |= modeDir
	case 0o120000:
		out |= modeSymlink
	case 0o010000:
		out |= modeNamed
	case 0o140000:
		out |= modeSocket
	case 0o060000:
		out |= modeDevice
	case 0o020000:
		out |= modeDevice | modeChar
	}
	if m&0o4000 != 0 {
		out |= modeSetuid
	}
	if m&0o2000 != 0 {
		out |= modeSetgid
	}
	if m&0o1000 != 0 {
		out |= modeSticky
	}
	return out
}

func setPathStatHeader(w http.ResponseWriter, st gvx.PathStat) {
	data, _ := json.Marshal(dockerPathStat{Name: st.Name, Size: st.Size, Mode: goFileMode(st.Mode), Mtime: st.ModTime, LinkTarget: st.LinkTarget})
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(data))
}

// archiveControl returns the control socket of a sandbox that shows the files
// of the container, and a function that releases it.
func (s *server) archiveControl(ctx context.Context, ref string) (string, func(), error) {
	rec, err := s.containers.get(ref)
	if err != nil {
		return "", nil, err
	}
	if rec.State.Running {
		return controlSocketPath(s.stateDir, rec.ID), func() {}, nil
	}
	return s.startIdleShim(ctx, rec)
}

// startIdleShim runs the container with the idle program until release.
func (s *server) startIdleShim(ctx context.Context, rec containerRecord) (string, func(), error) {
	sock := filepath.Join(s.stateDir, "run", rec.ID[:12]+".idle.sock")
	cmd := exec.Command(s.exe, "shim", "-idle", "-control", sock, "-state-dir", s.stateDir, rec.ID)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start copy sandbox: %w", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	release := func() {
		// SIGTERM ends the idle program, and the shim closes the root upper
		// image cleanly, so the copy reaches the disk.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = os.Remove(sock)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-done:
			_ = os.Remove(sock)
			return "", nil, errors.New("copy sandbox exited before it was ready")
		default:
		}
		if _, err := requestStat(ctx, sock, "/"); err == nil {
			return sock, release, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			release()
			return "", nil, errors.New("copy sandbox did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requestStat asks a shim for the metadata of a guest path.
func requestStat(ctx context.Context, sock, p string) (gvx.PathStat, error) {
	conn, err := dialControl(ctx, sock)
	if err != nil {
		return gvx.PathStat{}, err
	}
	defer func() { _ = conn.Close() }()
	data, _ := json.Marshal(controlRequest{Op: "stat", Path: p})
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return gvx.PathStat{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return gvx.PathStat{}, err
	}
	var resp controlResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return gvx.PathStat{}, err
	}
	if resp.NotFound {
		return gvx.PathStat{}, fmt.Errorf("%w: Could not find the file %s in container", errNotFound, p)
	}
	if !resp.OK || resp.Stat == nil {
		return gvx.PathStat{}, errors.New(resp.Error)
	}
	return *resp.Stat, nil
}

func (s *server) getArchive(w http.ResponseWriter, r *http.Request, ref string) {
	sock, release, err := s.archiveControl(r.Context(), ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer release()
	p := r.URL.Query().Get("path")
	st, err := requestStat(r.Context(), sock, p)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setPathStatHeader(w, st)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	conn, reader, err := controlCall(r.Context(), sock, controlRequest{Op: "archive-get", Path: p})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

func (s *server) putArchive(w http.ResponseWriter, r *http.Request, ref string) {
	sock, release, err := s.archiveControl(r.Context(), ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer release()
	p := r.URL.Query().Get("path")
	st, err := requestStat(r.Context(), sock, p)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if st.Mode&0o170000 != 0o040000 {
		writeError(w, http.StatusBadRequest, "extraction point is not a directory")
		return
	}
	conn, reader, err := controlCall(r.Context(), sock, controlRequest{Op: "archive-put", Path: p})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.Copy(conn, r.Body); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	closeWrite(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		writeError(w, http.StatusInternalServerError, "extract archive: "+err.Error())
		return
	}
	var resp controlResponse
	if err := json.Unmarshal(line, &resp); err != nil || !resp.OK {
		writeError(w, http.StatusInternalServerError, "extract archive: "+resp.Error)
		return
	}
	w.WriteHeader(http.StatusOK)
}
