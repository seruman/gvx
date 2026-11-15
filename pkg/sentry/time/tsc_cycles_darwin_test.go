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

//go:build darwin

package time

import (
	"testing"
	stdtime "time"
	_ "unsafe" // for go:linkname
)

// runtimeNanotime is mach_absolute_time in nanoseconds.
//
//go:linkname runtimeNanotime runtime.nanotime
func runtimeNanotime() int64

// TestTSCIsTheGuestCounterDomain checks that the cycle clock of the Sentry
// counts mach_absolute_time ticks. A guest under Hypervisor.framework reads
// that value from CNTVCT_EL0, and the vDSO of the guest converts it with the
// parameters that the Sentry calibrates against this clock. The host
// CNTVCT_EL0 runs at another rate and from another base, so a calibration
// against it gives the guest a wrong time.
func TestTSCIsTheGuestCounterDomain(t *testing.T) {
	// runtime.nanotime is mach_absolute_time scaled to nanoseconds.
	if freq := CNTFRQ(); freq != 24_000_000 {
		t.Fatalf("CNTFRQ() = %d, want the 24 MHz mach_absolute_time rate", freq)
	}
	for range 100 {
		before := runtimeNanotime()
		tsc := int64(TSC())
		after := runtimeNanotime()
		ns := tsc / 24 * 1000
		ns += tsc % 24 * 1000 / 24
		if ns < before-1000 || ns > after+1000 {
			t.Fatalf("TSC() = %d (%d ns), want a time in [%d, %d] of mach_absolute_time", tsc, ns, before, after)
		}
	}
	start, startNS := TSC(), runtimeNanotime()
	stdtime.Sleep(50 * stdtime.Millisecond)
	ticks, elapsed := float64(TSC()-start), float64(runtimeNanotime()-startNS)
	if rate := ticks / elapsed * 1e9; rate < 23_900_000 || rate > 24_100_000 {
		t.Fatalf("TSC() advanced at %.0f Hz, want 24 MHz", rate)
	}
}
