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
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess is not a test. Other tests start the test binary again
// with GVX_DOCKER_TEST_HELPER set, so that a real child process holds a lock
// or exits on request.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("GVX_DOCKER_TEST_HELPER") {
	case "":
		return
	case "hold-supervisor-lock":
		// Hold the lock as a supervisor does, and start a grandchild that
		// outlives this process, as a shim can.
		lock, err := acquireSupervisorLock(os.Getenv("GVX_DOCKER_TEST_DIR"))
		if err != nil {
			os.Exit(3)
		}
		defer lock.release()
		child := exec.Command("/bin/sleep", "30")
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		_, _ = os.Stdout.WriteString("ready " + strconv.Itoa(child.Process.Pid) + "\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	case "exit":
		os.Exit(0)
	}
}

func helperCommand(t *testing.T, mode string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), append([]string{"GVX_DOCKER_TEST_HELPER=" + mode}, env...)...)
	return cmd
}

// TestSupervisorLockReportsDeadAfterSIGKILL proves that the lock of a killed
// supervisor reads as free at once, even while the shim that it started still
// runs. A lock that the shim inherited would keep the container "running".
func TestSupervisorLockReportsDeadAfterSIGKILL(t *testing.T) {
	dir := t.TempDir()
	cmd := helperCommand(t, "hold-supervisor-lock", "GVX_DOCKER_TEST_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := out.Read(buf)
	line := strings.TrimSpace(string(buf[:n]))
	if !strings.HasPrefix(line, "ready ") {
		t.Fatalf("helper said %q, want ready", line)
	}
	grandchild, err := strconv.Atoi(strings.TrimPrefix(line, "ready "))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) }()

	if pid, ok := supervisorPID(dir); !ok || pid != cmd.Process.Pid {
		t.Fatalf("supervisorPID = %d %v while the supervisor %d holds its lock", pid, ok, cmd.Process.Pid)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := syscall.Kill(grandchild, 0); err != nil {
		t.Fatalf("the grandchild must still run for this test: %v", err)
	}
	if supervisorAlive(dir) {
		t.Fatal("supervisorAlive = true after SIGKILL of the supervisor: a child process kept the lock")
	}
}

// TestSupervisorLockRefusesSecondHolder proves that a second supervisor of one
// container fails while the first one lives, and succeeds after it ends.
func TestSupervisorLockRefusesSecondHolder(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireSupervisorLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireSupervisorLock(dir); err == nil {
		t.Fatal("a second lock succeeded while the first is held")
	}
	first.release()
	second, err := acquireSupervisorLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	second.release()
}

// TestRefreshStateIgnoresZombieSupervisor proves that a supervisor that exited
// reads as dead, even before its parent reaps it. kill(pid, 0) succeeds for a
// zombie, so a PID check keeps the container "running".
func TestRefreshStateIgnoresZombieSupervisor(t *testing.T) {
	dir := t.TempDir()
	cmd := helperCommand(t, "exit")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Do not reap the child until the end of the test, so it stays a zombie.
	defer func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper %d did not become a zombie: ps stat %q", pid, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	state := containerState{Running: true, Status: "running", SupervisorPid: pid}
	refreshProcessStateIn(dir, &state)
	if state.Running || state.Status != "exited" {
		t.Fatalf("state after refresh = running %v status %q, want an exited container", state.Running, state.Status)
	}
}
