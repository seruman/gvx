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
	"strconv"
	"strings"
	"testing"
	"time"

	"z.gfire.dev/gvx/internal/acceptprog"
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
	hostNow := time.Now().UnixNano()
	got := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("clockprobe line %q: %v", line, err)
		}
		got[k] = n
	}
	t.Logf("clockprobe: %v", got)
	if got["vdso_ready"] != 1 {
		t.Errorf("the vDSO parameter page of the guest is not ready; every read of the time is a system call")
	}
	// A system call costs microseconds; a vDSO read costs tens of
	// nanoseconds. 1 µs leaves room for a slow host.
	if ns := got["now_ns"]; ns <= 0 || ns > 1000 {
		t.Errorf("time.Now costs %d ns in the guest, want at most 1000", ns)
	}
	if got["monotonic_ok"] != 1 {
		t.Error("time.Now went back in the guest")
	}
	// The probe printed unix_ns before its 200 ms sleep, and the host read
	// its clock after the guest exited.
	if d := time.Duration(hostNow - got["unix_ns"]); d < 200*time.Millisecond || d > 5*time.Second {
		t.Errorf("guest wall time is %v before the host, want between 200ms and 5s", d)
	}
	if ms := got["sleep_ms"]; ms < 190 || ms > 2000 {
		t.Errorf("a 200 ms sleep measured %d ms in the guest", ms)
	}
}
