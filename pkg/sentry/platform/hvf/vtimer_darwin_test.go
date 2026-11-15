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
)

// TestVCPUVTimerOffsetIsZero reads the virtual timer offset of a vCPU that a
// Switch used. hv_vcpu.h defines CNTVCT_EL0 = mach_absolute_time() -
// vtimer_offset, so a zero offset makes the guest counter equal the host
// mach_absolute_time by contract.
func TestVCPUVTimerOffsetIsZero(t *testing.T) {
	plat := newTestPlatform(t)
	g := newGuest(t, plat, insnSVC0)
	if sig, _, err := g.switchOnce(); err != nil || sig != nil {
		t.Fatalf("Switch = (%v, %v)", sig, err)
	}
	offsets := plat.vm.vtimerOffsets()
	if len(offsets) == 0 {
		t.Fatal("no vCPU worker reported its virtual timer offset")
	}
	for i, off := range offsets {
		if off.err != nil {
			t.Fatalf("worker %d: hv_vcpu_get_vtimer_offset: %v", i, off.err)
		}
		if off.value != 0 {
			t.Fatalf("worker %d: virtual timer offset = %d, want 0", i, off.value)
		}
	}
}

type vtimerOffset struct {
	value uint64
	err   error
}

// vtimerOffsets returns the virtual timer offset that each started vCPU read
// back after configure set it. Tests use it.
func (v *vmState) vtimerOffsets() []vtimerOffset {
	var out []vtimerOffset
	for i := range v.pool.slots[:v.pool.max] {
		w := v.pool.slots[i].Load()
		if w == nil {
			continue
		}
		out = append(out, vtimerOffset{value: w.vtimerOffset})
	}
	return out
}
