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

//go:build darwin || windows

package gvx

import (
	"runtime"
	"testing"
	"time"

	"z.gfire.dev/gvx/internal/acceptprog"
)

// TestGuestClockUsesTheVDSO runs a guest that reads the time in a loop. The
// guest must read it through the vDSO, without a system call. Its time must
// match the host clock and advance at the rate of the host clock.
func TestGuestClockUsesTheVDSO(t *testing.T) {
	requireGuestBackend(t)
	cfg := hostTestConfig(t)
	cfg.BinaryPath = acceptprog.BuildClockProbe(t, runtime.GOARCH)
	code, stdout, stderr := runConfig(t, cfg)
	if code != 0 {
		t.Fatalf("clockprobe exited %d: %s", code, stderr)
	}
	checkClockProbe(t, stdout, time.Now().UnixNano(), true)
}
