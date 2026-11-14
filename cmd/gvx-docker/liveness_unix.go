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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// supervisorLockName is the file in a container directory that the live
// supervisor of the container holds with an exclusive flock. The kernel drops
// the lock when the supervisor dies, also on SIGKILL, so the lock tells
// whether a supervisor runs. A PID cannot: a zombie and a reused PID both
// answer kill(pid, 0).
const supervisorLockName = "supervisor.lock"

// supervisorStartWindow is the time that a started supervisor has to take
// its lock. A container whose supervisor has no lock after it reads as
// exited.
const supervisorStartWindow = 30 * time.Second

// supervisorLock is the held lock of one supervisor.
type supervisorLock struct{ f *os.File }

// acquireSupervisorLock takes the supervisor lock of the container directory
// dir for this process. It fails when another supervisor holds it. Callers
// serialize it with the store lock, so two supervisors cannot both pass the
// check.
//
// The function writes the PID into a new file, locks that file, and renames
// it over the old one. A reader that finds the lock held therefore reads the
// PID of the holder, never a PID that an earlier supervisor left.
//
// The descriptor has O_CLOEXEC, so a shim never inherits the lock: a flock
// belongs to the open file description, and a shim with a copy would keep the
// container "running" after the supervisor died.
func acquireSupervisorLock(dir string) (supervisorLock, error) {
	if supervisorAlive(dir) {
		return supervisorLock{}, errors.New("another supervisor runs this container")
	}
	tmp := filepath.Join(dir, supervisorLockName+"."+strconv.Itoa(os.Getpid()))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return supervisorLock{}, fmt.Errorf("open supervisor lock: %w", err)
	}
	fail := func(err error) (supervisorLock, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return supervisorLock{}, err
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		return fail(fmt.Errorf("write supervisor lock: %w", err))
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("lock supervisor: %w", err))
	}
	if err := os.Rename(tmp, filepath.Join(dir, supervisorLockName)); err != nil {
		return fail(fmt.Errorf("publish supervisor lock: %w", err))
	}
	return supervisorLock{f: f}, nil
}

func (l supervisorLock) release() {
	if l.f != nil {
		_ = l.f.Close()
	}
}

// supervisorAlive reports whether a supervisor holds the lock of the
// container directory dir.
func supervisorAlive(dir string) bool {
	_, ok := supervisorPID(dir)
	return ok
}

// supervisorPID returns the PID of the supervisor that holds the lock of the
// container directory dir. It probes with a non-blocking shared lock, which
// fails only while the holder lives, and reads the PID from the same file.
func supervisorPID(dir string) (int, bool) {
	f, err := os.OpenFile(filepath.Join(dir, supervisorLockName), os.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, false
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return 0, false
	}
	data, err := io.ReadAll(io.LimitReader(f, 32))
	if err != nil {
		return 0, true
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, true
	}
	return pid, true
}

// refreshProcessStateIn marks a running or restarting container as exited
// when no supervisor holds the lock of its directory dir. Only a supervisor
// runs a container, so the lock is the one source of truth for "alive".
func refreshProcessStateIn(dir string, state *containerState) {
	if !(state.Running || state.Restarting) || supervisorAlive(dir) {
		return
	}
	if !state.StartPending.IsZero() && time.Since(state.StartPending) < supervisorStartWindow {
		// The API process started a supervisor that has not taken its lock
		// yet.
		return
	}
	state.Paused = false
	state.Running = false
	state.Restarting = false
	state.Status = "exited"
	state.Pid = 0
	state.SupervisorPid = 0
	if state.FinishedAt.IsZero() {
		state.FinishedAt = time.Now().UTC()
	}
	if state.ExitCode == 0 {
		state.ExitCode = 137
	}
}

// lockStateDir takes the lock that one serve process holds on its state
// directory for its whole life. It fails when another serve holds it.
func lockStateDir(dir string) (supervisorLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return supervisorLock{}, fmt.Errorf("create state directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "serve.lock"), os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return supervisorLock{}, fmt.Errorf("open serve lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return supervisorLock{}, fmt.Errorf("another gvx-docker serve uses %s", dir)
		}
		return supervisorLock{}, fmt.Errorf("lock state directory: %w", err)
	}
	return supervisorLock{f: f}, nil
}
