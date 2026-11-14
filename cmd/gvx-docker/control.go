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
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"z.gfire.dev/gvx/pkg/gvx"
)

// The control protocol runs on the Unix socket of a shim. A client sends one
// JSON request line. For "exec", the shim answers with one JSON line and then
// relays the process streams: the client sends input frames and the shim sends
// output frames. A frame has a one-byte kind and a four-byte big-endian
// length. The last frame carries the exit code.
const (
	frameStdin  byte = 0
	frameStdout byte = 1
	frameStderr byte = 2
	frameExit   byte = 3
	frameResize byte = 4
	frameEOF    byte = 5
)

type controlRequest struct {
	Op     string    `json:"op"`
	Exec   *execSpec `json:"exec,omitempty"`
	Rows   uint16    `json:"rows,omitempty"`
	Cols   uint16    `json:"cols,omitempty"`
	Signal int       `json:"signal,omitempty"`
	Port   uint16    `json:"port,omitempty"`
	Path   string    `json:"path,omitempty"`
	Proto  string    `json:"proto,omitempty"`
	// Src and Dst name the endpoints of a peer connection on a container
	// network, as "ip:port".
	Src string `json:"src,omitempty"`
	Dst string `json:"dst,omitempty"`
}

type execSpec struct {
	Cmd        []string `json:"Cmd"`
	Env        []string `json:"Env"`
	WorkingDir string   `json:"WorkingDir"`
	User       string   `json:"User"`
	Tty        bool     `json:"Tty"`
	Rows       uint16   `json:"Rows,omitempty"`
	Cols       uint16   `json:"Cols,omitempty"`
}

type controlResponse struct {
	OK       bool          `json:"ok"`
	Error    string        `json:"error,omitempty"`
	NotFound bool          `json:"notFound,omitempty"`
	Stat     *gvx.PathStat `json:"stat,omitempty"`
}

func controlSocketPath(stateDir, id string) string {
	return filepath.Join(stateDir, "run", id[:12]+".sock")
}

func listenControl(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create control directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// Close of a Go Unix listener unlinks its path by default. The shim
	// closes the listener after the lockdown, where unlinkat on AT_FDCWD is
	// denied: Seatbelt returns an error, and the Linux seccomp filter kills
	// the process with SIGSYS. The supervisor removes the file after the shim
	// exits.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("restrict %s: %w", path, err)
	}
	return ln, nil
}

func serveControlIO(sb *gvx.Sandbox, sio *shimIO) {
	for {
		conn, err := sb.AcceptControlConn()
		if err != nil {
			return
		}
		go handleControlConnIO(sb, sio, conn)
	}
}

// shimIO is the main process stdin of a shim. Attach requests write to it.
type shimIO struct {
	stdin *io.PipeWriter
	mu    sync.Mutex
}

