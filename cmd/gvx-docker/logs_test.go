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
	"encoding/json"
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
