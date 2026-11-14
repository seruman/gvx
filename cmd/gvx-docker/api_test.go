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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"z.gfire.dev/gvx/pkg/oci"
)

type fakeImages struct {
	images []oci.Image
	loaded string
}

func (f *fakeImages) ListImages(string) ([]oci.Image, error) { return f.images, nil }

func (f *fakeImages) Select(string, string) (oci.Selection, error) {
	return oci.Selection{Images: f.images}, nil
}

func (f *fakeImages) LoadArchive(r io.Reader, _ string) ([]oci.LoadedImage, error) {
	b, err := io.ReadAll(r)
	f.loaded = string(b)
	return []oci.LoadedImage{{ID: "sha256:abc"}}, err
}

func (f *fakeImages) Pull(context.Context, string, string) (string, error) {
	return "sha256:manifest", nil
}

type noBuilder struct{}

func (noBuilder) ensure(context.Context) error { return nil }

func TestPingAcceptsVersionedPath(t *testing.T) {
	s := &server{images: &fakeImages{}, builder: noBuilder{}, arch: "arm64"}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1.47/_ping", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
		t.Fatalf("ping = %d %q, want 200 OK", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Api-Version"); got != apiVersion {
		t.Errorf("Api-Version = %q, want %q", got, apiVersion)
	}
}

func TestImageListUsesDockerShape(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	images := &fakeImages{images: []oci.Image{{
		Repository: "library/alpine", BaseURL: "https://registry-1.docker.io",
		Digest: "sha256:manifest", ID: "sha256:config", Tags: []string{"3.20"}, Created: created, Size: 42,
	}}}
	s := &server{images: images, builder: noBuilder{}, arch: "arm64"}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/images/json", nil))
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode images: %v", err)
	}
	if len(got) != 1 || got[0]["Id"] != "sha256:config" || got[0]["Created"] != float64(created.Unix()) {
		t.Fatalf("images = %#v", got)
	}
	if tags := got[0]["RepoTags"].([]any); len(tags) != 1 || tags[0] != "alpine:3.20" {
		t.Errorf("RepoTags = %#v, want alpine:3.20", tags)
	}
}

func TestImageLoadPassesArchiveToStore(t *testing.T) {
	images := &fakeImages{}
	s := &server{images: images, builder: noBuilder{}, arch: "arm64"}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/images/load", strings.NewReader("archive")))
	if rec.Code != http.StatusOK || images.loaded != "archive" {
		t.Fatalf("load = %d with archive %q", rec.Code, images.loaded)
	}
	if !strings.Contains(rec.Body.String(), "Loaded image ID: sha256:abc") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestImageDefaultsFollowDockerEntrypointRules(t *testing.T) {
	cfg := containerConfig{Env: []string{"A=override", "B=new"}}
	applyImageDefaults(&cfg, []string{"/entry"}, []string{"default"}, []string{"A=image", "PATH=/bin"}, "/app", "app")
	if strings.Join(cfg.Entrypoint, " ") != "/entry" || strings.Join(cfg.Cmd, " ") != "default" {
		t.Fatalf("command = %q %q, want image entrypoint and cmd", cfg.Entrypoint, cfg.Cmd)
	}
	if strings.Join(cfg.Env, ",") != "A=override,PATH=/bin,B=new" {
		t.Errorf("env = %q", cfg.Env)
	}

	explicit := containerConfig{Entrypoint: []string{"/other"}}
	applyImageDefaults(&explicit, []string{"/entry"}, []string{"default"}, nil, "", "")
	if explicit.Cmd != nil {
		t.Errorf("cmd with explicit entrypoint = %q, want none", explicit.Cmd)
	}
}

func TestDefaultNetworkModeAttachesBridge(t *testing.T) {
	dir := t.TempDir()
	s := &server{containers: newContainerStore(dir), networks: newNetworkStore(dir), stateDir: dir}
	rec := containerRecord{ID: strings.Repeat("a", 64)}
	cfg := networkingConfig{EndpointsConfig: map[string]struct {
		Aliases []string `json:"Aliases"`
	}{"default": {}}}
	if err := s.containerNetworks(&rec, "default", cfg); err != nil {
		t.Fatalf("containerNetworks: %v", err)
	}
	// The store assigns the address when it writes the record.
	if err := s.containers.create(rec); err != nil {
		t.Fatal(err)
	}
	rec, err := s.containers.get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ep, ok := rec.Networks[defaultNetworkName]; !ok || ep.IPAddress != "172.17.0.2" {
		t.Fatalf("networks = %+v, want bridge at 172.17.0.2", rec.Networks)
	}
}

func TestResolveNameNeedsSharedNetwork(t *testing.T) {
	self := containerRecord{ID: strings.Repeat("a", 64), Networks: map[string]endpointSettings{"app": {NetworkID: "n1", IPAddress: "172.18.0.2"}}}
	peer := containerRecord{ID: strings.Repeat("b", 64), Name: "db", State: containerState{Running: true},
		Config:   containerConfig{Labels: map[string]string{composeServiceKey: "database"}},
		Networks: map[string]endpointSettings{"app": {NetworkID: "n1", IPAddress: "172.18.0.3"}}}
	other := containerRecord{ID: strings.Repeat("c", 64), Name: "cache", State: containerState{Running: true},
		Networks: map[string]endpointSettings{"other": {NetworkID: "n2", IPAddress: "172.19.0.2"}}}
	all := []containerRecord{self, peer, other}
	for _, name := range []string{"db", "database", "DB."} {
		if got := resolveName(self, all, name); len(got) != 1 || got[0].String() != "172.18.0.3" {
			t.Errorf("resolveName(%q) = %v, want 172.18.0.3", name, got)
		}
	}
	if got := resolveName(self, all, "cache"); len(got) != 0 {
		t.Errorf("resolveName(cache) = %v, want none across networks", got)
	}
}

func TestLabelFiltersMustAllMatch(t *testing.T) {
	db := containerRecord{Config: containerConfig{Labels: map[string]string{"project": "p", "service": "db"}}}
	app := containerRecord{Config: containerConfig{Labels: map[string]string{"project": "p", "service": "app"}}}
	filters := map[string][]string{"label": {"project=p", "service=db"}}
	if !matchesFilters(db, filters) {
		t.Error("db does not match its own labels")
	}
	if matchesFilters(app, filters) {
		t.Error("app matches the label filter of db")
	}
}

func TestEventsFollowContainerLifecycle(t *testing.T) {
	rec := containerRecord{}
	started := time.Unix(10, 0)
	finished := time.Unix(20, 0)
	created := observedState{exists: true}
	running := observedState{exists: true, running: true, startedAt: started, health: "starting"}
	healthy := running
	healthy.health = "healthy"
	exited := observedState{exists: true, startedAt: started, finishedAt: finished}
	cases := []struct {
		name          string
		before, after observedState
		want          string
	}{
		{"create", observedState{}, created, "create"},
		{"start", created, running, "start"},
		{"healthy", running, healthy, "health_status: healthy"},
		{"die", healthy, exited, "die"},
	}
	for _, tc := range cases {
		got := strings.Join(diffEvents(rec, tc.before, tc.after), ",")
		if got != tc.want {
			t.Errorf("%s: events = %q, want %q", tc.name, got, tc.want)
		}
	}
}
