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

package hvf

import (
	"testing"
	_ "unsafe" // for go:linkname
)

// insnMrsX1CNTVCT is MRS X1, CNTVCT_EL0.
const insnMrsX1CNTVCT uint32 = 0xd53be041

//go:linkname runtimeNanotime runtime.nanotime
func runtimeNanotime() int64

// TestGuestCounterCountsMachAbsoluteTime pins the counter contract of the
// guest clock: a guest reads mach_absolute_time ticks (24 MHz on Apple
// Silicon) from CNTVCT_EL0. The Sentry cycle clock counts the same ticks
// (pkg/sentry/time/tsc_darwin_arm64.s), so the guest vDSO converts its
// counter with the parameters that the Sentry calibrates. runtime.nanotime
// is mach_absolute_time in nanoseconds.
func TestGuestCounterCountsMachAbsoluteTime(t *testing.T) {
	plat := newTestPlatform(t)
	g := newGuest(t, plat, insnMrsX1CNTVCT, insnSVC0)
	toTicks := func(ns int64) int64 { return ns/1000*24 + ns%1000*24/1000 }
	for range 20 {
		g.ac.SetIP(uintptr(guestCodeAddr))
		before := toTicks(runtimeNanotime())
		if sig, _, err := g.switchOnce(); err != nil || sig != nil {
			t.Fatalf("Switch = (%v, %v)", sig, err)
		}
		after := toTicks(runtimeNanotime())
		// One tick of slack on each side covers the rounding of nanotime.
		if v := int64(g.regs().Regs[1]); v < before-1 || v > after+1 {
			t.Fatalf("guest CNTVCT_EL0 = %d, want a value in [%d, %d] of mach_absolute_time", v, before, after)
		}
	}
}
