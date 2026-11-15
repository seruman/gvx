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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"z.gfire.dev/gvx/pkg/gvx"
)

// runFakeIdleShim acts as "gvx-docker shim -idle -control SOCK -state-dir DIR
// ID". Like the real idle shim, it holds the root upper image of the
// container with an exclusive lock, which ext4lite takes, and fails when
// another shim holds it. It answers stat and archive-get, and exits on
// SIGTERM.
func runFakeIdleShim() {
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	var sock, stateDir, id string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-control":
			i++
			sock = args[i]
		case "-state-dir":
			i++
			stateDir = args[i]
		case "shim", "-idle":
		default:
			id = args[i]
		}
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, "containers", id, "rootfs.ext4"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Exit(3)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		os.Exit(4)
	}
	if err := removeStaleSocket(sock); err != nil {
		os.Exit(5)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(6)
	}
	if f, err := os.OpenFile(filepath.Join(stateDir, "idle-starts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.WriteString("start\n")
		_ = f.Close()
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	go func() {
		<-term
		// The real shim closes the root image after SIGTERM, which takes
		// a moment. It holds the image lock until it exits.
		time.Sleep(400 * time.Millisecond)
		os.Exit(0)
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func(c net.Conn) {
			defer c.Close()
			line, err := bufio.NewReader(c).ReadBytes('\n')
			if err != nil {
				return
			}
			var req controlRequest
			_ = json.Unmarshal(line, &req)
			// A copy takes a moment, so parallel requests overlap.
			time.Sleep(50 * time.Millisecond)
			resp := controlResponse{OK: true}
			if req.Op == "stat" {
				resp.Stat = &gvx.PathStat{Name: "/", Mode: 0o040755}
			}
			data, _ := json.Marshal(resp)
			_, _ = c.Write(append(data, '\n'))
		}(c)
	}
}

// fakeIdleShimServer returns a server whose shim program is the fake idle
// shim, with one stopped container.
func fakeIdleShimServer(t *testing.T, id string) *server {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gvx-idle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	state := filepath.Join(dir, "state")
	exe := filepath.Join(dir, "gvx-docker")
	script := fmt.Sprintf("#!/bin/sh\nexec env GVX_DOCKER_TEST_HELPER=idle-shim %q -test.run='^TestHelperProcess$' -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &server{containers: newContainerStore(state), networks: newNetworkStore(state), stateDir: state, exe: exe}
	if err := s.containers.create(containerRecord{ID: id, Created: time.Now(), State: containerState{Status: "exited"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestParallelCopiesShareOneIdleShim runs copies out of one stopped
// container at once, as parallel docker cp calls or a Compose project do.
// Each copy boots an idle sandbox on the root image of the container. A second
// sandbox fails on the lock of that image and removes the socket of the first,
// so every copy must share one sandbox.
func TestParallelCopiesShareOneIdleShim(t *testing.T) {
	id := strings.Repeat("3", 64)
	s := fakeIdleShimServer(t, id)
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/containers/"+id+"/archive?path=/", nil))
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("copy %d = %d, want 200", i, code)
		}
	}
}

// recordingRuntime records when start ran.
type recordingRuntime struct {
	mu      sync.Mutex
	started time.Time
}

func (r *recordingRuntime) start(context.Context, containerRecord) error {
	r.mu.Lock()
	r.started = time.Now()
	r.mu.Unlock()
	return nil
}

// TestStartWaitsForACopyOfAStoppedContainer starts a container while a copy
// holds its idle sandbox. The run needs the root image that the idle sandbox
// holds, so start must wait until the copy ends.
func TestStartWaitsForACopyOfAStoppedContainer(t *testing.T) {
	id := strings.Repeat("2", 64)
	s := fakeIdleShimServer(t, id)
	rt := &recordingRuntime{}
	s.runtime = rt
	_, release, err := s.archiveControl(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	releasedAt := make(chan time.Time, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		releasedAt <- time.Now()
		release()
	}()
	req := httptest.NewRequest(http.MethodPost, "/containers/"+id+"/start", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	s.startContainer(httptest.NewRecorder(), req.WithContext(ctx), id)
	rt.mu.Lock()
	started := rt.started
	rt.mu.Unlock()
	if started.IsZero() {
		t.Fatal("start did not run")
	}
	var released time.Time
	select {
	case released = <-releasedAt:
	default:
	}
	if released.IsZero() || started.Before(released) {
		t.Fatal("start ran while a copy held the idle sandbox of the container")
	}
}

func idleStarts(t *testing.T, s *server) int {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(s.stateDir, "idle-starts"))
	return strings.Count(string(data), "start")
}

// TestCopyAndStartAfterACopyEndsWaitForTheOldSandbox ends a copy and, while
// its idle sandbox stops, sends a second copy and a start. The old sandbox
// holds the root image until its process exits. The second copy must not
// fail on the image lock, and the start must not run before the old process
// ends.
func TestCopyAndStartAfterACopyEndsWaitForTheOldSandbox(t *testing.T) {
	id := strings.Repeat("1", 64)
	s := fakeIdleShimServer(t, id)
	rt := &recordingRuntime{}
	s.runtime = rt
	_, release, err := s.archiveControl(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	releasedAt := time.Now()
	go release()
	time.Sleep(50 * time.Millisecond)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/containers/"+id+"/archive?path=/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("a copy while the old sandbox stops = %d %s, want 200", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/containers/"+id+"/start", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	s.startContainer(httptest.NewRecorder(), req.WithContext(ctx), id)
	rt.mu.Lock()
	started := rt.started
	rt.mu.Unlock()
	// The stop of the second sandbox takes 400 ms after its copy ended.
	if started.IsZero() || started.Sub(releasedAt) < 400*time.Millisecond {
		t.Errorf("start ran %v after the first copy ended, before an idle sandbox exited", started.Sub(releasedAt))
	}
}

// slowStartRuntime marks the container running after a delay, as start does
// between its wait for the idle sandbox and its state write.
type slowStartRuntime struct{ s *server }

func (r slowStartRuntime) start(_ context.Context, rec containerRecord) error {
	time.Sleep(300 * time.Millisecond)
	return r.s.containers.updateState(rec.ID, func(st *containerState) {
		st.Running, st.Status, st.StartPending = true, "running", time.Now().UTC()
	})
}

// TestCopyDuringAStartUsesTheRunNotAnIdleSandbox sends a copy while a start
// of the stopped container runs. The copy must not boot an idle sandbox on
// the root image that the new run needs.
func TestCopyDuringAStartUsesTheRunNotAnIdleSandbox(t *testing.T) {
	id := strings.Repeat("0", 64)
	s := fakeIdleShimServer(t, id)
	s.runtime = slowStartRuntime{s: s}
	startDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/containers/"+id+"/start", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		s.startContainer(httptest.NewRecorder(), req.WithContext(ctx), id)
		close(startDone)
	}()
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, release, err := s.archiveControl(ctx, id); err == nil {
		release()
	}
	<-startDone
	if n := idleStarts(t, s); n != 0 {
		t.Fatalf("a copy during a start booted %d idle sandboxes on the root image of the run", n)
	}
}

// TestCopyIntoARestartingContainerConflicts sends a copy to a container in
// the wait between two runs of its restart policy. No shim serves its files
// then, and an idle sandbox would hold the root image that the next run
// needs, so the copy fails with 409.
func TestCopyIntoARestartingContainerConflicts(t *testing.T) {
	id := strings.Repeat("e", 64)
	s := fakeIdleShimServer(t, id)
	lock, err := acquireSupervisorLock(s.containers.dir(id))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	_ = s.containers.updateState(id, func(st *containerState) { st.Restarting, st.Status = true, "restarting" })
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/containers/"+id+"/archive?path=/", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("copy into a restarting container = %d, want 409", rec.Code)
	}
	if n := idleStarts(t, s); n != 0 {
		t.Fatalf("a copy into a restarting container booted %d idle sandboxes", n)
	}
}

// countingRuntime counts the runs that start begins, and marks the container
// running, as the real start does.
type countingRuntime struct {
	s      *server
	mu     sync.Mutex
	starts int
}

func (r *countingRuntime) start(_ context.Context, rec containerRecord) error {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	return r.s.containers.updateState(rec.ID, func(st *containerState) {
		st.Running, st.Status, st.StartPending = true, "running", time.Now().UTC()
	})
}

// TestConcurrentStartsRunTheContainerOnce starts an exited container twice
// at once. The second start must see the run of the first one and answer
// 304, not start a second supervisor.
func TestConcurrentStartsRunTheContainerOnce(t *testing.T) {
	id := strings.Repeat("d", 64)
	s := fakeIdleShimServer(t, id)
	rt := &countingRuntime{s: s}
	s.runtime = rt
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/containers/"+id+"/start", nil)
			ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
			defer cancel()
			rec := httptest.NewRecorder()
			s.startContainer(rec, req.WithContext(ctx), id)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	if rt.starts != 1 {
		t.Fatalf("two concurrent starts ran the container %d times (codes %v), want once", rt.starts, codes)
	}
}
