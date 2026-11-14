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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStopOfRestartingContainerSignalsNoProcessGroup stops a container whose
// record has Pid 0, as during a restart backoff, with t=0 as docker rm -f
// sends it. kill(0, sig) signals the whole process group of the caller, so
// the test process must receive no signal.
func TestStopOfRestartingContainerSignalsNoProcessGroup(t *testing.T) {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGTERM)
	defer signal.Stop(sigs)

	dir := t.TempDir()
	store := newContainerStore(dir)
	id := strings.Repeat("c", 64)
	rec := containerRecord{ID: id, Created: time.Now(), State: containerState{Restarting: true, Status: "restarting", Pid: 0, SupervisorPid: 0}}
	if err := store.create(rec); err != nil {
		t.Fatal(err)
	}
	// A live supervisor holds the lock, so the record stays "restarting".
	holder := startLockHolder(t, store.dir(id))
	holderExited := make(chan struct{})
	go func() { _ = holder.Wait(); close(holderExited) }()

	srv := newServer(nil, noBuilder{}, store, nil, dir)
	srv.stopGrace = 100 * time.Millisecond
	req := httptest.NewRequest(http.MethodPost, "/containers/"+id+"/stop?t=0", nil)
	srv.stopContainer(httptest.NewRecorder(), req, id, false)
	_ = holder.Process.Kill()
	<-holderExited

	select {
	case sig := <-sigs:
		t.Fatalf("stop sent %v to the process group of the engine", sig)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStopSignalsOnlyTheLockHolder proves that stop signals no PID that the
// record names unless the supervisor lock names the same PID. After a crash
// or a reboot, a stale record can name an unrelated live process.
func TestStopSignalsOnlyTheLockHolder(t *testing.T) {
	victim := helperCommand(t, "sleep")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	victimExited := make(chan error, 1)
	go func() { victimExited <- victim.Wait() }()
	defer func() { _ = victim.Process.Kill() }()

	dir := t.TempDir()
	store := newContainerStore(dir)
	id := strings.Repeat("d", 64)
	stale := containerState{Running: true, Status: "running", Pid: victim.Process.Pid, SupervisorPid: victim.Process.Pid}
	if err := store.create(containerRecord{ID: id, Created: time.Now(), State: stale}); err != nil {
		t.Fatal(err)
	}
	// A supervisor with a different PID holds the lock.
	startLockHolder(t, store.dir(id))

	srv := newServer(nil, noBuilder{}, store, nil, dir)
	srv.stopGrace = 100 * time.Millisecond
	req := httptest.NewRequest(http.MethodPost, "/containers/"+id+"/stop?t=0", nil)
	srv.stopContainer(httptest.NewRecorder(), req, id, false)

	select {
	case err := <-victimExited:
		t.Fatalf("stop signalled the unrelated process %d that a stale record named: %v", victim.Process.Pid, err)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestShimStopperEscalates proves the order of a stop: the stop signal of the
// container, then SIGUSR2 after the timeout, which makes the shim kill its
// guest. /bin/sleep stands for a shim: the default action of SIGUSR2 ends
// it, and the stop signal goes to the guest, not to the process.
func TestShimStopperEscalates(t *testing.T) {
	shim := exec.Command("/bin/sleep", "60")
	if err := shim.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- shim.Wait() }()
	defer func() { _ = shim.Process.Kill() }()

	var guestSignals []int
	st := shimStopper{shim: shim.Process, signal: 10, timeout: 50 * time.Millisecond, signalGuest: func(n int) error {
		guestSignals = append(guestSignals, n)
		return nil
	}}
	timer := st.begin()
	if st.begin() != nil || len(guestSignals) != 1 || guestSignals[0] != 10 {
		t.Fatalf("guest signals = %v after two begin calls, want one signal 10", guestSignals)
	}
	select {
	case err := <-exited:
		t.Fatalf("the shim ended before the timeout: %v", err)
	case <-timer:
	}
	st.next()
	select {
	case err := <-exited:
		if status, ok := shim.ProcessState.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGUSR2 {
			t.Fatalf("shim ended with %v, want SIGUSR2", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the shim got no SIGUSR2 after the timeout")
	}
}

// TestShimSeesSupervisorDeath proves that the shim side of the supervisor
// pipe reports the death of a supervisor that the kernel killed, as SIGKILL
// does. The pipe is the darwin path; Linux adds a parent death signal.
func TestShimSeesSupervisorDeath(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	supervisor := exec.Command("/bin/sleep", "60")
	supervisor.ExtraFiles = []*os.File{w}
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	dead := make(chan struct{})
	go watchSupervisor(r, func() { close(dead) })
	select {
	case <-dead:
		t.Fatal("watchSupervisor reported death while the supervisor runs")
	case <-time.After(100 * time.Millisecond):
	}
	_ = supervisor.Process.Kill()
	_ = supervisor.Wait()
	select {
	case <-dead:
	case <-time.After(5 * time.Second):
		t.Fatal("watchSupervisor did not report the death of the supervisor")
	}
}

// TestStopDuringRestartBackoffRecordsExit proves that a stop that arrives in
// the wait between two runs leaves the container exited. A supervisor that
// returned with the record at "restarting" left a container that inspect
// showed as restarting forever.
func TestStopDuringRestartBackoffRecordsExit(t *testing.T) {
	dir := t.TempDir()
	store := newContainerStore(dir)
	id := strings.Repeat("e", 64)
	rec := containerRecord{ID: id, Created: time.Now(), State: containerState{Restarting: true, Status: "restarting", ExitCode: 1}}
	if err := store.create(rec); err != nil {
		t.Fatal(err)
	}
	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGTERM
	if waitRestartBackoff(context.Background(), store, id, time.Minute, sigs) {
		t.Fatal("waitRestartBackoff = true after SIGTERM, want the restart cancelled")
	}
	raw, err := os.ReadFile(store.statePath(id))
	if err != nil {
		t.Fatal(err)
	}
	var st containerState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Restarting || st.Running || st.Status != "exited" || st.ExitCode != 1 {
		t.Fatalf("state after a stop in backoff = %+v, want exited with code 1", st)
	}
}
