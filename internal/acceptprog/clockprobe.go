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

package acceptprog

import "testing"

// clockProbePackage is the guest program that measures the guest clock.
const clockProbePackage = "z.gfire.dev/gvx/internal/acceptprog/clockprobe"

// BuildClockProbe compiles the guest clockprobe for the Linux architecture
// goarch and returns the path of the static executable.
func BuildClockProbe(tb testing.TB, goarch string) string {
	tb.Helper()
	return buildStaticGuest(tb, clockProbePackage, "clockprobe", goarch, 0)
}
