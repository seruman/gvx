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
	"fmt"

	"z.gfire.dev/gvx/internal/xsys/unix"
)

// ErrCPULimit reports a finite hard RLIMIT_CPU, which stops every guest.
var ErrCPULimit = errors.New("hvf: a finite RLIMIT_CPU (ulimit -t) stops every guest instruction; run without a CPU time limit")

// liftCPULimit removes a finite soft RLIMIT_CPU before the VM exists.
//
// XNU checks the CPU timers of a task at every exception entry, and a finite
// RLIMIT_CPU, ITIMER_PROF, or ITIMER_VIRTUAL turns that check on. While it is
// on, hv_vcpu_run executes no guest instruction: it returns only when
// hv_vcpus_exit cancels it. A soft limit rises to the hard limit, which an
// unprivileged process may do. The Go runtime ignores SIGXCPU, so the soft
// limit had no effect on the Sentry before. A finite hard limit cannot rise,
// so it fails with ErrCPULimit instead of a hang.
func liftCPULimit(get, set func(*unix.Rlimit) error) error {
	var l unix.Rlimit
	if err := get(&l); err != nil {
		return fmt.Errorf("hvf: read RLIMIT_CPU: %w", err)
	}
	inf := uint64(unix.RLIM_INFINITY)
	if l.Cur == inf {
		return nil
	}
	if l.Max != inf {
		return fmt.Errorf("%w (soft %d s, hard %d s)", ErrCPULimit, l.Cur, l.Max)
	}
	if err := set(&unix.Rlimit{Cur: inf, Max: inf}); err != nil {
		return fmt.Errorf("hvf: raise RLIMIT_CPU: %w", err)
	}
	return nil
}

func liftProcessCPULimit() error {
	return liftCPULimit(
		func(l *unix.Rlimit) error { return unix.Getrlimit(unix.RLIMIT_CPU, l) },
		func(l *unix.Rlimit) error { return unix.Setrlimit(unix.RLIMIT_CPU, l) },
	)
}
