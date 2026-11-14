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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type execCreateRequest struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env"`
	WorkingDir   string   `json:"WorkingDir"`
	User         string   `json:"User"`
	ConsoleSize  []uint16 `json:"ConsoleSize"`
	DetachKeys   string   `json:"DetachKeys"`
}

type execInstance struct {
	ID          string
	ContainerID string
	Config      execCreateRequest

	mu       sync.Mutex
	running  bool
	exitCode *int
	conn     net.Conn
	writeMu  sync.Mutex
}

type execRegistry struct {
	mu    sync.Mutex
	execs map[string]*execInstance
}

func (r *execRegistry) add(e *execInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.execs == nil {
		r.execs = map[string]*execInstance{}
	}
	r.execs[e.ID] = e
}

func (r *execRegistry) get(id string) (*execInstance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.execs[id]
	return e, ok
}

func (s *server) routeExec(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/exec") && r.Method == http.MethodPost:
		s.createExec(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/exec"))
	case strings.HasPrefix(path, "/exec/"):
		id, action, _ := strings.Cut(strings.TrimPrefix(path, "/exec/"), "/")
		switch {
		case action == "start" && r.Method == http.MethodPost:
			s.startExec(w, r, id)
		case action == "resize" && r.Method == http.MethodPost:
			s.resizeExec(w, r, id)
		case action == "json" && r.Method == http.MethodGet:
			s.inspectExec(w, id)
		default:
			return false
		}
	default:
		return false
	}
	return true
}

func (s *server) createExec(w http.ResponseWriter, r *http.Request, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !rec.State.Running {
		writeError(w, http.StatusConflict, "Container "+ref+" is not running")
		return
	}
	var req execCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode exec config: "+err.Error())
		return
	}
	if len(req.Cmd) == 0 {
		writeError(w, http.StatusBadRequest, "exec has no command")
		return
	}
	id, err := newContainerID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.execs.add(&execInstance{ID: id, ContainerID: rec.ID, Config: req})
	writeJSON(w, http.StatusCreated, map[string]string{"Id": id})
}

func (s *server) startExec(w http.ResponseWriter, r *http.Request, id string) {
	e, ok := s.execs.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "No such exec instance: "+id)
		return
	}
	var start struct {
		Detach      bool     `json:"Detach"`
		Tty         bool     `json:"Tty"`
		ConsoleSize []uint16 `json:"ConsoleSize"`
	}
	_ = json.NewDecoder(r.Body).Decode(&start)
	spec := execSpec{Cmd: e.Config.Cmd, Env: e.Config.Env, WorkingDir: e.Config.WorkingDir, User: e.Config.User, Tty: e.Config.Tty || start.Tty}
	size := e.Config.ConsoleSize
	if len(start.ConsoleSize) == 2 {
		size = start.ConsoleSize
	}
	if len(size) == 2 {
		spec.Rows, spec.Cols = size[0], size[1]
	}
	ctrl, reader, err := controlCall(r.Context(), controlSocketPath(s.stateDir, e.ContainerID), controlRequest{Op: "exec", Exec: &spec})
	var notFound *controlNotFoundError
	if errors.As(err, &notFound) {
		// Docker starts the stream, prints the runtime error, and records
		// 127 for a missing command. Clients such as Testcontainers read the
		// code to fall back.
		s.failExec(w, e, spec.Tty, start.Detach, 127, fmt.Sprintf("OCI runtime exec failed: exec failed: unable to start container process: exec: %q: %v\r\n", spec.Cmd[0], notFound))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec in container: %v", err))
		return
	}
	e.mu.Lock()
	e.running = true
	e.conn = ctrl
	e.mu.Unlock()
	if start.Detach {
		_ = writeControlFrame(&e.writeMu, ctrl, frameEOF, nil)
		go s.finishExec(e, reader, io.Discard, spec.Tty)
		w.WriteHeader(http.StatusOK)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = ctrl.Close()
		writeError(w, http.StatusInternalServerError, "the connection cannot be hijacked")
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = ctrl.Close()
		return
	}
	defer func() { _ = client.Close() }()
	contentType := "application/vnd.docker.multiplexed-stream"
	if spec.Tty {
		contentType = "application/vnd.docker.raw-stream"
	}
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 101 UPGRADED\r\nContent-Type: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", contentType)
	_ = buf.Flush()
	var input io.Reader = buf.Reader
	if spec.Tty {
		keys, err := parseDetachKeys(e.Config.DetachKeys)
		if err != nil {
			keys = defaultDetachKeys
		}
		input = newDetachReader(buf.Reader, keys)
	}
	go func() {
		chunk := make([]byte, 32*1024)
		for {
			n, err := input.Read(chunk)
			if n > 0 && e.Config.AttachStdin {
				if writeControlFrame(&e.writeMu, ctrl, frameStdin, chunk[:n]) != nil {
					return
				}
			}
			if errors.Is(err, errDetached) {
				// The process keeps running, and its output goes nowhere, as
				// in Docker. Closing the client ends the writes to it.
				_ = client.Close()
				return
			}
			if err != nil {
				_ = writeControlFrame(&e.writeMu, ctrl, frameEOF, nil)
				return
			}
		}
	}()
	if !e.Config.AttachStdin {
		_ = writeControlFrame(&e.writeMu, ctrl, frameEOF, nil)
	}
	s.finishExec(e, reader, ignoreClosed{client}, spec.Tty)
}

