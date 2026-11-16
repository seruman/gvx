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
	"unsafe"

	"z.gfire.dev/gvx/internal/xsys/unix"
)

// insnMrsX1CNTVCT is MRS X1, CNTVCT_EL0.
const insnMrsX1CNTVCT uint32 = 0xd53be041

// libSystemSymbol returns the address of a libSystem function.
func libSystemSymbol(t *testing.T, name string) uintptr {
	t.Helper()
	path, _ := unix.BytePtrFromString("/usr/lib/libSystem.B.dylib")
	handle, _, _ := darwinSyscall6(addrOfDlopen(), uintptr(unsafe.Pointer(path)), rtldLazy, 0, 0, 0, 0)
	if handle == 0 {
		t.Fatal("dlopen libSystem failed")
	}
	cname, _ := unix.BytePtrFromString(name)
	sym, _, _ := darwinSyscall6(addrOfDlsym(), handle, uintptr(unsafe.Pointer(cname)), 0, 0, 0, 0)
	if sym == 0 {
		t.Fatalf("dlsym %s failed", name)
	}
	return sym
}

// TestGuestCounterCountsMachAbsoluteTime pins the counter contract of the
// guest clock: hv_vcpu.h defines CNTVCT_EL0 as mach_absolute_time() minus
// the vtimer offset, which hvf sets to zero. The Sentry cycle clock counts
// the same ticks (pkg/sentry/time), so the guest vDSO converts its counter
// with the parameters that the Sentry calibrates. The test reads
// mach_absolute_time itself, so it holds for any timebase.
func TestGuestCounterCountsMachAbsoluteTime(t *testing.T) {
	plat := newTestPlatform(t)
	machAbsoluteTime := libSystemSymbol(t, "mach_absolute_time")
	now := func() uint64 {
		v, _, _ := darwinSyscall6(machAbsoluteTime, 0, 0, 0, 0, 0, 0)
		return uint64(v)
	}
	g := newGuest(t, plat, insnMrsX1CNTVCT, insnSVC0)
	for range 20 {
		g.ac.SetIP(uintptr(guestCodeAddr))
		before := now()
		if sig, _, err := g.switchOnce(); err != nil || sig != nil {
			t.Fatalf("Switch = (%v, %v)", sig, err)
		}
		after := now()
		if v := g.regs().Regs[1]; v < before || v > after {
			t.Fatalf("guest CNTVCT_EL0 = %d, want a value in [%d, %d] of mach_absolute_time", v, before, after)
		}
	}
}
