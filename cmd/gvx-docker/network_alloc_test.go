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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"z.gfire.dev/gvx/pkg/oci"
)

// TestParallelCreatesGetDistinctAddresses creates containers on one network
// at once, as Compose does for the services of one level. Each container must
// get its own address.
func TestParallelCreatesGetDistinctAddresses(t *testing.T) {
	dir := t.TempDir()
	s := &server{
		images:     &fakeImages{images: []oci.Image{{ID: "sha256:img"}}},
		builder:    noBuilder{},
		containers: newContainerStore(dir),
		networks:   newNetworkStore(dir),
		stateDir:   dir,
		arch:       "arm64",
	}
	if _, err := s.networks.create("app", nil, false); err != nil {
		t.Fatal(err)
	}
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"Image":"img","HostConfig":{"NetworkMode":"app"}}`
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/containers/create?name=c%d", i), strings.NewReader(body))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Errorf("create c%d = %d %s", i, rec.Code, rec.Body.String())
			}
		}(i)
	}
	wg.Wait()
	assertDistinctAddresses(t, s, "app", n)
}

// TestParallelConnectsGetDistinctAddresses connects containers to one network
// at once, as docker network connect does.
func TestParallelConnectsGetDistinctAddresses(t *testing.T) {
	dir := t.TempDir()
	s := &server{
		images:     &fakeImages{images: []oci.Image{{ID: "sha256:img"}}},
		builder:    noBuilder{},
		containers: newContainerStore(dir),
		networks:   newNetworkStore(dir),
		stateDir:   dir,
		arch:       "arm64",
	}
	if _, err := s.networks.create("side", nil, false); err != nil {
		t.Fatal(err)
	}
	const n = 16
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/containers/create?name=c%d", i), strings.NewReader(`{"Image":"img"}`))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create c%d = %d %s", i, rec.Code, rec.Body.String())
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"Container":"c%d"}`, i)
			req := httptest.NewRequest(http.MethodPost, "/networks/side/connect", strings.NewReader(body))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("connect c%d = %d %s", i, rec.Code, rec.Body.String())
			}
		}(i)
	}
	wg.Wait()
	assertDistinctAddresses(t, s, "side", n)
}

func assertDistinctAddresses(t *testing.T, s *server, network string, want int) {
	t.Helper()
	recs, err := s.containers.list()
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string]string{}
	for _, c := range recs {
		ep, ok := c.Networks[network]
		if !ok {
			t.Errorf("%s has no endpoint on %s", c.Name, network)
			continue
		}
		if other, dup := owner[ep.IPAddress]; dup {
			t.Errorf("%s and %s share the address %s", other, c.Name, ep.IPAddress)
		}
		owner[ep.IPAddress] = c.Name
	}
	if len(owner) != want {
		raw, _ := json.Marshal(owner)
		t.Fatalf("%d distinct addresses on %s, want %d: %s", len(owner), network, want, raw)
	}
}
