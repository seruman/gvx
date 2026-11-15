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
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestSetupAliasesKeepUserBuildersAndRemoveCleanly runs the alias step of
// setup against a Docker config that has a builder named default of its own.
// setup must keep that file byte for byte, write only the files that are
// free, record them, and setup -remove must delete exactly those files.
func TestSetupAliasesKeepUserBuildersAndRemoveCleanly(t *testing.T) {
	dockerDir := t.TempDir()
	stateDir := t.TempDir()
	instances := filepath.Join(dockerDir, "buildx", "instances")
	if err := os.MkdirAll(instances, 0o700); err != nil {
		t.Fatal(err)
	}
	gvxBuilder := []byte(`{"Name":"gvx-buildkit","Driver":"remote","Nodes":[{"Name":"gvx-buildkit0","Endpoint":"unix:///x/buildkit.sock"}]}`)
	userDefault := []byte(`{"Name":"default","Driver":"docker-container","Nodes":[{"Name":"default0","Endpoint":"desktop-linux"}]}`)
	if err := os.WriteFile(filepath.Join(instances, builderName), gvxBuilder, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instances, "default"), userDefault, 0o600); err != nil {
		t.Fatal(err)
	}
	before := listDir(t, instances)

	if err := writeBuilderAliases(dockerDir, stateDir, io.Discard); err != nil {
		t.Fatalf("writeBuilderAliases: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(instances, "default")); !bytes.Equal(got, userDefault) {
		t.Errorf("setup changed the builder default of the user: %s", got)
	}
	if _, err := os.Stat(filepath.Join(instances, contextName)); err != nil {
		t.Errorf("setup did not write the free alias %s: %v", contextName, err)
	}
	// A second setup must not take over the user file either, and must
	// keep its own alias.
	if err := writeBuilderAliases(dockerDir, stateDir, io.Discard); err != nil {
		t.Fatalf("second writeBuilderAliases: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(instances, "default")); !bytes.Equal(got, userDefault) {
		t.Errorf("a second setup changed the builder default of the user: %s", got)
	}

	if err := removeBuilderAliases(dockerDir, stateDir, io.Discard); err != nil {
		t.Fatalf("removeBuilderAliases: %v", err)
	}
	if got := listDir(t, instances); !equalStrings(got, before) {
		t.Errorf("instances after -remove = %v, want %v", got, before)
	}
	if got, _ := os.ReadFile(filepath.Join(instances, "default")); !bytes.Equal(got, userDefault) {
		t.Errorf("-remove changed the builder default of the user: %s", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
