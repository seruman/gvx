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

package gvx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"

	"z.gfire.dev/gvx/pkg/abi/linux"
	"z.gfire.dev/gvx/pkg/hostlockdown"
	"z.gfire.dev/gvx/pkg/netbackend"
	"z.gfire.dev/gvx/pkg/sentry/kernel"
	"z.gfire.dev/gvx/pkg/sentry/kernel/auth"
	"z.gfire.dev/gvx/pkg/sentry/vfs"
	"z.gfire.dev/gvx/pkg/tcpip"
	"z.gfire.dev/gvx/pkg/tcpip/adapters/gonet"
	"z.gfire.dev/gvx/pkg/tcpip/network/ipv4"
)

// ErrExecutableNotFound reports that the guest root file system has no
// regular file for the command of a process.
var ErrExecutableNotFound = errors.New("gvx: executable not found")

// ExecConfig configures one process that Exec starts in a running guest.
type ExecConfig struct {
	// Argv is the command vector. Argv[0] is resolved through PATH of Env.
	Argv []string
	// Env is merged over the environment of the guest configuration.
	Env []string
	// WorkDir is the working directory. The empty string selects the working
	// directory of the main process.
	WorkDir string
	// User is a Docker user specification. The empty string selects the user
	// of the main process.
	User string
	// TTY gives the process a terminal on its standard streams. The terminal
	// runs the line discipline itself, because a remote client sends raw
	// input, as the Docker CLI does.
	TTY bool
	// Rows and Cols set the first terminal size when TTY is true.
	Rows, Cols uint16

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// ExecProcess is a process that Exec started.
type ExecProcess struct {
	k       *kernel.Kernel
	tg      *kernel.ThreadGroup
	tty     *ttyState
	cleanup func()
}

// Exec starts a process in the running guest. It shares the mount, network,
// UTS, IPC, and PID namespaces of the main process. It returns ErrNotRunning
// when the guest does not run.
func (s *Sandbox) Exec(ctx context.Context, cfg ExecConfig) (*ExecProcess, error) {
	if len(cfg.Argv) == 0 {
		return nil, fmt.Errorf("%w: empty exec command", ErrInvalidConfig)
	}
	if _, _, _, err := splitUserSpec(cfg.User); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := s.waitGuestStarted(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	k, mainTG := s.activeKernel, s.activeThreadGroup
	s.mu.Unlock()
	if k == nil || mainTG == nil {
		return nil, ErrNotRunning
	}
	leader := mainTG.Leader()
	if leader == nil {
		return nil, ErrNotRunning
	}
	mntns := leader.GetMountNamespace()
	if mntns == nil {
		return nil, ErrNotRunning
	}
	sctx := k.SupervisorContext()
	root := mntns.Root(sctx)
	defer root.DecRef(sctx)
	sctxWithRoot := vfs.WithRoot(sctx, root)
	vfsObj := k.VFS()
	rootCreds := auth.NewRootCredentials(k.RootUserNamespace())

	env, err := mergeEnv(s.cfg.Envv, cfg.Env)
	if err != nil {
		mntns.DecRef(sctx)
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	execPath, err := resolveGuestExecutable(sctxWithRoot, vfsObj, rootCreds, root, cfg.Argv[0], env)
	if err != nil {
		mntns.DecRef(sctx)
		return nil, err
	}
	userSpec := cmp.Or(cfg.User, s.cfg.User)
	user, procCreds, err := s.guestUserSpec(sctxWithRoot, vfsObj, rootCreds, root, userSpec)
	if err != nil {
		mntns.DecRef(sctx)
		return nil, err
	}

	fdTable := k.NewFDTable()
	var tty *ttyState
	var pump *stdinPump
	var cleanups []func()
	runCleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	if cfg.TTY {
		tty = newTTYState(cfg.Stdin, cfg.Stdout, true, cfg.Rows, cfg.Cols)
		cleanups = append(cleanups, tty.stop)
	} else if cfg.Stdin != nil {
		pump = newStdinPump(cfg.Stdin)
		cleanups = append(cleanups, pump.stop)
	}
	stdout, stderr := cfg.Stdout, cfg.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	stdio, releaseStdio, err := s.openExecStdio(sctx, vfsObj, rootCreds, cfg.Stdin, stdout, stderr, tty, pump)
	if err != nil {
		fdTable.DecRef(sctx)
		mntns.DecRef(sctx)
		runCleanup()
		return nil, err
	}
	cleanups = append(cleanups, releaseStdio)
	_, fdErr := fdTable.NewFDs(sctx, 0, stdio[:], kernel.FDFlags{})
	for _, fd := range stdio {
		fd.DecRef(sctx)
	}
	if fdErr != nil {
		fdTable.DecRef(sctx)
		mntns.DecRef(sctx)
		runCleanup()
		return nil, fmt.Errorf("gvx: bind exec stdio: %w", fdErr)
	}
	if tty != nil {
		removeNode, err := exposeTerminal(sctx, vfsObj, rootCreds, root, fdTable, tty)
		if err != nil {
			fdTable.DecRef(sctx)
			mntns.DecRef(sctx)
			runCleanup()
			return nil, err
		}
		cleanups = append(cleanups, removeNode)
	}
	ls, err := s.limitSet()
	if err != nil {
		fdTable.DecRef(sctx)
		mntns.DecRef(sctx)
		runCleanup()
		return nil, err
	}
	var procTTY *kernel.TTY
	if tty != nil {
		procTTY = tty.ktty
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = s.initialWorkingDir()
	}
	tg, _, err := k.CreateProcess(kernel.CreateProcessArgs{
		Filename:             execPath,
		Argv:                 cfg.Argv,
		Envv:                 finalizeEnv(env, guestEnvDefaults{Home: user.Home, Hostname: s.hostname(), TTY: tty != nil}),
		WorkingDirectory:     workDir,
		Credentials:          procCreds,
		FDTable:              fdTable,
		MountNamespace:       mntns,
		Umask:                0o022,
		Limits:               ls,
		MaxSymlinkTraversals: linux.MaxSymlinkTraversals,
		UTSNamespace:         k.RootUTSNamespace(),
		IPCNamespace:         k.RootIPCNamespace(),
		Origin:               kernel.OriginExec,
		ContainerID:          cmp.Or(s.cfg.Name, "gvx-default"),
		PIDNamespace:         k.RootPIDNamespace(),
		TTY:                  procTTY,
	})
	fdTable.DecRef(sctx)
	if err != nil {
		runCleanup()
		return nil, fmt.Errorf("gvx: create exec process %q: %w", execPath, err)
	}
	if tty != nil {
		tty.startPump()
	}
	k.StartProcess(tg)
	p := &ExecProcess{k: k, tg: tg, tty: tty, cleanup: runCleanup}
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Signal(int(linux.SIGKILL))
		case <-waitThreadGroup(tg):
		}
	}()
	return p, nil
}

func waitThreadGroup(tg *kernel.ThreadGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		tg.WaitExited()
		close(done)
	}()
	return done
}

