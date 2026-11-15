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
	"sync"

	"z.gfire.dev/gvx/pkg/log"
	sentrytime "z.gfire.dev/gvx/pkg/sentry/time"
)

// newGuestClocks returns the clocks of a kernel. When the cycle clock cannot
// serve the guest counter (sentrytime.GuestCounterReady is false), the clocks
// never report a calibrated update. The timekeeper then leaves the vDSO
// parameter page not ready, and the guest reads the time with system calls,
// which the Sentry answers from the host clock.
func newGuestClocks() sentrytime.Clocks {
	return guestClocksFor(sentrytime.NewCalibratedClocks(false), sentrytime.GuestCounterReady())
}

func guestClocksFor(c sentrytime.Clocks, counterReady bool) sentrytime.Clocks {
	if counterReady {
		return c
	}
	warnSyscallClockOnce.Do(func() {
		log.Warningf("gvx: the host cycle clock cannot serve the guest counter; guest clock reads use system calls")
	})
	return syscallOnlyClocks{c}
}

var warnSyscallClockOnce sync.Once

// syscallOnlyClocks hides the calibration of its clocks from the vDSO.
type syscallOnlyClocks struct {
	sentrytime.Clocks
}

// Update implements sentrytime.Clocks.Update. It keeps the calibration up to
// date for the kernel, and reports no clock as calibrated.
func (c syscallOnlyClocks) Update(parked bool) sentrytime.UpdateResult {
	c.Clocks.Update(parked)
	return sentrytime.UpdateResult{}
}

// TSCSnapshot implements sentrytime.Clocks.TSCSnapshot. The guest counter
// does not follow the cycle clock, so no snapshot is valid.
func (syscallOnlyClocks) TSCSnapshot() (sentrytime.TSCSnapshot, bool) {
	return sentrytime.TSCSnapshot{}, false
}
