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

// Package cpuprofile writes a Go CPU profile of the current process in a way
// that does not stop Hypervisor.framework guests.
//
// runtime/pprof samples with the process timer ITIMER_PROF. On macOS, a vCPU
// executes no guest instruction while ITIMER_PROF or ITIMER_VIRTUAL is armed:
// hv_vcpu_run returns on every cancellation, but the guest makes no progress.
// A SIGPROF sent to a thread does not have that effect. On macOS, Start
// therefore disarms the timer and sends SIGPROF itself, one signal to a thread
// for each sampling period of CPU time that the thread used. The runtime
// accepts a SIGPROF from any source on macOS. Other systems use
// runtime/pprof unchanged.
package cpuprofile

import (
	"errors"
	"io"
	"runtime/pprof"
	"sync"
)

// period is the CPU time that one sample stands for: the 100 Hz default rate
// of runtime/pprof.
const periodMicros = 10_000

// ErrRunning reports that a profile of the process is already running.
var ErrRunning = errors.New("cpuprofile: a CPU profile is already running")

var (
	mu      sync.Mutex
	running bool
)

// Profile is a running CPU profile.
type Profile struct {
	stopSampler func() Stats
	once        sync.Once
	stats       Stats
}

// Stats describes the work of the macOS sampler.
type Stats struct {
	// Signals counts the SIGPROF signals that the sampler sent.
	Signals uint64
}

// Start starts a CPU profile of the process that writes to w. Call Stop to
// finish the profile. One profile can run at a time.
func Start(w io.Writer) (*Profile, error) {
	mu.Lock()
	defer mu.Unlock()
	if running {
		return nil, ErrRunning
	}
	if err := pprof.StartCPUProfile(w); err != nil {
		return nil, err
	}
	stop, err := startSampler()
	if err != nil {
		pprof.StopCPUProfile()
		return nil, err
	}
	running = true
	return &Profile{stopSampler: stop}, nil
}

// Stop finishes the profile and flushes it to the writer of Start. It returns
// the sampler statistics. Later calls return the same statistics.
func (p *Profile) Stop() Stats {
	p.once.Do(func() {
		p.stats = p.stopSampler()
		pprof.StopCPUProfile()
		mu.Lock()
		running = false
		mu.Unlock()
	})
	return p.stats
}