// Wait waits for the process and returns its exit code. A signal gives 128
// plus the signal number.
func (p *ExecProcess) Wait() int {
	p.tg.WaitExited()
	if p.cleanup != nil {
		p.cleanup()
	}
	ws := p.tg.ExitStatus()
	if ws.Signaled() {
		return 128 + int(ws.TerminationSignal())
	}
	return int(ws.ExitStatus())
}

// Signal sends signo to the process.
func (p *ExecProcess) Signal(signo int) error {
	if signo <= 0 || signo > int(linux.SignalMaximum) {
		return fmt.Errorf("%w: invalid signal %d", ErrInvalidConfig, signo)
	}
	return p.k.SendExternalSignalThreadGroup(p.tg, &linux.SignalInfo{Signo: int32(signo)})
}

// Resize sets the terminal size of a process that has a terminal.
func (p *ExecProcess) Resize(rows, cols uint16) error {
	if p.tty == nil {
		return errors.New("gvx: the exec process has no terminal")
	}
	p.tty.setWinsize(linux.Winsize{Row: rows, Col: cols})
	return nil
}

// Resize sets the terminal size of the main process when it has a terminal.
func (s *Sandbox) Resize(rows, cols uint16) error {
	s.mu.Lock()
	tg := s.activeThreadGroup
	s.mu.Unlock()
	if tg == nil {
		return ErrNotRunning
	}
	ktty := tg.TTY()
	if ktty == nil {
		return errors.New("gvx: the guest has no terminal")
	}
	tty, ok := ktty.TTYOperations.(*ttyState)
	if !ok {
		return errors.New("gvx: the guest terminal does not support resize")
	}
	tty.setWinsize(linux.Winsize{Row: rows, Col: cols})
	return nil
}

