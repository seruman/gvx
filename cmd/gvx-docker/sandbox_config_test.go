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

//go:build !windows

package main

import (
	"path/filepath"
	"testing"
)

// TestSandboxConfigKeepsTheRootWritesInTheContainerImage verifies that a
// container keeps its root writes in its own ext4lite image. A read-only
// root writes nothing, so it gets no image: the gvx root volume refuses
// ReadOnlyRoot.
func TestSandboxConfigKeepsTheRootWritesInTheContainerImage(t *testing.T) {
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	for _, tc := range []struct {
		name     string
		readOnly bool
		want     string
	}{
		{name: "writable", want: rootfs},
		{name: "read-only", readOnly: true, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec containerRecord
			rec.ID = "0123456789abcdef"
			rec.Config.Image = "sha256:0123"
			rec.HostConfig.ReadonlyRootfs = tc.readOnly
			cfg, err := sandboxConfig(rec, rootfs, "")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RootVolume != tc.want {
				t.Fatalf("RootVolume = %q, want %q", cfg.RootVolume, tc.want)
			}
			if cfg.ReadOnlyRoot != tc.readOnly {
				t.Fatalf("ReadOnlyRoot = %v, want %v", cfg.ReadOnlyRoot, tc.readOnly)
			}
		})
	}
}
