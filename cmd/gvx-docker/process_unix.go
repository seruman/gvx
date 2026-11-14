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
	"os"
	"os/exec"
	"syscall"
)

// signalProcess sends sig to the process pid. It refuses a PID that is not
// positive, because kill(2) sends such a signal to a process group or to
// every process of the user.
func signalProcess(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("refuse to signal PID %d", pid)
	}
	return syscall.Kill(pid, sig)
}

func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

var stopSignal os.Signal = syscall.SIGTERM