// AcceptControlConn accepts one connection from Config.ControlListener. It
// works before and after the host lockdown.
func (s *Sandbox) AcceptControlConn() (net.Conn, error) {
	s.mu.Lock()
	ln := s.cfg.ControlListener
	locked := s.lockdown.Applied
	s.mu.Unlock()
	if ln == nil {
		return nil, fmt.Errorf("%w: Config.ControlListener is nil", ErrInvalidConfig)
	}
	if locked {
		return hostlockdown.AcceptListener(ln)
	}
	return ln.Accept()
}

// DialGuestTCP opens a TCP connection from the host side to port on the guest
// address of the NAT network. It works after the host lockdown.
func (s *Sandbox) DialGuestTCP(ctx context.Context, port uint16) (net.Conn, error) {
	s.mu.Lock()
	st := s.netStack
	s.mu.Unlock()
	if st == nil {
		return nil, fmt.Errorf("%w: the sandbox has no network stack", ErrInvalidConfig)
	}
	addr := tcpip.FullAddress{Addr: tcpip.AddrFromSlice(net.ParseIP(netbackend.DefaultGuestIPv4).To4()), Port: port}
	return gonet.DialContextTCP(ctx, st, addr, ipv4.ProtocolNumber)
}

// DialGuestUDP opens a UDP socket from the host side to port on the guest
// address of the NAT network. Each Write sends one datagram. It works after
// the host lockdown.
func (s *Sandbox) DialGuestUDP(port uint16) (net.Conn, error) {
	s.mu.Lock()
	st := s.netStack
	s.mu.Unlock()
	if st == nil {
		return nil, fmt.Errorf("%w: the sandbox has no network stack", ErrInvalidConfig)
	}
	addr := tcpip.FullAddress{Addr: tcpip.AddrFromSlice(net.ParseIP(netbackend.DefaultGuestIPv4).To4()), Port: port}
	return gonet.DialUDP(st, nil, &addr, ipv4.ProtocolNumber)
}

// DialGuestFrom opens a connection to dst on the guest network with the source
// address src, so the guest sees src as the peer. network is "tcp" or "udp".
// It needs NAT mode and works after the host lockdown.
func (s *Sandbox) DialGuestFrom(ctx context.Context, network string, src, dst netip.AddrPort) (net.Conn, error) {
	s.mu.Lock()
	nf := s.natForwarder
	s.mu.Unlock()
	if nf == nil {
		return nil, fmt.Errorf("%w: the sandbox has no NAT network", ErrInvalidConfig)
	}
	return nf.DialGuestFrom(ctx, network, src, dst)
}

// NATGuestSource returns the guest source address of the connection that a
// Config.NATDialContext call serves.
func NATGuestSource(ctx context.Context) (netip.AddrPort, bool) {
	return netbackend.GuestSource(ctx)
}

// waitGuestStarted waits until Run has started the guest, so an embedder can
// serve requests from the moment it calls Run. It returns ErrNotRunning when
// the run ends first.
func (s *Sandbox) waitGuestStarted(ctx context.Context) error {
	s.mu.Lock()
	ready, done := s.kernelReady, s.runDone
	s.mu.Unlock()
	if ready == nil {
		return nil
	}
	select {
	case <-ready:
		return nil
	case <-done:
		return ErrNotRunning
	case <-ctx.Done():
		return ctx.Err()
	}
}
