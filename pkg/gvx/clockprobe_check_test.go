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

package gvx

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// checkClockProbe checks the output of the clockprobe that ran until hostNow.
// With requireRealtime false, a guest whose realtime vDSO clock is not ready
// may read the wall time with a system call (see
// TestRestoredGuestReadsTheClockThroughTheVDSO).
func checkClockProbe(t *testing.T, stdout string, hostNow int64, requireRealtime bool) {
	t.Helper()
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
	realtimeFast := requireRealtime || got["vdso_realtime_ready"] == 1
	if requireRealtime && got["vdso_realtime_ready"] != 1 {
		t.Errorf("the realtime clock of the vDSO page is not ready")
	}
	// A system call costs microseconds; a vDSO read costs tens of
	// nanoseconds. 1 µs leaves room for a slow host.
	if ns := got["now_ns"]; ns <= 0 || (realtimeFast && ns > 1000) {
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
