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
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// engineLogName is the log of serve and of its builder in the state
// directory.
const engineLogName = "engine.log"

// engineLogMaxSize is the size above which serve moves an old engine log to
// engine.log.1 when it starts.
const engineLogMaxSize = 10 << 20

// openEngineLog opens the engine log of stateDir and returns a writer that
// writes to the file and to stderr. The log moves to engine.log.1 when it
// passes engineLogMaxSize, also while serve runs. The returned function
// closes the file.
func openEngineLog(stateDir string, stderr io.Writer) (io.Writer, func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create state directory: %w", err)
	}
	f, err := openRotatingFile(filepath.Join(stateDir, engineLogName), engineLogMaxSize)
	if err != nil {
		return nil, nil, fmt.Errorf("open engine log: %w", err)
	}
	return io.MultiWriter(f, stderr), func() { _ = f.Close() }, nil
}

// rotatingFile is an append-only file that moves to path.1 before a write
// would take it past maxSize. It keeps one old file.
type rotatingFile struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	file    *os.File
	size    int64
}

// openRotatingFile opens path for appending. A file that is already larger
// than maxSize moves to path.1 first.
func openRotatingFile(path string, maxSize int64) (*rotatingFile, error) {
	r := &rotatingFile{path: path, maxSize: maxSize}
	if info, err := os.Stat(path); err == nil && info.Size() > maxSize {
		if err := os.Rename(path, path+".1"); err != nil {
			return nil, err
		}
	}
	if err := r.openLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) openLocked() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.file, r.size = f, info.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.maxSize {
		// A failed rotation keeps the current file: losing log lines is
		// worse than a log above its limit.
		if err := r.file.Close(); err == nil {
			_ = os.Rename(r.path, r.path+".1")
			if err := r.openLocked(); err != nil {
				return 0, err
			}
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}

// logRequests logs one line for each API request after the handler ends:
// the method, the path, the status, and the time. The line holds the error
// message of a response with status 400 or more. It never logs headers or
// the query string, which can hold registry credentials and build arguments.
// Pings are not logged; the CLI sends one before most commands.
func logRequests(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", rec.statusCode(), "duration", time.Since(start).Round(time.Millisecond)}
		if rec.hijacked {
			attrs = append(attrs, "hijacked", true)
		}
		level := slog.LevelInfo
		if rec.statusCode() >= http.StatusBadRequest {
			level = slog.LevelWarn
			attrs = append(attrs, "error", strings.TrimSpace(rec.errBody.String()))
		}
		log.Log(r.Context(), level, "api", attrs...)
	})
}

// statusRecorder records the status of a response and the start of its body
// for an error. It passes Flush and Hijack through, so streaming endpoints
// and attach keep working.
type statusRecorder struct {
	http.ResponseWriter
	status   int
	hijacked bool
	errBody  strings.Builder
}

// maxLoggedError bounds the error text that the request log keeps.
const maxLoggedError = 512

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.status >= http.StatusBadRequest && r.errBody.Len() < maxLoggedError {
		r.errBody.Write(p[:min(len(p), maxLoggedError-r.errBody.Len())])
	}
	return r.ResponseWriter.Write(p)
}

func (r *statusRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the response does not support hijack")
	}
	conn, rw, err := hj.Hijack()
	if err == nil {
		r.hijacked = true
		if r.status == 0 {
			r.status = http.StatusSwitchingProtocols
		}
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach the original writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// supervisorLogName is the log of the supervisor of a container, in the
// directory of the container. It holds the runs, their exit codes, and the
// errors of the supervisor and of the shim before the guest starts.
const supervisorLogName = "supervisor.log"

// openSupervisorLog opens the supervisor log of a container for appending.
func openSupervisorLog(store *containerStore, id string) (*os.File, error) {
	return os.OpenFile(filepath.Join(store.dir(id), supervisorLogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