func (s *shimIO) write(p []byte) error {
	if s == nil || s.stdin == nil {
		return errors.New("the container has no open stdin")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.stdin.Write(p)
	return err
}

func (s *shimIO) close() {
	if s != nil && s.stdin != nil {
		_ = s.stdin.Close()
	}
}

func handleControlConnIO(sb *gvx.Sandbox, sio *shimIO, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var req controlRequest
	if err := json.Unmarshal(line, &req); err != nil {
		writeControlResponse(conn, err)
		return
	}
	switch req.Op {
	case "dial":
		if req.Src != "" && req.Dst != "" {
			src, serr := netip.ParseAddrPort(req.Src)
			dst, derr := netip.ParseAddrPort(req.Dst)
			if serr != nil || derr != nil {
				writeControlResponse(conn, errors.Join(serr, derr))
				return
			}
			proto := req.Proto
			if proto == "" {
				proto = "tcp"
			}
			guest, err := sb.DialGuestFrom(context.Background(), proto, src, dst)
			writeControlResponse(conn, err)
			if err != nil {
				return
			}
			defer func() { _ = guest.Close() }()
			if proto == "udp" {
				relayFramedDatagrams(bufferedConn{Conn: conn, r: reader}, guest)
			} else {
				spliceConns(bufferedConn{Conn: conn, r: reader}, guest)
			}
			return
		}
		if req.Proto == "udp" {
			guest, err := sb.DialGuestUDP(req.Port)
			writeControlResponse(conn, err)
			if err != nil {
				return
			}
			defer func() { _ = guest.Close() }()
			relayFramedDatagrams(bufferedConn{Conn: conn, r: reader}, guest)
			return
		}
		guest, err := sb.DialGuestTCP(context.Background(), req.Port)
		writeControlResponse(conn, err)
		if err != nil {
			return
		}
		defer func() { _ = guest.Close() }()
		spliceConns(bufferedConn{Conn: conn, r: reader}, guest)
	case "stat":
		st, err := sb.StatPath(req.Path)
		resp := controlResponse{OK: err == nil, NotFound: errors.Is(err, gvx.ErrPathNotFound)}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Stat = &st
		}
		data, _ := json.Marshal(resp)
		_, _ = conn.Write(append(data, '\n'))
	case "archive-get":
		if _, err := sb.StatPath(req.Path); err != nil {
			resp := controlResponse{Error: err.Error(), NotFound: errors.Is(err, gvx.ErrPathNotFound)}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
			return
		}
		writeControlResponse(conn, nil)
		_ = sb.ArchiveRead(req.Path, conn)
	case "archive-put":
		writeControlResponse(conn, nil)
		err := sb.ArchiveWrite(req.Path, reader)
		writeControlResponse(conn, err)
	case "stdin":
		// The client sends the input and then closes its write side. With
		// Signal set, the end of the input closes the stdin of the guest, as
		// docker run -i does with StdinOnce.
		writeControlResponse(conn, nil)
		buf := make([]byte, 32*1024)
		for {
			n, err := reader.Read(buf)
			if n > 0 && sio.write(buf[:n]) != nil {
				return
			}
			if err != nil {
				// A client that ends the input closes only its write side
				// and still reads, so the reply byte reaches it. A client
				// that detached closed the whole connection: the write
				// fails, and the stdin of the guest stays open.
				if req.Signal == 1 {
					if _, werr := conn.Write([]byte{0}); werr == nil {
						sio.close()
					}
				}
				return
			}
		}
	case "resize":
		writeControlResponse(conn, sb.Resize(req.Rows, req.Cols))
	case "signal":
		writeControlResponse(conn, sb.Signal(req.Signal))
	case "pause":
		writeControlResponse(conn, sb.Pause())
	case "resume":
		writeControlResponse(conn, sb.Resume())
	case "top", "stats":
		var value any
		var err error
		if req.Op == "top" {
			value, err = sb.Processes()
		} else {
			value, err = sb.Usage()
		}
		writeControlResponse(conn, err)
		if err == nil {
			_ = json.NewEncoder(conn).Encode(value)
		}
	case "diff":
		writeControlResponse(conn, nil)
		_ = sb.ArchiveUpper(conn)
	case "exec":
		if req.Exec == nil {
			writeControlResponse(conn, errors.New("exec request has no command"))
			return
		}
		runControlExec(sb, conn, reader, *req.Exec)
	default:
		writeControlResponse(conn, fmt.Errorf("unknown control operation %q", req.Op))
	}
}

func writeControlResponse(w io.Writer, err error) {
	resp := controlResponse{OK: err == nil}
	if err != nil {
		resp.Error = err.Error()
	}
	data, _ := json.Marshal(resp)
	_, _ = w.Write(append(data, '\n'))
}

type frameWriter struct {
	mu   *sync.Mutex
	w    io.Writer
	kind byte
}

