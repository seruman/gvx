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
	"io"
	"slices"
	"testing"
)

// TestBuilderReachesHostLoopbackOnlyWithTheFlag checks the builder sandbox
// config. RUN steps of a build share the network of the builder, so with
// LoopbackToHost any Dockerfile reaches the services that listen only on the
// loopback of the Mac. That needs the serve flag -builder-host-loopback, and
// the builder process must get the flag from serve.
func TestBuilderReachesHostLoopbackOnlyWithTheFlag(t *testing.T) {
	p := paths{stateDir: t.TempDir()}
	p.normalize()
	if cfg := buildKitConfig(p, io.Discard); cfg.LoopbackToHost {
		t.Error("the builder reaches the host loopback without -builder-host-loopback")
	}
	p.builderHostLoopback = true
	if cfg := buildKitConfig(p, io.Discard); !cfg.LoopbackToHost {
		t.Error("the builder does not reach the host loopback with -builder-host-loopback")
	}
	b := &builderProcess{exe: "/bin/true", paths: p}
	if args := b.builderArgs(); !slices.Contains(args, "-builder-host-loopback") {
		t.Errorf("builder arguments %q lack -builder-host-loopback", args)
	}
	p.builderHostLoopback = false
	b = &builderProcess{exe: "/bin/true", paths: p}
	if args := b.builderArgs(); slices.Contains(args, "-builder-host-loopback") {
		t.Errorf("builder arguments %q have -builder-host-loopback without the flag", args)
	}
}
