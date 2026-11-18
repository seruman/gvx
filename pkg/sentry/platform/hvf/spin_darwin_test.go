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
	"time"
)

// TestSpinYieldRule pins when a hand-off spin gives up its processor. A
// yield moves the spinning goroutine to the end of the run queue, and the
// hand-off then waits for the scheduler. With GOMAXPROCS of 3 or more, the
// partner goroutine has a processor of its own, so an early yield only adds
// latency: BenchmarkSwitchSyscall measured 3.4 us with a yield every 1024
// polls and 1.26 us without one. With 1 or 2 processors, the partner may
// wait for the processor of the spinner, so the spin yields at once.
func TestSpinYieldRule(t *testing.T) {
	for _, tc := range []struct {
		procs   int
		elapsed time.Duration
		want    bool
	}{
		{procs: 1, elapsed: 0, want: true},
		{procs: 2, elapsed: time.Microsecond, want: true},
		{procs: 3, elapsed: 0, want: false},
		{procs: 3, elapsed: spinYieldAfter - time.Nanosecond, want: false},
		{procs: 3, elapsed: spinYieldAfter, want: true},
		{procs: 18, elapsed: 5 * time.Microsecond, want: false},
		{procs: 18, elapsed: 30 * time.Microsecond, want: true},
	} {
		if got := spinShouldYield(tc.procs, tc.elapsed); got != tc.want {
			t.Errorf("spinShouldYield(%d, %v) = %v, want %v", tc.procs, tc.elapsed, got, tc.want)
		}
	}
}

// TestWorkerSpinYieldRule pins the yield rule of an idle vCPU worker. The
// worker holds a locked OS thread, so a yield parks that thread and the next
// request must wake it: a CPU profile of go build showed a quarter of the
// samples in that park and wake. With GOMAXPROCS of 3 or more, the worker
// never yields during its spin. With 1 or 2 processors, the Switch caller
// may need the processor of the worker, so the worker yields at once.
func TestWorkerSpinYieldRule(t *testing.T) {
	for _, tc := range []struct {
		procs   int
		elapsed time.Duration
		want    bool
	}{
		{procs: 1, elapsed: 0, want: true},
		{procs: 2, elapsed: time.Microsecond, want: true},
		{procs: 3, elapsed: spinYieldAfter, want: false},
		{procs: 18, elapsed: workerSpin, want: false},
	} {
		if got := workerSpinShouldYield(tc.procs, tc.elapsed); got != tc.want {
			t.Errorf("workerSpinShouldYield(%d, %v) = %v, want %v", tc.procs, tc.elapsed, got, tc.want)
		}
	}
}
