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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// syncBuffer is a bytes.Buffer that the handler and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAPIRequestsAreLogged sends requests through the request log of serve.
// Each request gets one line with its method, path, and status. A failed
// request also logs the error text. Headers and the query string stay out
// of the log, because they can hold credentials and build arguments.
func TestAPIRequestsAreLogged(t *testing.T) {
	var out syncBuffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	s := newServer(nil, noBuilder{}, newContainerStore(t.TempDir()), nil, t.TempDir())
	h := logRequests(s, log)

	req := httptest.NewRequest(http.MethodGet, "/v1.47/containers/nosuch/json?size=secretquery", nil)
	req.Header.Set("X-Registry-Auth", "c2VjcmV0aGVhZGVy")
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1.47/version", nil))

	got := out.String()
	for _, want := range []string{
		"method=GET path=/v1.47/containers/nosuch/json status=404",
		"No such container",
		"method=GET path=/v1.47/version status=200",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("request log lacks %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"secretquery", "c2VjcmV0aGVhZGVy"} {
		if strings.Contains(got, secret) {
			t.Errorf("request log holds %q:\n%s", secret, got)
		}
	}
}

// TestPingIsNotLogged keeps the request log readable: the Docker CLI sends a
// ping before most commands.
func TestPingIsNotLogged(t *testing.T) {
	var out syncBuffer
	h := logRequests(newServer(nil, noBuilder{}, nil, nil, t.TempDir()), slog.New(slog.NewTextHandler(&out, nil)))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, "/_ping", nil))
	if got := out.String(); got != "" {
		t.Fatalf("a ping wrote %q to the request log", got)
	}
}

// TestRequestLogKeepsStreamingAndHijack proves that the request log keeps
// the interfaces that logs -f, events, attach, and exec need.
func TestRequestLogKeepsStreamingAndHijack(t *testing.T) {
	var flushed, hijackable bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flushed = w.(http.Flusher)
		_, hijackable = w.(http.Hijacker)
	})
	srv := httptest.NewServer(logRequests(inner, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !flushed || !hijackable {
		t.Fatalf("handler saw Flusher %v and Hijacker %v, want both", flushed, hijackable)
	}
}

// TestEngineLogTeesToTheFile opens the engine log of serve. Lines reach both
// the state directory file and stderr, and a large old log moves aside.
func TestEngineLogTeesToTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "engine.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), engineLogMaxSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr syncBuffer
	w, closeLog, err := openEngineLog(dir, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello engine\n")); err != nil {
		t.Fatal(err)
	}
	closeLog()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello engine\n" {
		t.Fatalf("engine.log = %q, want only the new line", data)
	}
	if !strings.Contains(stderr.String(), "hello engine") {
		t.Fatalf("stderr = %q, want the line", stderr.String())
	}
	if old, err := os.Stat(path + ".1"); err != nil || old.Size() != engineLogMaxSize+1 {
		t.Fatalf("engine.log.1 = %v, %v; want the old log", old, err)
	}
}
