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
	"regexp"
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

// TestStraceSummaryCountsTheSystemCallsOfTheGuest runs a guest with
// GVX_STRACE=summary. The kernel log must hold one summary row for write(2)
// and a total row, and no line for each call.
func TestStraceSummaryCountsTheSystemCallsOfTheGuest(t *testing.T) {
	t.Setenv("GVX_STRACE", "summary")
	elf, err := testelf.BuildGuestELF64At(runtime.GOARCH, staticBase(), 0, "counted\n")
	if err != nil {
		t.Fatal(err)
	}
	var kernelLog syncBuffer
	cfg := hostTestConfig(t)
	cfg.ELFBytes = elf
	cfg.DebugLog = &kernelLog
	code, stdout, stderr := runConfig(t, cfg)
	if code != 0 || stdout != "counted\n" {
		t.Fatalf("run = (%d, %q, stderr=%q), want (0, %q)", code, stdout, stderr, "counted\n")
	}
	out := kernelLog.String()
	if !regexp.MustCompile(`strace summary: +1 +[0-9.]+ +[0-9.]+ +0  write\n`).MatchString(out) {
		t.Fatalf("the kernel log has no summary row of one write(2):\n%s", out)
	}
	if !strings.Contains(out, "  total\n") {
		t.Fatalf("the kernel log has no total row:\n%s", out)
	}
	if strings.Contains(out, " X write(") {
		t.Fatalf("summary mode logged a line for each call:\n%s", out)
	}
}

// TestStraceSummaryCountsEachSandboxApart runs two sandboxes one after the
// other in one process with GVX_STRACE=summary. Each summary must count the
// one write(2) of its own guest, not the calls of an earlier sandbox.
func TestStraceSummaryCountsEachSandboxApart(t *testing.T) {
	t.Setenv("GVX_STRACE", "summary")
	elf, err := testelf.BuildGuestELF64At(runtime.GOARCH, staticBase(), 0, "apart\n")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile(`strace summary: +([0-9]+) +[0-9.]+ +[0-9.]+ +[0-9]+  write\n`)
	for i := range 2 {
		var kernelLog syncBuffer
		cfg := hostTestConfig(t)
		cfg.ELFBytes = elf
		cfg.DebugLog = &kernelLog
		if code, stdout, stderr := runConfig(t, cfg); code != 0 || stdout != "apart\n" {
			t.Fatalf("run %d = (%d, %q, stderr=%q), want (0, %q)", i, code, stdout, stderr, "apart\n")
		}
		m := row.FindStringSubmatch(kernelLog.String())
		if m == nil || m[1] != "1" {
			t.Fatalf("run %d: write(2) row = %q, want a count of 1:\n%s", i, m, kernelLog.String())
		}
	}
}

// TestProfileLogsTheExitsOfTheGuest runs a guest on hvf with GVX_PROFILE=1.
// The kernel log must hold the exit profile with a system call row.
func TestProfileLogsTheExitsOfTheGuest(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only the hvf platform keeps an exit profile")
	}
	t.Setenv("GVX_PROFILE", "1")
	elf, err := testelf.BuildGuestELF64At(runtime.GOARCH, staticBase(), 0, "profiled\n")
	if err != nil {
		t.Fatal(err)
	}
	var kernelLog syncBuffer
	cfg := hostTestConfig(t)
	cfg.ELFBytes = elf
	cfg.DebugLog = &kernelLog
	if code, stdout, stderr := runConfig(t, cfg); code != 0 || stdout != "profiled\n" {
		t.Fatalf("run = (%d, %q, stderr=%q), want (0, %q)", code, stdout, stderr, "profiled\n")
	}
	out := kernelLog.String()
	if !regexp.MustCompile(`hvf profile: switches=[1-9][0-9]* `).MatchString(out) || !regexp.MustCompile(`hvf profile: +[1-9][0-9]*  syscall`).MatchString(out) {
		t.Fatalf("the kernel log has no hvf profile with a system call row:\n%s", out)
	}
}
