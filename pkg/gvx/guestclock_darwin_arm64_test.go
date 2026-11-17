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

package gvx

import (
	"testing"
	"time"

	sentrytime "z.gfire.dev/gvx/pkg/sentry/time"
)

// TestExactRateClocksAreReadyAtTheFirstUpdate checks the parameters of the
// first update against the host clocks. The monotonic result of the vDSO
// formula for a counter value must lie between two CLOCK_UPTIME_RAW reads
// around that value, and the realtime result within 1 ms of the wall clock.
func TestExactRateClocksAreReadyAtTheFirstUpdate(t *testing.T) {
	c := withExactCounterRate(sentrytime.NewCalibratedClocks(false))
	res := c.Update(false)
	if !res.MonotonicOk || !res.RealtimeOk {
		t.Fatalf("Update() = %+v, want both clocks ready at the first update", res)
	}
	for range 1000 {
		before := sentrytime.MonotonicNanoseconds()
		cycles := sentrytime.Rdtsc()
		after := sentrytime.MonotonicNanoseconds()
		got, ok := res.Monotonic.ComputeTime(cycles)
		if !ok || got < before || got > after {
			t.Fatalf("monotonic ComputeTime(%d) = %d, want within [%d, %d]", cycles, got, before, after)
		}
	}
	// The sample takes its counter value between two reads, so its result
	// can lead the wall clock by up to half a read: compare the distance.
	got, ok := res.Realtime.ComputeTime(sentrytime.Rdtsc())
	if d := time.Duration(time.Now().UnixNano() - got).Abs(); !ok || d > time.Millisecond {
		t.Fatalf("realtime ComputeTime is %v from the wall clock, want within 1ms", d)
	}
	if mono, err := c.GetTime(sentrytime.Monotonic); err != nil || mono <= 0 {
		t.Fatalf("GetTime(Monotonic) = %d, %v", mono, err)
	}
}
