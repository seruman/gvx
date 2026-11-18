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
	"errors"
	"testing"
	"time"

	"z.gfire.dev/gvx/internal/xsys/unix"
)

// TestCPULimitPolicy pins what the platform does with RLIMIT_CPU before it
// creates the VM. A finite limit stops every guest instruction.
func TestCPULimitPolicy(t *testing.T) {
	inf := uint64(unix.RLIM_INFINITY)
	for _, tc := range []struct {
		name    string
		in      unix.Rlimit
		set     *unix.Rlimit
		wantErr bool
	}{
		{name: "unlimited", in: unix.Rlimit{Cur: inf, Max: inf}},
		{name: "soft only", in: unix.Rlimit{Cur: 100, Max: inf}, set: &unix.Rlimit{Cur: inf, Max: inf}},
		{name: "hard", in: unix.Rlimit{Cur: 100, Max: 100}, wantErr: true},
	} {
		var got *unix.Rlimit
		err := liftCPULimit(
			func(l *unix.Rlimit) error { *l = tc.in; return nil },
			func(l *unix.Rlimit) error { got = l; return nil },
		)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: liftCPULimit error = %v, want error %v", tc.name, err, tc.wantErr)
		}
		if tc.wantErr && !errors.Is(err, ErrCPULimit) {
			t.Errorf("%s: error %v does not wrap ErrCPULimit", tc.name, err)
		}
		if (got == nil) != (tc.set == nil) || (got != nil && *got != *tc.set) {
			t.Errorf("%s: setrlimit got %+v, want %+v", tc.name, got, tc.set)
		}
	}
}

// TestGuestRunsUnderASoftCPULimit sets a finite soft RLIMIT_CPU, as
// "ulimit -St" does, and runs a guest. Without the lift the vCPU executes no
// instruction and Switch never returns.
func TestGuestRunsUnderASoftCPULimit(t *testing.T) {
	var old unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CPU, &old); err != nil {
		t.Fatal(err)
	}
	if old.Max != uint64(unix.RLIM_INFINITY) {
		t.Skipf("the hard RLIMIT_CPU of the test is finite (%d)", old.Max)
	}
	soft := unix.Rlimit{Cur: 100000, Max: old.Max}
	if err := unix.Setrlimit(unix.RLIMIT_CPU, &soft); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_CPU, &old) })

	plat := newTestPlatform(t)
	g := newGuest(t, plat, insnSVC0, 0x17ffffff) // SVC #0; B .-4
	done := make(chan error, 1)
	go func() {
		_, _, err := g.switchOnce()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Switch: %v", err)
		}
	case <-time.After(10 * time.Second):
		// Unblock the vCPU so that cleanup can destroy it.
		_ = unix.Setrlimit(unix.RLIMIT_CPU, &old)
		<-done
		t.Fatal("the guest ran no instruction for 10 s under a soft RLIMIT_CPU")
	}
}
