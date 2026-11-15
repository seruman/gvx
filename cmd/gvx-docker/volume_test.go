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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
