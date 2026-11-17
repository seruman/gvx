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
	sentrytime "z.gfire.dev/gvx/pkg/sentry/time"
)

// withExactCounterRate makes the vDSO clocks ready at the first update. The
// upstream calibration needs two samples about one second apart, so a guest
// reads the time with system calls for its first second: node makes about
// 7,000 of them at startup.
//
// On macOS the guest counter is mach_absolute_time, and mach_timebase_info
// gives its exact rate (sentrytime.CNTFRQ). CLOCK_UPTIME_RAW, the monotonic
// clock of the Sentry, is mach_absolute_time converted with that rate, so the
// monotonic parameters follow from the counter alone. The realtime clock
// takes one sample at the first update and the upstream calibration after it
// is ready.
func withExactCounterRate(c sentrytime.Clocks) sentrytime.Clocks {
	hz := sentrytime.CNTFRQ()
	if hz == 0 {
		return c
	}
	g := gcd(hz, nanosPerSecond)
	return exactRateClocks{Clocks: c, hz: hz, step: hz / g, nanosPerStep: nanosPerSecond / g}
}

const nanosPerSecond = 1_000_000_000

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// exactRateClocks serves the monotonic clock from the counter rate and the
// realtime clock from one sample until the embedded calibration is ready.
type exactRateClocks struct {
	sentrytime.Clocks
	hz uint64
	// step is the smallest cycle count whose duration is a whole number of
	// nanoseconds, and nanosPerStep is that duration: 3 and 125 at 24 MHz.
	step         uint64
	nanosPerStep uint64
}

// Update implements sentrytime.Clocks.Update.
func (c exactRateClocks) Update(parked bool) sentrytime.UpdateResult {
	res := c.Clocks.Update(parked)
	res.Monotonic, res.MonotonicOk = c.monotonicParams(), true
	if !res.RealtimeOk {
		res.Realtime, res.RealtimeOk = c.realtimeSample(), true
	}
	return res
}

// GetTime implements sentrytime.Clocks.GetTime. The monotonic clock reads
// CLOCK_UPTIME_RAW, which equals the vDSO result for the same counter value.
func (c exactRateClocks) GetTime(id sentrytime.ClockID) (int64, error) {
	if id == sentrytime.Monotonic {
		return sentrytime.MonotonicNanoseconds(), nil
	}
	return c.Clocks.GetTime(id)
}

// monotonicParams returns parameters whose base is a counter value with a
// whole nanosecond time. The vDSO then computes floor(cycles * 1e9 / hz),
// the same value that CLOCK_UPTIME_RAW returns for that counter value.
func (c exactRateClocks) monotonicParams() sentrytime.Parameters {
	now := uint64(sentrytime.Rdtsc())
	base := now - now%c.step
	return sentrytime.Parameters{
		BaseCycles: sentrytime.TSCValue(base),
		BaseRef:    sentrytime.ReferenceNS(base / c.step * c.nanosPerStep),
		Frequency:  c.hz,
	}
}

// realtimeSample returns realtime parameters from one reading of the wall
// clock between two readings of the counter.
func (c exactRateClocks) realtimeSample() sentrytime.Parameters {
	before := sentrytime.Rdtsc()
	ns := sentrytime.RealtimeNanoseconds()
	after := sentrytime.Rdtsc()
	return sentrytime.Parameters{
		BaseCycles: before + (after-before)/2,
		BaseRef:    sentrytime.ReferenceNS(ns),
		Frequency:  c.hz,
	}
}
