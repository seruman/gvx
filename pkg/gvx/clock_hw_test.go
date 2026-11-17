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
	"time"

	"z.gfire.dev/gvx/internal/acceptprog"
	sentrytime "z.gfire.dev/gvx/pkg/sentry/time"
)

// TestGuestClockUsesTheVDSO runs a guest that reads the time in a loop. The
// guest must read it through the vDSO, without a system call. Its time must
// match the host clock and advance at the rate of the host clock.
func TestGuestClockUsesTheVDSO(t *testing.T) {
	requireGuestBackend(t)
	cfg := hostTestConfig(t)
	cfg.BinaryPath = acceptprog.BuildClockProbe(t, runtime.GOARCH)
	code, stdout, stderr := runConfig(t, cfg)
	if code != 0 {
		t.Fatalf("clockprobe exited %d: %s", code, stderr)
	}
	checkClockProbe(t, stdout, time.Now().UnixNano(), true)
}

// TestGuestClockIsReadyAtBoot reads the vDSO parameter page at the first
// instruction of the guest program. The page must be ready then: a guest
// that reads the time through system calls for its first second makes
// thousands of them, as node does at startup.
func TestGuestClockIsReadyAtBoot(t *testing.T) {
	requireGuestBackend(t)
	if !sentrytime.GuestCounterReady() {
		t.Skip("the host cycle clock cannot serve the guest counter")
	}
	cfg := hostTestConfig(t)
	cfg.BinaryPath = acceptprog.BuildClockProbe(t, runtime.GOARCH)
	cfg.Argv = []string{"boot"}
	code, stdout, stderr := runConfig(t, cfg)
	if code != 0 {
		t.Fatalf("clockprobe exited %d: %s", code, stderr)
	}
	for _, want := range []string{"boot_vdso_ready=1", "boot_vdso_realtime_ready=1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("clockprobe boot = %q, want %s", stdout, want)
		}
	}
}
