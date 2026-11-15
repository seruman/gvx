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

import "unsafe"

// The cycle clock of the Sentry on macOS is mach_absolute_time. A guest under
// Hypervisor.framework reads CNTVCT_EL0, which hv_vcpu.h defines as
// mach_absolute_time() - vtimer_offset; the hvf platform sets the offset to
// zero on every vCPU. The guest vDSO therefore converts its counter with the
// parameters that the Sentry calibrates against this clock. Both functions
// are public libSystem API and make no system call, so they work after the
// host lockdown.

func machAbsoluteTimeTrampoline()
func addrOfMachAbsoluteTime() uintptr
func machTimebaseInfoTrampoline()
func addrOfMachTimebaseInfo() uintptr

//go:cgo_import_dynamic libc_mach_absolute_time mach_absolute_time "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_timebase_info mach_timebase_info "/usr/lib/libSystem.B.dylib"

var (
	machAbsoluteTimeAddr = addrOfMachAbsoluteTime()
	machTimebaseInfoAddr = addrOfMachTimebaseInfo()
)

// defaultMachCyclesHz is the mach_absolute_time rate of Apple Silicon. The
// clock uses it only when mach_timebase_info fails; GuestCounterReady then
// reports false.
const defaultMachCyclesHz = 24_000_000

// machCyclesHz is the rate of machCycles in Hz, or 0 when mach_timebase_info
// failed or gave a rate that is not a whole number of Hz.
var machCyclesHz = readMachCyclesHz()

// readMachCyclesHz returns 1e9 * denom / numer, the ticks of one second.
func readMachCyclesHz() uint64 {
	var tb struct{ numer, denom uint32 }
	r, _, _ := darwinRawSyscall6(machTimebaseInfoAddr, uintptr(unsafe.Pointer(&tb)), 0, 0, 0, 0, 0)
	// A failure leaves the rate zero. GuestCounterReady then reports false,
	// and pkg/gvx logs the fall back to system calls.
	hz, _ := timebaseHz(int32(r), tb.numer, tb.denom)
	return hz
}

// timebaseHz converts a mach_timebase_info result to Hz. It rejects a failed
// call, a zero field, and a rate that is not a whole number of Hz.
func timebaseHz(kr int32, numer, denom uint32) (uint64, bool) {
	if kr != 0 || numer == 0 || denom == 0 {
		return 0, false
	}
	n := 1_000_000_000 * uint64(denom)
	if n%uint64(numer) != 0 {
		return 0, false
	}
	return n / uint64(numer), true
}

// GuestCounterReady reports whether the cycle clock has a known rate, so the
// guest vDSO may convert its counter with parameters calibrated against it.
// When it is false, the embedder must not publish the vDSO clock parameters,
// and the guest reads the time with system calls.
func GuestCounterReady() bool {
	return machCyclesHz != 0
}

// machCycles returns mach_absolute_time. It runs from nosplit callers, as
// runtime.nanotime1 does: the libSystem call goes through libcCall on the
// system stack.
//
//go:nosplit
func machCycles() TSCValue {
	t, _, _ := darwinRawSyscall6(machAbsoluteTimeAddr, 0, 0, 0, 0, 0, 0)
	return TSCValue(t)
}

// machCyclesFrequency returns the rate of machCycles.
//
//go:nosplit
func machCyclesFrequency() TSCValue {
	if machCyclesHz == 0 {
		return defaultMachCyclesHz
	}
	return TSCValue(machCyclesHz)
}
