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

//go:build darwin && arm64

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
// is mach_absolute_time, the value that hv_vcpu.h gives a guest in
// CNTVCT_EL0 with a zero vtimer offset, and that its rate is the
// mach_timebase_info rate.
func TestTSCIsTheGuestCounterDomain(t *testing.T) {
	for range 100 {
		before := uint64(machCycles())
		tsc := uint64(TSC())
		after := uint64(machCycles())
		if tsc < before || tsc > after {
			t.Fatalf("TSC() = %d, want a value in [%d, %d] of mach_absolute_time", tsc, before, after)
		}
	}
	if !GuestCounterReady() {
		t.Fatal("GuestCounterReady() = false on a host with mach_timebase_info")
	}
	// runtime.nanotime is mach_absolute_time in nanoseconds.
	freq := CNTFRQ()
	start, startNS := TSC(), runtimeNanotime()
	stdtime.Sleep(50 * stdtime.Millisecond)
	ticks, elapsed := float64(TSC()-start), float64(runtimeNanotime()-startNS)
	if rate := ticks / elapsed * 1e9; rate < float64(freq)*0.999 || rate > float64(freq)*1.001 {
		t.Fatalf("TSC() advanced at %.0f Hz, want the CNTFRQ() rate %d Hz", rate, freq)
	}
}

func TestTimebaseHz(t *testing.T) {
	for _, tc := range []struct {
		kr           int32
		numer, denom uint32
		want         uint64
		ok           bool
	}{
		{0, 125, 3, 24_000_000, true},
		{0, 1, 1, 1_000_000_000, true},
		{0, 3, 125, 0, false},
		{0, 7, 1, 0, false},
		{0, 0, 3, 0, false},
		{0, 125, 0, 0, false},
		{5, 125, 3, 0, false},
	} {
		got, ok := timebaseHz(tc.kr, tc.numer, tc.denom)
		if got != tc.want || ok != tc.ok {
			t.Errorf("timebaseHz(%d, %d, %d) = (%d, %v), want (%d, %v)", tc.kr, tc.numer, tc.denom, got, ok, tc.want, tc.ok)
		}
	}
}

func BenchmarkRdtsc(b *testing.B) {
	for range b.N {
		_ = Rdtsc()
	}
}
