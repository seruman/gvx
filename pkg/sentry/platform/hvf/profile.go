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
	"fmt"
	"sync/atomic"
	"time"
)

// ExitClass names one kind of hv_vcpu_run return in a Profile.
type ExitClass int

const (
	// ExitSyscall is an SVC that the Sentry serves.
	ExitSyscall ExitClass = iota
	// ExitSignal is a fault or trap that becomes a Linux signal.
	ExitSignal
	// ExitSysReg is a trapped MRS that the platform emulates.
	ExitSysReg
	// ExitWFx is a trapped WFI or WFE.
	ExitWFx
	// ExitCancel is a return after hv_vcpus_exit: an interrupt, a TLB
	// shootdown, or a Go preemption signal.
	ExitCancel
	// ExitOther is any other return.
	ExitOther
	numExitClasses
)

var exitClassNames = [numExitClasses]string{"syscall", "signal", "sysreg", "wfx", "cancel", "other"}

// String implements fmt.Stringer.
func (c ExitClass) String() string {
	if c < 0 || c >= numExitClasses {
		return fmt.Sprintf("ExitClass(%d)", int(c))
	}
	return exitClassNames[c]
}

// Profile is a snapshot of the exit counters of one Platform.
type Profile struct {
	// Switches counts Context.Switch calls that reached a vCPU.
	Switches uint64
	// SwitchNanos is the time from the hand-off to a vCPU thread to the end
	// of the Switch, summed over all Switches.
	SwitchNanos int64
	// GuestNanos is the time inside hv_vcpu_run, summed over all exits.
	GuestNanos int64
	// Exits counts the returns of hv_vcpu_run by class.
	Exits [numExitClasses]uint64
}

// Lines formats p as text lines for a log.
func (p Profile) Lines() []string {
	var exits uint64
	for _, n := range p.Exits {
		exits += n
	}
	lines := []string{fmt.Sprintf("hvf profile: switches=%d exits=%d switch_ms=%.3f guest_ms=%.3f",
		p.Switches, exits, float64(p.SwitchNanos)/1e6, float64(p.GuestNanos)/1e6)}
	if p.Switches > 0 {
		lines = append(lines, fmt.Sprintf("hvf profile: mean per switch: %.2f us total, %.2f us in the guest, %.2f exits",
			float64(p.SwitchNanos)/1e3/float64(p.Switches), float64(p.GuestNanos)/1e3/float64(p.Switches), float64(exits)/float64(p.Switches)))
	}
	for c, n := range p.Exits {
		if n > 0 {
			lines = append(lines, fmt.Sprintf("hvf profile: %10d  %s", n, ExitClass(c)))
		}
	}
	return lines
}

// exitProfile holds the counters of one Platform. A nil *exitProfile counts
// nothing, so the run loop of an unprofiled Platform reads no clock.
type exitProfile struct {
	switches    atomic.Uint64
	switchNanos atomic.Int64
	guestNanos  atomic.Int64
	exits       [numExitClasses]atomic.Uint64
}

func (p *exitProfile) now() time.Time {
	if p == nil {
		return time.Time{}
	}
	return time.Now()
}

func (p *exitProfile) addSwitch(start time.Time) {
	if p == nil {
		return
	}
	p.switches.Add(1)
	p.switchNanos.Add(int64(time.Since(start)))
}

func (p *exitProfile) addGuest(start time.Time) {
	if p == nil {
		return
	}
	p.guestNanos.Add(int64(time.Since(start)))
}

func (p *exitProfile) addExit(c ExitClass) {
	if p == nil {
		return
	}
	p.exits[c].Add(1)
}

func (p *exitProfile) snapshot() Profile {
	if p == nil {
		return Profile{}
	}
	s := Profile{
		Switches:    p.switches.Load(),
		SwitchNanos: p.switchNanos.Load(),
		GuestNanos:  p.guestNanos.Load(),
	}
	for c := range p.exits {
		s.Exits[c] = p.exits[c].Load()
	}
	return s
}

// EnableProfile starts the exit counters of p. Call it before the first
// Switch; the counters cost two clock reads for each exit.
func (p *Platform) EnableProfile() {
	p.profile.CompareAndSwap(nil, &exitProfile{})
}

// Profile returns the exit counters of p, or a zero Profile when
// EnableProfile was not called.
func (p *Platform) Profile() Profile {
	return p.profile.Load().snapshot()
}

// exitClassOf maps the decoded kind of a stub or hypervisor exit to its
// profile class.
func exitClassOf(k exitKind) ExitClass {
	switch k {
	case exitSyscall:
		return ExitSyscall
	case exitSignal:
		return ExitSignal
	case exitSysReg:
		return ExitSysReg
	case exitWFx:
		return ExitWFx
	default:
		return ExitOther
	}
}

// ProfileLines returns the text lines of Profile, or nil when EnableProfile
// was not called.
func (p *Platform) ProfileLines() []string {
	prof := p.profile.Load()
	if prof == nil {
		return nil
	}
	return prof.snapshot().Lines()
}
