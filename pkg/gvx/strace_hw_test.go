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

//go:build darwin || windows

package gvx

import (
	"runtime"
	"strings"
	"testing"

	"z.gfire.dev/gvx/internal/testelf"
)

// TestStraceLogsTheSystemCallsOfTheGuest runs a guest with GVX_STRACE=1 and
// reads its kernel log. The log must hold the exit line of the write call of
// the guest. Package initialization rebuilds the system call tables after
// the runtime globals start, so a trace switch that runs only then is lost.
func TestStraceLogsTheSystemCallsOfTheGuest(t *testing.T) {
	t.Setenv("GVX_STRACE", "1")
	elf, err := testelf.BuildGuestELF64At(runtime.GOARCH, staticBase(), 0, "traced\n")
	if err != nil {
		t.Fatal(err)
	}
	var kernelLog syncBuffer
	cfg := hostTestConfig(t)
	cfg.ELFBytes = elf
	cfg.DebugLog = &kernelLog
	code, stdout, stderr := runConfig(t, cfg)
	if code != 0 || stdout != "traced\n" {
		t.Fatalf("run = (%d, %q, stderr=%q), want (0, %q)", code, stdout, stderr, "traced\n")
	}
	if !strings.Contains(kernelLog.String(), " X write(") {
		t.Fatalf("the kernel log has no exit line of write(2) with GVX_STRACE=1:\n%s", kernelLog.String())
	}
}
