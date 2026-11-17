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

package hvf

import (
	"strings"
	"testing"
)

// TestProfileCountsTheExitsOfTheGuest runs a guest that executes WFI and then
// SVC on every Switch. The profile must count one Switch, one WFx exit and
// one system call exit for each round, and time inside the guest.
func TestProfileCountsTheExitsOfTheGuest(t *testing.T) {
	const wfi uint32 = 0xd503207f
	const rounds = 50
	plat := newTestPlatform(t)
	plat.EnableProfile()
	g := newGuest(t, plat, wfi, insnSVC0, 0x17fffffe) // WFI; SVC #0; B .-8
	for range rounds {
		if sig, _, err := g.switchOnce(); err != nil || sig != nil {
			t.Fatalf("Switch = (%v, %v)", sig, err)
		}
	}
	got := plat.Profile()
	if got.Switches != rounds {
		t.Errorf("Switches = %d, want %d", got.Switches, rounds)
	}
	if got.Exits[ExitSyscall] != rounds || got.Exits[ExitWFx] != rounds {
		t.Errorf("Exits = %v, want %d system call and %d WFx exits", got.Exits, rounds, rounds)
	}
	if got.GuestNanos <= 0 || got.SwitchNanos < got.GuestNanos {
		t.Errorf("GuestNanos = %d, SwitchNanos = %d, want 0 < guest <= switch", got.GuestNanos, got.SwitchNanos)
	}
	if lines := strings.Join(got.Lines(), "\n"); !strings.Contains(lines, "syscall") || !strings.Contains(lines, "wfx") {
		t.Errorf("Lines() has no syscall or wfx row:\n%s", lines)
	}
}

// TestProfileIsOffByDefault verifies that a platform counts nothing until
// EnableProfile, so the run loop pays for no clock read.
func TestProfileIsOffByDefault(t *testing.T) {
	plat := newTestPlatform(t)
	g := newGuest(t, plat, insnSVC0, 0x17ffffff) // SVC #0; B .-4
	if sig, _, err := g.switchOnce(); err != nil || sig != nil {
		t.Fatalf("Switch = (%v, %v)", sig, err)
	}
	if got := plat.Profile(); got.Switches != 0 || got.Exits[ExitSyscall] != 0 {
		t.Errorf("Profile() = %+v without EnableProfile, want zero", got)
	}
}