// ignoreClosed drops writes after the client closed, so a detached exec runs
// until it exits.
type ignoreClosed struct{ w io.Writer }

func (i ignoreClosed) Write(p []byte) (int, error) {
	_, _ = i.w.Write(p)
	return len(p), nil
}

// failExec ends an exec that could not start with the exit code and the
// message that Docker gives for it.
func (s *server) failExec(w http.ResponseWriter, e *execInstance, tty, detach bool, code int, msg string) {
	e.mu.Lock()
	e.exitCode = &code
	e.running = false
	e.mu.Unlock()
	if detach {
		w.WriteHeader(http.StatusOK)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "the connection cannot be hijacked")
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	contentType := "application/vnd.docker.multiplexed-stream"
	if tty {
		contentType = "application/vnd.docker.raw-stream"
	}
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 101 UPGRADED\r\nContent-Type: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", contentType)
	if tty {
		_, _ = buf.WriteString(msg)
	} else {
		writeFrame(buf, frameStdout, []byte(msg))
	}
	_ = buf.Flush()
}

func (s *server) finishExec(e *execInstance, reader *bufio.Reader, client io.Writer, tty bool) {
	defer func() {
		e.mu.Lock()
		e.running = false
		if e.conn != nil {
			_ = e.conn.Close()
		}
		e.mu.Unlock()
	}()
	for {
		kind, payload, err := readControlFrame(reader)
		if err != nil {
			code := 126
			e.mu.Lock()
			if e.exitCode == nil {
				e.exitCode = &code
			}
			e.mu.Unlock()
			return
		}
		switch kind {
		case frameStdout, frameStderr:
			if tty {
				_, _ = client.Write(payload)
			} else {
				writeFrame(client, kind, payload)
			}
		case frameExit:
			code := int(binary.BigEndian.Uint32(payload))
			e.mu.Lock()
			e.exitCode = &code
			e.mu.Unlock()
			return
		}
	}
}

func (s *server) resizeExec(w http.ResponseWriter, r *http.Request, id string) {
	e, ok := s.execs.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "No such exec instance: "+id)
		return
	}
	rows, _ := strconv.Atoi(r.URL.Query().Get("h"))
	cols, _ := strconv.Atoi(r.URL.Query().Get("w"))
	e.mu.Lock()
	conn := e.conn
	e.mu.Unlock()
	if conn != nil {
		var size [4]byte
		binary.BigEndian.PutUint16(size[:2], uint16(rows))
		binary.BigEndian.PutUint16(size[2:], uint16(cols))
		_ = writeControlFrame(&e.writeMu, conn, frameResize, size[:])
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) inspectExec(w http.ResponseWriter, id string) {
	e, ok := s.execs.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "No such exec instance: "+id)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var code any
	if e.exitCode != nil {
		code = *e.exitCode
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ID": e.ID, "ContainerID": e.ContainerID, "Running": e.running, "ExitCode": code,
		"OpenStdin": e.Config.AttachStdin, "OpenStdout": e.Config.AttachStdout, "OpenStderr": e.Config.AttachStderr,
		"ProcessConfig": map[string]any{"tty": e.Config.Tty, "entrypoint": e.Config.Cmd[0], "arguments": e.Config.Cmd[1:], "user": e.Config.User},
		"Pid":           0,
	})
}

func (s *server) resizeContainer(w http.ResponseWriter, r *http.Request, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	rows, _ := strconv.Atoi(r.URL.Query().Get("h"))
	cols, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if !rec.State.Running {
		// docker run resizes before the start. The shim reads the size.
		_, _ = s.containers.update(rec.ID, func(cur *containerRecord) error {
			cur.ConsoleRows, cur.ConsoleCols = uint16(rows), uint16(cols)
			return nil
		})
		w.WriteHeader(http.StatusOK)
		return
	}
	conn, _, err := controlCall(r.Context(), controlSocketPath(s.stateDir, rec.ID), controlRequest{Op: "resize", Rows: uint16(rows), Cols: uint16(cols)})
	if err == nil {
		_ = conn.Close()
	}
	w.WriteHeader(http.StatusOK)
}
