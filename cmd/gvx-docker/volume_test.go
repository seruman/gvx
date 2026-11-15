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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"z.gfire.dev/gvx/pkg/oci"
)

// TestVolumeHandlersRefuseNamesOutsideTheVolumeDirectory sends a volume name
// that climbs out of the volume directory, as an encoded path element. The
// handlers must refuse it, and the directory outside must stay.
func TestVolumeHandlersRefuseNamesOutsideTheVolumeDirectory(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "volume.json"), []byte(`{"Name":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{containers: newContainerStore(state), networks: newNetworkStore(state), stateDir: state}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(method, "/volumes/..%2F..%2Fvictim", nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s of a name outside the volume directory = %d %s, want 400", method, rec.Code, rec.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(victim, "volume.json")); err != nil {
		t.Fatalf("a volume request removed a directory outside the volume directory: %v", err)
	}
}

// TestAnonymousVolumesGoWithTheirContainer creates containers whose image
// declares a VOLUME and whose request mounts a volume without a name. Each
// gets anonymous volumes. docker rm -v, container prune, and --rm must remove
// them, as Docker does; a named volume stays.
func TestAnonymousVolumesGoWithTheirContainer(t *testing.T) {
	state := t.TempDir()
	s := &server{
		images:     &fakeImages{images: []oci.Image{{ID: "sha256:img", Config: oci.RuntimeConfig{Volumes: map[string]struct{}{"/data": {}}}}}},
		builder:    noBuilder{},
		containers: newContainerStore(state),
		networks:   newNetworkStore(state),
		stateDir:   state,
		arch:       "arm64",
	}
	create := func(name string) string {
		t.Helper()
		body := `{"Image":"img","HostConfig":{"NetworkMode":"none","Binds":["keep:/keep"],"Mounts":[{"Type":"volume","Target":"/cache"}]}}`
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/containers/create?name="+name, strings.NewReader(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, rec.Code, rec.Body.String())
		}
		var out struct{ Id string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Id
	}
	volumes := func() []string {
		entries, _ := os.ReadDir(s.volumeRoot())
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}

	create("withv")
	if got := len(volumes()); got != 3 {
		t.Fatalf("volumes after create = %v, want keep and two anonymous ones", volumes())
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/containers/withv?v=1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("rm -v = %d %s", rec.Code, rec.Body.String())
	}
	if got := volumes(); len(got) != 1 || got[0] != "keep" {
		t.Errorf("volumes after rm -v = %v, want only keep", got)
	}

	create("pruned")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/containers/prune", nil))
	if got := volumes(); len(got) != 1 || got[0] != "keep" {
		t.Errorf("volumes after container prune = %v, want only keep", got)
	}

	id := create("autorm")
	if err := s.containers.removeContainerAndAnonymousVolumes(id, s.volumeRoot(), 0); err != nil {
		t.Fatalf("auto remove: %v", err)
	}
	if got := volumes(); len(got) != 1 || got[0] != "keep" {
		t.Errorf("volumes after --rm = %v, want only keep", got)
	}
}

// TestCreateAcceptsANameThatIsAnIDPrefix creates a container whose name is a
// prefix of the ID of another container. Hex words such as "add", "cafe", or
// "c15" are valid names, and only an equal name is a conflict.
func TestCreateAcceptsANameThatIsAnIDPrefix(t *testing.T) {
	store := newContainerStore(t.TempDir())
	other := containerRecord{ID: "cafe" + strings.Repeat("0", 60), Name: "other", Created: time.Now()}
	if err := store.create(other); err != nil {
		t.Fatal(err)
	}
	if err := store.create(containerRecord{ID: strings.Repeat("1", 64), Name: "cafe", Created: time.Now()}); err != nil {
		t.Fatalf("create of the name cafe = %v, want success: it only matches the ID prefix of another container", err)
	}
	if err := store.create(containerRecord{ID: strings.Repeat("2", 64), Name: "other", Created: time.Now()}); err == nil {
		t.Fatal("create of a name in use succeeded")
	}
}

// TestParallelRenamesToOneNameConflict renames two containers to one name at
// once. One rename must fail with 409, so no two containers share a name.
func TestParallelRenamesToOneNameConflict(t *testing.T) {
	state := t.TempDir()
	s := &server{containers: newContainerStore(state), networks: newNetworkStore(state), stateDir: state}
	for i, name := range []string{"first", "second"} {
		id := strings.Repeat(string(rune('a'+i)), 64)
		if err := s.containers.create(containerRecord{ID: id, Name: name, Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	s.beforeRename = func() { time.Sleep(100 * time.Millisecond) }
	codes := make(chan int, 2)
	for _, name := range []string{"first", "second"} {
		go func(name string) {
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/containers/"+name+"/rename?name=shared", nil))
			codes <- rec.Code
		}(name)
	}
	got := []int{<-codes, <-codes}
	records, err := s.containers.list()
	if err != nil {
		t.Fatal(err)
	}
	shared := 0
	for _, r := range records {
		if r.Name == "shared" {
			shared++
		}
	}
	if shared != 1 {
		t.Fatalf("%d containers have the name shared after two parallel renames (codes %v), want 1", shared, got)
	}
}

// TestAnonymousVolumeSharedByTwoContainersStaysWhileUsed mounts the
// anonymous volume of one container into a second container by name, as
// Compose does when it recreates a service. rm -v of the first container
// must keep the volume that the second still mounts, and rm -v of the second
// must remove it: the volume stays anonymous.
func TestAnonymousVolumeSharedByTwoContainersStaysWhileUsed(t *testing.T) {
	state := t.TempDir()
	s := &server{
		images:     &fakeImages{images: []oci.Image{{ID: "sha256:img", Config: oci.RuntimeConfig{Volumes: map[string]struct{}{"/data": {}}}}}},
		builder:    noBuilder{},
		containers: newContainerStore(state),
		networks:   newNetworkStore(state),
		stateDir:   state,
		arch:       "arm64",
	}
	create := func(name, body string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/containers/create?name="+name, strings.NewReader(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, rec.Code, rec.Body.String())
		}
		var out struct{ Id string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Id
	}
	firstID := create("first", `{"Image":"img","HostConfig":{"NetworkMode":"none"}}`)
	first, err := s.containers.get(firstID)
	if err != nil || len(first.HostConfig.Binds) != 1 {
		t.Fatalf("first has binds %v (%v), want the one of its VOLUME", first.HostConfig.Binds, err)
	}
	src, _, _ := strings.Cut(first.HostConfig.Binds[0], ":")
	vol, ok := volumeNameOf(src, s.volumeRoot())
	if !ok {
		t.Fatalf("the VOLUME bind %q names no volume", src)
	}
	create("second", `{"Image":"img","HostConfig":{"NetworkMode":"none","Binds":["`+vol+`:/data"]}}`)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/containers/first?v=1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("rm -v first = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.volumeRoot(), vol)); err != nil {
		t.Fatalf("rm -v of the first container removed a volume that the second mounts: %v", err)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/containers/second?v=1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("rm -v second = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.volumeRoot(), vol)); !os.IsNotExist(err) {
		t.Fatalf("rm -v of the last user kept the anonymous volume: %v", err)
	}
}
