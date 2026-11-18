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
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"

	"z.gfire.dev/gvx/internal/cpuprofile"
	"z.gfire.dev/gvx/internal/testelf"
)

// TestGuestRunsDuringACPUProfile runs a guest while the process writes a Go
// CPU profile. With the ITIMER_PROF timer of runtime/pprof armed, the vCPU
// runs no guest instruction, so the guest never ends. The profile must also
// hold samples of the Go code that burns CPU meanwhile.
func TestGuestRunsDuringACPUProfile(t *testing.T) {
	elf, err := testelf.BuildGuestELF64At(runtime.GOARCH, staticBase(), 0, "profiled\n")
	if err != nil {
		t.Fatal(err)
	}
	var profile bytes.Buffer
	p, err := cpuprofile.Start(&profile)
	if err != nil {
		t.Fatalf("cpuprofile.Start: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			p.Stop()
		}
	}()

	cfg := hostTestConfig(t)
	cfg.ELFBytes = elf
	var stdout, stderr bytes.Buffer
	cfg.Stdout, cfg.Stderr = &stdout, &stderr
	sb, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, err := sb.Run(ctx)
	if err != nil || code != 0 || stdout.String() != "profiled\n" {
		t.Fatalf("Run = (%d, %v), stdout %q, stderr %q; want (0, nil) and %q", code, err, stdout.String(), stderr.String(), "profiled\n")
	}

	// Burn CPU in Go so that the profile has samples to hold.
	end := time.Now().Add(300 * time.Millisecond)
	for x := 0; time.Now().Before(end); x++ {
		_ = x * x
	}
	stats := p.Stop()
	stopped = true
	if stats.Signals == 0 {
		t.Errorf("the sampler sent no SIGPROF while the test burned 300 ms of CPU")
	}
	if profile.Len() == 0 {
		t.Errorf("the CPU profile is empty")
	}
}
