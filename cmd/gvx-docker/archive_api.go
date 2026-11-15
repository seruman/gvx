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
	"sync"
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
// of the container, and a function that releases it. A running container
// answers through its own shim. A stopped one boots a shared idle sandbox.
// A container in the wait between two runs of its restart policy has no shim,
// and an idle sandbox would take the root image of the next run, so the copy
// fails with a conflict, as Docker answers during a restart.
func (s *server) archiveControl(ctx context.Context, ref string) (string, func(), error) {
	rec, err := s.containers.get(ref)
	if err != nil {
		return "", nil, err
	}
	// The decision and the acquire run under the start lock of the
	// container, so a start cannot begin between them.
	unlock := s.idleShims.lockStart(rec.ID)
	defer unlock()
	rec, err = s.containers.get(rec.ID)
	if err != nil {
		return "", nil, err
	}
	switch {
	case rec.State.Restarting:
		return "", nil, fmt.Errorf("%w: container %s is restarting, wait until it is running", errConflict, rec.ID[:12])
	case rec.State.Running:
		return controlSocketPath(s.stateDir, rec.ID), func() {}, nil
	}
	return s.idleShims.acquire(ctx, s, rec)
}

// idleShims shares one idle sandbox of a stopped container among the copy,
// export, and commit requests that run at once. The sandbox holds the root
// upper image of the container with an exclusive lock, so a second sandbox
// would fail on it, and a start must wait until the sandbox process ends.
type idleShims struct {
	mu sync.Mutex
	// shims maps a container ID to its idle sandbox. An entry stays until
	// its process ended, also while it stops.
	shims map[string]*idleShim
	// starts maps a container ID to the lock that orders a start against the
	// "running or idle" decision of a copy.
	starts map[string]*sync.Mutex
}

// idleShim is the idle sandbox of one stopped container.
type idleShim struct {
	// ready closes when the sandbox serves its control socket or failed.
	ready chan struct{}
	sock  string
	err   error
	// users counts the requests that hold the sandbox. Guarded by
	// idleShims.mu.
	users int
	// stopping reports that the last user released the sandbox. A new
	// request then waits for done and boots a new sandbox. Guarded by
	// idleShims.mu.
	stopping bool
	stop     func()
	// done closes when the sandbox process ended.
	done chan struct{}
}

// lockStart takes the start lock of container id and returns its unlock.
func (m *idleShims) lockStart(id string) func() {
	m.mu.Lock()
	if m.starts == nil {
		m.starts = map[string]*sync.Mutex{}
	}
	l, ok := m.starts[id]
	if !ok {
		l = &sync.Mutex{}
		m.starts[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// acquire returns the control socket of the idle sandbox of rec, and starts
// the sandbox when none runs. A sandbox that stops is waited for first, so
// two sandboxes never hold the root image at once. The release function ends
// the sandbox after the last user.
func (m *idleShims) acquire(ctx context.Context, s *server, rec containerRecord) (string, func(), error) {
	var sh *idleShim
	for {
		m.mu.Lock()
		if m.shims == nil {
			m.shims = map[string]*idleShim{}
		}
		cur, ok := m.shims[rec.ID]
		if ok && cur.stopping {
			m.mu.Unlock()
			select {
			case <-cur.done:
				m.mu.Lock()
				if m.shims[rec.ID] == cur {
					delete(m.shims, rec.ID)
				}
				m.mu.Unlock()
				continue
			case <-ctx.Done():
				return "", nil, ctx.Err()
			}
		}
		if !ok {
			cur = &idleShim{ready: make(chan struct{}), done: make(chan struct{})}
			m.shims[rec.ID] = cur
			go func(sh *idleShim) {
				sh.sock, sh.stop, sh.err = s.startIdleShim(context.Background(), rec, sh.done)
				close(sh.ready)
			}(cur)
		}
		cur.users++
		sh = cur
		m.mu.Unlock()
		break
	}

	release := func() { m.release(rec.ID, sh) }
	select {
	case <-sh.ready:
	case <-ctx.Done():
		release()
		return "", nil, ctx.Err()
	}
	if sh.err != nil {
		release()
		return "", nil, sh.err
	}
	return sh.sock, release, nil
}

// release drops one user of sh. The last user stops the sandbox; the entry
// stays, marked stopping, until the process ended.
func (m *idleShims) release(id string, sh *idleShim) {
	m.mu.Lock()
	sh.users--
	last := sh.users == 0
	if last {
		sh.stopping = true
	}
	m.mu.Unlock()
	if !last {
		return
	}
	<-sh.ready
	if sh.stop != nil {
		sh.stop()
	}
	<-sh.done
	m.mu.Lock()
	if m.shims[id] == sh {
		delete(m.shims, id)
	}
	m.mu.Unlock()
}

// waitIdle returns after the idle sandbox of container id, if one runs or
// stops, ended, or when ctx ends. The end of the process frees the root
// image, which is what a start needs. The caller holds the start lock, so no
// new sandbox starts after the wait.
func (m *idleShims) waitIdle(ctx context.Context, id string) error {
	m.mu.Lock()
	sh, ok := m.shims[id]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-sh.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startIdleShim runs the container with the idle program until stop. done
// closes when the process ends.
func (s *server) startIdleShim(ctx context.Context, rec containerRecord, done chan struct{}) (string, func(), error) {
	sock := filepath.Join(s.stateDir, "run", rec.ID[:12]+".idle.sock")
	cmd := exec.Command(s.exe, "shim", "-idle", "-control", sock, "-state-dir", s.stateDir, rec.ID)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		close(done)
		return "", nil, fmt.Errorf("start copy sandbox: %w", err)
	}
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	stop := func() {
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
			return sock, stop, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			stop()
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
