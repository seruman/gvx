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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// appendLog appends one log line of container id, as the supervisor does.
func appendLog(t *testing.T, store *containerStore, id, text string) {
	t.Helper()
	f, err := os.OpenFile(store.logPath(id), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	line, _ := json.Marshal(logEntry{Log: text, Stream: "stdout", Time: time.Now().UTC()})
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// TestLogFollowerKeepsTheLastLinesOfAnExit runs a follower of a running
// container. Between the end-of-file read of the follower and its status
// check, the container writes its last line and exits. The follower must
// print that line before it returns.
func TestLogFollowerKeepsTheLastLinesOfAnExit(t *testing.T) {
	dir := t.TempDir()
	store := newContainerStore(dir)
	id := strings.Repeat("7", 64)
	rec := containerRecord{ID: id, Created: time.Now(), State: containerState{Running: true, Status: "running"}}
	if err := store.create(rec); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireSupervisorLock(store.dir(id))
	if err != nil {
		t.Fatal(err)
	}
	appendLog(t, store, id, "first\n")
	s := &server{containers: store, stateDir: dir}
	fired := false
	s.logEOFHook = func() {
		if fired {
			return
		}
		fired = true
		appendLog(t, store, id, "last words\n")
		_ = store.updateState(id, func(st *containerState) { st.Running = false; st.Status = "exited" })
		lock.release()
	}
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		s.streamLogs(context.Background(), &out, rec, logOptions{stdout: true, follow: true, raw: true, tail: -1})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the follower did not end after the exit")
	}
	if got := out.String(); got != "first\nlast words\n" {
		t.Fatalf("follower printed %q, want both lines", got)
	}
}

// exitedContainer creates an exited container with exit code 5 that ended a
// minute ago, as before docker start -a, and holds no supervisor lock.
func exitedContainer(t *testing.T, s *server, id string) {
	t.Helper()
	old := time.Now().UTC().Add(-time.Minute)
	rec := containerRecord{ID: id, Created: old, State: containerState{Status: "exited", ExitCode: 5, StartedAt: old, FinishedAt: old}}
	if err := s.containers.create(rec); err != nil {
		t.Fatal(err)
	}
}

// runAgain records a new run of container id that prints line and exits with
// code 7, as start and the supervisor do.
func runAgain(t *testing.T, s *server, id, line string) {
	t.Helper()
	_ = s.containers.updateState(id, func(st *containerState) {
		st.Running, st.Status, st.StartedAt, st.FinishedAt = true, "running", time.Now().UTC(), time.Time{}
		st.StartPending = time.Now().UTC()
	})
	time.Sleep(150 * time.Millisecond)
	appendLog(t, s.containers, id, line)
	_ = s.containers.updateState(id, func(st *containerState) {
		st.Running, st.Status, st.ExitCode, st.FinishedAt = false, "exited", 7, time.Now().UTC()
		st.StartPending = time.Time{}
	})
}

// TestWaitNextExitWaitsForANewRun asks for the next exit of a container that
// exited before, as docker start -a does before it starts the container. The
// wait must return the code of the new run, not the old one.
func TestWaitNextExitWaitsForANewRun(t *testing.T) {
	dir := t.TempDir()
	s := &server{containers: newContainerStore(dir), networks: newNetworkStore(dir), stateDir: dir}
	id := strings.Repeat("6", 64)
	exitedContainer(t, s, id)
	srv := httptest.NewServer(s)
	defer srv.Close()
	result := make(chan string, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/containers/"+id+"/wait?condition=next-exit", "application/json", nil)
		if err != nil {
			result <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		result <- strings.TrimSpace(string(b))
	}()
	select {
	case got := <-result:
		t.Fatalf("wait for the next exit returned %s before a new run", got)
	case <-time.After(300 * time.Millisecond):
	}
	runAgain(t, s, id, "new run\n")
	select {
	case got := <-result:
		if got != `{"StatusCode":7}` {
			t.Fatalf("wait = %s, want the exit code 7 of the new run", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return after the new run exited")
	}
}

// TestAttachToAnExitedContainerFollowsTheNextRun attaches to a container
// that exited before, as docker start -a does before it starts it. The stream
// must carry the output of the new run and end with it.
func TestAttachToAnExitedContainerFollowsTheNextRun(t *testing.T) {
	dir := t.TempDir()
	s := &server{containers: newContainerStore(dir), networks: newNetworkStore(dir), stateDir: dir}
	id := strings.Repeat("5", 64)
	exitedContainer(t, s, id)
	srv := httptest.NewServer(s)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /containers/%s/attach?stream=1&stdout=1&stderr=1 HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", id)
	br := bufio.NewReader(conn)
	if status, _ := br.ReadString('\n'); !strings.Contains(status, "101") {
		t.Fatalf("attach answered %q", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	got := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(br)
		got <- string(b)
	}()
	select {
	case out := <-got:
		t.Fatalf("attach ended before the new run with %q", out)
	case <-time.After(300 * time.Millisecond):
	}
	runAgain(t, s, id, "new run\n")
	select {
	case out := <-got:
		if !strings.Contains(out, "new run") {
			t.Fatalf("attach stream = %q, want the output of the new run", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach did not end after the new run exited")
	}
}
