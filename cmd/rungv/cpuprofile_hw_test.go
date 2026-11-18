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

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"z.gfire.dev/gvx/internal/hvtest"
	"z.gfire.dev/gvx/internal/testelf"
)

// TestRunWritesACPUProfileWithGVXCPUPROFILE runs a guest with GVX_CPUPROFILE
// set. The guest must finish, and the file must hold a profile.
func TestRunWritesACPUProfileWithGVXCPUPROFILE(t *testing.T) {
	hvtest.Require(t)
	elfData, err := testelf.BuildGuestELF64(runtime.GOARCH, 0, "profiled-cli\n")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "cpu.pprof")
	t.Setenv("GVX_CPUPROFILE", out)
	code, stdout, stderr := runCLI(t, "run", "-lockdown=false", "-net=none", "-cache-dir="+t.TempDir(), writeELF(t, elfData))
	if code != 0 || stdout != "profiled-cli\n" {
		t.Fatalf("rungv run = (code %d, stdout %q, stderr %q), want (0, %q)", code, stdout, stderr, "profiled-cli\n")
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("GVX_CPUPROFILE file: %v", err)
	}
	if st.Size() == 0 {
		t.Fatalf("GVX_CPUPROFILE file is empty")
	}
}
