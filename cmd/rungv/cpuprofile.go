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

package main

import (
	"fmt"
	"io"
	"os"

	"z.gfire.dev/gvx/internal/cpuprofile"
)

// startCPUProfile writes a Go CPU profile of rungv to the file that
// GVX_CPUPROFILE names, until the returned function runs. The file opens
// before any host lockdown.
func startCPUProfile(stderr io.Writer) func() {
	path := os.Getenv("GVX_CPUPROFILE")
	if path == "" {
		return func() {}
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(stderr, "rungv: GVX_CPUPROFILE: %v\n", err)
		return func() {}
	}
	p, err := cpuprofile.Start(f)
	if err != nil {
		fmt.Fprintf(stderr, "rungv: GVX_CPUPROFILE: %v\n", err)
		_ = f.Close()
		return func() {}
	}
	return func() {
		p.Stop()
		if err := f.Close(); err != nil {
			fmt.Fprintf(stderr, "rungv: GVX_CPUPROFILE: %v\n", err)
		}
	}
}
