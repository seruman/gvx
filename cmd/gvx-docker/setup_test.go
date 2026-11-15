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
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

	if err := writeBuilderAliases(dockerDir, stateDir, "unix:///x/buildkit.sock", io.Discard); err != nil {
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
	if err := writeBuilderAliases(dockerDir, stateDir, "unix:///x/buildkit.sock", io.Discard); err != nil {
		t.Fatalf("second writeBuilderAliases: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(instances, "default")); !bytes.Equal(got, userDefault) {
		t.Errorf("a second setup changed the builder default of the user: %s", got)
	}

	if err := removeBuilderAliases(dockerDir, readSetupRecord(stateDir)); err != nil {
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

// fakeDocker records the docker commands of setup. A context inspect answers
// with host when hasContext is set. buildx create writes the instance file
// of the gvx builder, as the real command does.
type fakeDocker struct {
	dockerDir  string
	hasContext bool
	host       string
	calls      []string
}

func (f *fakeDocker) run(_ context.Context, stdout, _ io.Writer, args ...string) error {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case len(args) >= 2 && args[0] == "context" && args[1] == "inspect":
		if !f.hasContext {
			return errors.New("context not found")
		}
		_, _ = io.WriteString(stdout, f.host+"\n")
	case len(args) >= 2 && args[0] == "context" && args[1] == "create":
		f.hasContext = true
	case len(args) >= 2 && args[0] == "buildx" && args[1] == "create":
		dir := filepath.Join(f.dockerDir, "buildx", "instances")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, builderName), []byte(`{"Name":"gvx-buildkit","Driver":"remote","Nodes":[{"Name":"gvx-buildkit0","Endpoint":"`+args[len(args)-1]+`"}]}`), 0o600)
	}
	return nil
}

func (f *fakeDocker) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func setupPaths(t *testing.T) (paths, string) {
	t.Helper()
	dockerDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dockerDir)
	p := paths{stateDir: t.TempDir(), apiSocket: "/tmp/gvx-test/docker.sock"}
	p.normalize()
	return p, dockerDir
}

// TestSetupRemoveRestoresAContextOfTheUser runs setup when the user already
// has a context named gvx. setup -remove must restore its endpoint and not
// delete it.
func TestSetupRemoveRestoresAContextOfTheUser(t *testing.T) {
	p, dockerDir := setupPaths(t)
	f := &fakeDocker{dockerDir: dockerDir, hasContext: true, host: "unix:///user/engine.sock"}
	if err := setupDocker(context.Background(), p, f.run, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.host = "unix://" + p.apiSocket
	f.calls = nil
	if err := removeDockerSetup(context.Background(), p, f.run, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.ran("context rm") {
		t.Errorf("setup -remove deleted the context gvx of the user: %q", f.calls)
	}
	if !f.ran("context update gvx --docker host=unix:///user/engine.sock") {
		t.Errorf("setup -remove did not restore the endpoint of the user context: %q", f.calls)
	}
}

// TestSetupRemoveDeletesAContextThatSetupCreated runs setup without a gvx
// context. setup -remove must delete the context that setup created.
func TestSetupRemoveDeletesAContextThatSetupCreated(t *testing.T) {
	p, dockerDir := setupPaths(t)
	f := &fakeDocker{dockerDir: dockerDir}
	if err := setupDocker(context.Background(), p, f.run, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	// A second setup must not take the context of the first one for the
	// context of the user.
	if err := setupDocker(context.Background(), p, f.run, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := removeDockerSetup(context.Background(), p, f.run, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !f.ran("context rm --force gvx") {
		t.Errorf("setup -remove kept the context that setup created: %q", f.calls)
	}
}

// TestSetupRecordsAnAliasBeforeTheNextOne makes the write of the second
// alias fail. The first alias must be in the record already, so a later
// setup and setup -remove treat it as their own.
func TestSetupRecordsAnAliasBeforeTheNextOne(t *testing.T) {
	p, dockerDir := setupPaths(t)
	f := &fakeDocker{dockerDir: dockerDir}
	writeAliasFile = func(name string, data []byte, perm os.FileMode) error {
		if filepath.Base(name) == "default" {
			return errors.New("disk full")
		}
		return os.WriteFile(name, data, perm)
	}
	defer func() { writeAliasFile = os.WriteFile }()
	if err := setupDocker(context.Background(), p, f.run, io.Discard, io.Discard); err == nil {
		t.Fatal("setup succeeded with a failing alias write")
	}
	if got := readSetupRecord(p.stateDir).Aliases; len(got) != 1 || got[0] != contextName {
		t.Fatalf("record after a failed second alias = %v, want [%s]", got, contextName)
	}
}

// TestSetupTakesOverTheAliasesOfAnOlderSetup finds a default alias that an
// older setup wrote without a record: a remote builder on the BuildKit socket
// of this engine. setup must treat it as its own, and -remove must delete it.
func TestSetupTakesOverTheAliasesOfAnOlderSetup(t *testing.T) {
	p, dockerDir := setupPaths(t)
	instances := filepath.Join(dockerDir, "buildx", "instances")
	if err := os.MkdirAll(instances, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"Name":"default","Driver":"remote","Nodes":[{"Name":"default0","Endpoint":"unix://` + p.buildkitSock + `"}]}`
	if err := os.WriteFile(filepath.Join(instances, "default"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{dockerDir: dockerDir}
	if err := setupDocker(context.Background(), p, f.run, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := removeDockerSetup(context.Background(), p, f.run, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(instances, "default")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup -remove kept the default alias of an older setup: %v", err)
	}
}
