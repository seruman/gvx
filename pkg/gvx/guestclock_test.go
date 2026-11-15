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
	"testing"
	"time"

	sentrytime "z.gfire.dev/gvx/pkg/sentry/time"
)

// TestGuestClocksWithoutACounterPublishNoVDSOParameters proves the fail-safe
// path: clocks whose cycle clock cannot serve the guest counter never report
// a calibrated update, so the timekeeper keeps the vDSO page not ready. The
// kernel still reads the time.
func TestGuestClocksWithoutACounterPublishNoVDSOParameters(t *testing.T) {
	c := guestClocksFor(sentrytime.NewCalibratedClocks(false), false)
	for range 3 {
		res := c.Update(false)
		if res.MonotonicOk || res.RealtimeOk || res.MonotonicRawOk {
			t.Fatalf("Update() = %+v, want no calibrated clock", res)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := c.TSCSnapshot(); ok {
		t.Fatal("TSCSnapshot() is valid without a guest counter")
	}
	ns, err := c.GetTime(sentrytime.Realtime)
	if err != nil {
		t.Fatalf("GetTime(Realtime): %v", err)
	}
	if d := time.Duration(time.Now().UnixNano() - ns); d < 0 || d > time.Second {
		t.Fatalf("GetTime(Realtime) is %v from the host clock", d)
	}

	if c := guestClocksFor(sentrytime.NewCalibratedClocks(false), true); func() bool { _, ok := c.(syscallOnlyClocks); return ok }() {
		t.Fatal("guestClocksFor wrapped clocks with a ready guest counter")
	}
}