func (f frameWriter) Write(p []byte) (int, error) {
	if err := writeControlFrame(f.mu, f.w, f.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func writeControlFrame(mu *sync.Mutex, w io.Writer, kind byte, payload []byte) error {
	mu.Lock()
	defer mu.Unlock()
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readControlFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > 1<<20 {
		return 0, nil, fmt.Errorf("control frame of %d bytes is too large", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func runControlExec(sb *gvx.Sandbox, conn net.Conn, reader *bufio.Reader, spec execSpec) {
	stdinR, stdinW := io.Pipe()
	var mu sync.Mutex
	stdout := frameWriter{mu: &mu, w: conn, kind: frameStdout}
	stderr := frameWriter{mu: &mu, w: conn, kind: frameStderr}
	if spec.Tty {
		stderr = stdout
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proc, err := sb.Exec(ctx, gvx.ExecConfig{
		Argv: spec.Cmd, Env: spec.Env, WorkDir: spec.WorkingDir, User: spec.User,
		TTY: spec.Tty, Rows: spec.Rows, Cols: spec.Cols,
		Stdin: stdinR, Stdout: stdout, Stderr: stderr,
	})
	if errors.Is(err, gvx.ErrExecutableNotFound) {
		data, _ := json.Marshal(controlResponse{Error: err.Error(), NotFound: true})
		_, _ = conn.Write(append(data, '\n'))
	} else {
		writeControlResponse(conn, err)
	}
	if err != nil {
		_ = stdinW.Close()
		return
	}
	go func() {
		defer func() { _ = stdinW.Close() }()
		for {
			kind, payload, err := readControlFrame(reader)
			if err != nil {
				return
			}
			switch kind {
			case frameStdin:
				if _, err := stdinW.Write(payload); err != nil {
					return
				}
			case frameEOF:
				return
			case frameResize:
				if len(payload) == 4 {
					_ = proc.Resize(binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:]))
				}
			}
		}
	}()
	code := proc.Wait()
	var exit [4]byte
	binary.BigEndian.PutUint32(exit[:], uint32(code))
	_ = writeControlFrame(&mu, conn, frameExit, exit[:])
}

// dialControl connects to the control socket of a running container.
func dialControl(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return d.DialContext(ctx, "unix", path)
}

func controlCall(ctx context.Context, path string, req controlRequest) (net.Conn, *bufio.Reader, error) {
	conn, err := dialControl(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := parseControlResponse(line); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, reader, nil
}

// bufferedConn reads the bytes that a bufio.Reader holds before the
// connection.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// relayFramedDatagrams carries datagrams over a stream. Each datagram has a
// two-byte big-endian length.
func relayFramedDatagrams(stream net.Conn, packet net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		var hdr [2]byte
		buf := make([]byte, 65535)
		for {
			if _, err := io.ReadFull(stream, hdr[:]); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint16(hdr[:]))
			if _, err := io.ReadFull(stream, buf[:n]); err != nil {
				return
			}
			if _, err := packet.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 65535)
		for {
			n, err := packet.Read(buf)
			if err != nil {
				return
			}
			var hdr [2]byte
			binary.BigEndian.PutUint16(hdr[:], uint16(n))
			if _, err := stream.Write(append(hdr[:], buf[:n]...)); err != nil {
				return
			}
		}
	}()
	<-done
	_ = stream.Close()
	_ = packet.Close()
	<-done
}

// framedPacketConn is a net.Conn whose Write and Read carry one datagram each
// over a stream with two-byte length prefixes.
type framedPacketConn struct {
	net.Conn
	mu sync.Mutex
}

func (c *framedPacketConn) Write(p []byte) (int, error) {
	if len(p) > 65535 {
		return 0, fmt.Errorf("datagram of %d bytes is too large", len(p))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(p)))
	if _, err := c.Conn.Write(append(hdr[:], p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *framedPacketConn) Read(p []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.Conn, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.Conn, buf); err != nil {
		return 0, err
	}
	return copy(p, buf), nil
}

// controlNotFoundError reports a shim operation whose subject does not exist,
// such as the executable of an exec.
type controlNotFoundError struct{ msg string }

func (e *controlNotFoundError) Error() string { return e.msg }

// controlCallRaw sends req and returns the bare connection after an OK reply.
// It reads the reply one byte at a time, so the bytes that the shim sends
// after the reply stay in the socket. A caller that passes the connection to
// another process, as a dial does, then loses none of them.
func controlCallRaw(ctx context.Context, path string, req controlRequest) (net.Conn, error) {
	conn, err := dialControl(ctx, path)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		_ = conn.Close()
		return nil, err
	}
	line, err := readLineUnbuffered(conn, 64<<10)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := parseControlResponse(line); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// readLineUnbuffered reads up to and with the first newline, one byte at a
// time, and no byte after it.
func readLineUnbuffered(r io.Reader, max int) ([]byte, error) {
	var line []byte
	var b [1]byte
	for len(line) < max {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		line = append(line, b[0])
		if b[0] == '\n' {
			return line, nil
		}
	}
	return nil, errors.New("control reply is too long")
}

// parseControlResponse returns the error of a control reply line.
func parseControlResponse(line []byte) error {
	var resp controlResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if resp.OK {
		return nil
	}
	if resp.NotFound {
		return &controlNotFoundError{resp.Error}
	}
	return errors.New(resp.Error)
}
