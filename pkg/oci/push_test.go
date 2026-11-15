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

package oci

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
)

// basicRegistry answers every request without the right Basic credentials
// with a challenge. POST of an upload answers with location(r).
func basicRegistry(t *testing.T, location func(r *http.Request) string, put http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "u" || pass != "secret" {
			w.Header().Set("Www-Authenticate", `Basic realm="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
			w.Header().Set("Location", location(r))
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && put != nil:
			put(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func credentialsFor(t *testing.T, srv *httptest.Server) context.Context {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	ctx, err := WithCredentials(context.Background(), strings.TrimPrefix(srv.URL, "http://"), Credentials{Username: "u", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// TestPushSendsNoCredentialsToAnUploadHostOfAnotherOrigin proves that the
// Authorization header of the registry goes only to the registry. An upload
// Location can name another host, such as a storage service or a host that
// a hostile registry names. Basic credentials there leak the password.
func TestPushSendsNoCredentialsToAnUploadHostOfAnotherOrigin(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if h := r.Header.Get("Authorization"); h != "" {
			leaked = append(leaked, h)
		}
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(foreign.Close)
	reg := basicRegistry(t, func(*http.Request) string { return foreign.URL + "/upload/1?state=x" }, nil)
	ctx := credentialsFor(t, reg)
	rc, err := newRegistryClient(nil, reg.URL, "app", DefaultLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.close()
	data := []byte("layer data")
	if err := rc.uploadBlob(ctx, digest.FromBytes(data), strings.NewReader(string(data)), int64(len(data))); err != nil {
		t.Fatalf("uploadBlob: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("the upload host of another origin got the registry credentials: %q", leaked)
	}
}
