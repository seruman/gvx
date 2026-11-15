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
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"z.gfire.dev/gvx/pkg/gvx"
)

// rotatingLog is the json-file log of a container. It moves log.json to
// log.json.1 when the file passes maxSize, and keeps maxFiles files in all.
type rotatingLog struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	maxSize  int64
	maxFiles int
}

const (
	defaultLogMaxSize  = 10 << 20
	defaultLogMaxFiles = 3
)

func openRotatingLog(path string, cfg logConfig) (*rotatingLog, error) {
	l := &rotatingLog{path: path, maxSize: defaultLogMaxSize, maxFiles: defaultLogMaxFiles}
	if v, ok := cfg.Config["max-size"]; ok {
		if n, err := parseLogSize(v); err == nil {
			l.maxSize = n
		}
	}
	if v, ok := cfg.Config["max-file"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			l.maxFiles = n
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	l.file, l.size = f, info.Size()
	return l, nil
}

// parseLogSize parses a size such as "10m", "512k", or "1g".
func parseLogSize(v string) (int64, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "k"):
		mult, v = 1<<10, strings.TrimSuffix(v, "k")
	case strings.HasSuffix(v, "m"):
		mult, v = 1<<20, strings.TrimSuffix(v, "m")
	case strings.HasSuffix(v, "g"):
		mult, v = 1<<30, strings.TrimSuffix(v, "g")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid log size %q", v)
	}
	return n * mult, nil
}

func (l *rotatingLog) write(stream string, p []byte) error {
	entry := map[string]string{"log": string(p), "stream": stream, "time": time.Now().UTC().Format(time.RFC3339Nano)}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(data)) > l.maxSize {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := l.file.Write(data)
	l.size += int64(n)
	return err
}

func (l *rotatingLog) rotateLocked() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	if l.maxFiles > 1 {
		_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.maxFiles-1))
		for i := l.maxFiles - 2; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
		}
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	l.file, l.size = f, 0
	return nil
}

func (l *rotatingLog) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

// logWriter writes one stream of a container to its log.
type logWriter struct {
	log    *rotatingLog
	stream string
}

func (w logWriter) Write(p []byte) (int, error) {
	if err := w.log.write(w.stream, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func superviseCommand(ctx context.Context, args []string, stderr io.Writer) int {
	p, id, code := parseContainerCommand("supervise", args, stderr)
	if code != 0 {
		return code
	}
	exe, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker supervise: find executable: %v\n", err)
		return 1
	}
	if err := supervise(ctx, exe, p, newContainerStore(p.stateDir), id); err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker supervise: %v\n", err)
		return 1
	}
	return 0
}

// prepareShimProcess makes a write to a pipe without a reader return EPIPE.
// The supervisor reads the stdout and stderr of the shim. When it dies, Go
// would end the shim with SIGPIPE at its next write to them, before the
// sandbox closes its volumes and root image.
func prepareShimProcess() {
	signal.Ignore(syscall.SIGPIPE)
}

func shimCommand(ctx context.Context, args []string, stderr io.Writer) int {
	prepareShimProcess()
	var opts shimOptions
	p, id, code := parseContainerCommandWith("shim", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&opts.idle, "idle", false, "Run the idle program in place of the container command")
		fs.StringVar(&opts.control, "control", "", "Control socket path")
		fs.IntVar(&opts.netFD, "net-fd", -1, "Inherited descriptor of the supervisor network connection")
		fs.IntVar(&opts.lifeFD, "life-fd", -1, "Inherited read end of the supervisor pipe")
	})
	if code != 0 {
		return code
	}
	exitCode, err := runShim(ctx, p, newContainerStore(p.stateDir), id, opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker: %v\n", err)
	}
	return exitCode
}

func parseContainerCommand(name string, args []string, stderr io.Writer) (paths, string, int) {
	return parseContainerCommandWith(name, args, stderr, nil)
}

func parseContainerCommandWith(name string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (paths, string, int) {
	p, err := defaultPaths()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker %s: %v\n", name, err)
		return paths{}, "", 1
	}
	fs := flag.NewFlagSet("gvx-docker "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	addPathFlags(fs, &p)
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return paths{}, "", 2
	}
	p.normalize()
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintf(stderr, "gvx-docker %s: want one container ID\n", name)
		return paths{}, "", 2
	}
	return p, fs.Arg(0), 0
}

// supervise runs the container until it stops for good. Each run is a child
// shim process, because a sandbox locks down its host process and cannot start
// a second sandbox. The supervisor writes the logs and the state, and it
// applies the restart policy.
func supervise(ctx context.Context, exe string, p paths, store *containerStore, id string) error {
	rec, err := store.get(id)
	if err != nil {
		return err
	}
	// The lock marks this supervisor as alive for the API process and for
	// every later supervisor. The kernel drops it when this process dies.
	lock, err := store.claimSupervisor(rec.ID)
	if err != nil {
		return err
	}
	defer lock.release()
	// A stop that came before the lock found no supervisor to signal, so it
	// left only Stopping in the record. Honor it before the first run.
	stopped := false
	if err := store.updateState(rec.ID, func(st *containerState) {
		st.StartPending = time.Time{}
		if st.Stopping {
			// Docker records 137 for a container that a stop ended without
			// a clean exit; no run took place to give another code.
			stopped = true
			st.Running = false
			st.Restarting = false
			st.Status = "exited"
			st.ExitCode = 137
			st.FinishedAt = time.Now().UTC()
		}
	}); err != nil {
		return err
	}
	if stopped {
		return nil
	}
	logFile, err := openRotatingLog(store.logPath(rec.ID), rec.HostConfig.LogConfig)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer func() { _ = logFile.close() }()
	stdout := logWriter{log: logFile, stream: "stdout"}
	stderr := logWriter{log: logFile, stream: "stderr"}

	// One channel takes the stop and kill requests of the API process for
	// the whole life of the supervisor, also between runs. Without a handler,
	// SIGUSR1 would end the supervisor through its default action.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(sigs)

	restarts := 0
	backoff := 100 * time.Millisecond
	for {
		code, runErr := runShimProcess(ctx, exe, p, store, rec.ID, stdout, stderr, sigs)
		var restart bool
		current, err := store.update(rec.ID, func(cur *containerRecord) error {
			cur.State.Running = false
			cur.State.Restarting = false
			cur.State.Paused = false
			cur.State.Status = "exited"
			cur.State.Pid = 0
			cur.State.ExitCode = code
			cur.State.FinishedAt = time.Now().UTC()
			if runErr != nil {
				cur.State.Error = runErr.Error()
			}
			restart = shouldRestart(*cur, code, restarts)
			if restart {
				cur.State.Restarting = true
				cur.State.Status = "restarting"
				cur.RestartCount = restarts + 1
			}
			return nil
		})
		if errors.Is(err, errNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !restart {
			if current.HostConfig.AutoRemove {
				return store.removeContainerAndAnonymousVolumes(current.ID, filepath.Join(p.stateDir, "volumes"), code)
			}
			return nil
		}
		restarts++
		if !waitRestartBackoff(ctx, store, rec.ID, backoff, sigs) {
			return nil
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// waitRestartBackoff waits before the next run of a restart policy. It
// returns false when a stop or kill request, or the end of ctx, cancels the
// restart.
func waitRestartBackoff(ctx context.Context, store *containerStore, id string, backoff time.Duration, sigs <-chan os.Signal) bool {
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
		case sig := <-sigs:
			if sig != syscall.SIGTERM && sig != syscall.SIGUSR1 {
				continue
			}
		case <-timer.C:
			return true
		}
		// The run that ended is the last one. Record the exit, so the
		// container does not stay "restarting" without a supervisor.
		_ = store.updateState(id, func(st *containerState) {
			st.Restarting = false
			st.Running = false
			st.Status = "exited"
		})
		return false
	}
}

func shouldRestart(rec containerRecord, code, restarts int) bool {
	if rec.State.Stopping {
		return false
	}
	switch rec.HostConfig.RestartPolicy.Name {
	case "always", "unless-stopped":
		return true
	case "on-failure":
		maxRetries := rec.HostConfig.RestartPolicy.MaximumRetryCount
		return code != 0 && (maxRetries == 0 || restarts < maxRetries)
	default:
		return false
	}
}

func runShimProcess(ctx context.Context, exe string, p paths, store *containerStore, id string, stdout, stderr io.Writer, sigs <-chan os.Signal) (int, error) {
	cmd := exec.Command(exe, "shim", "-state-dir", p.stateDir, id)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// The shim reads the end of this pipe as the death of its supervisor and
	// kills the guest, so a shim never outlives a killed supervisor.
	lifeRead, lifeWrite, err := os.Pipe()
	if err != nil {
		return 125, fmt.Errorf("create supervisor pipe: %w", err)
	}
	defer func() { _ = lifeWrite.Close() }()
	cmd.ExtraFiles = []*os.File{lifeRead}
	cmd.Args = []string{exe, "shim", "-life-fd", "3", "-state-dir", p.stateDir, id}
	netConn, netChild, err := socketPair()
	if err == nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, netChild)
		cmd.Args = []string{exe, "shim", "-life-fd", "3", "-net-fd", "4", "-state-dir", p.stateDir, id}
		netCtx, stopNet := context.WithCancel(context.Background())
		defer stopNet()
		go serveNetworkConn(netCtx, netConn, p.stateDir, store, id)
	}
	setParentDeathSignal(cmd)
	startErr := cmd.Start()
	_ = lifeRead.Close()
	if netChild != nil {
		_ = netChild.Close()
	}
	if startErr != nil {
		return 125, fmt.Errorf("start shim: %w", startErr)
	}
	healthCtx, stopHealth := context.WithCancel(context.Background())
	defer stopHealth()
	rec, err := store.update(id, func(cur *containerRecord) error {
		cur.State.Health = nil
		if healthCommand(cur.Config.Healthcheck) != nil {
			cur.State.Health = &healthState{Status: "starting"}
		}
		cur.State.Running = true
		cur.State.Restarting = false
		cur.State.Status = "running"
		cur.State.Pid = cmd.Process.Pid
		cur.State.SupervisorPid = os.Getpid()
		cur.State.Error = ""
		cur.State.StartedAt = time.Now().UTC()
		cur.State.FinishedAt = time.Time{}
		return nil
	})
	if err == nil {
		go runHealthChecks(healthCtx, p.stateDir, store, rec)
	}
	stop := &shimStopper{shim: cmd.Process, settings: func() (int, time.Duration) { return stopSettings(store, id) }, signalGuest: func(signo int) error {
		conn, _, err := controlCall(context.Background(), controlSocketPath(p.stateDir, id), controlRequest{Op: "signal", Signal: signo})
		if err != nil {
			return err
		}
		return conn.Close()
	}}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = os.Remove(controlSocketPath(p.stateDir, id)) }()
	return superviseShim(ctx, cmd.Process, sigs, stop, done)
}

// superviseShim forwards signals to a running shim and ends it on a stop or
// kill request. It returns the exit code of the shim. done receives the result
// of the Wait for the shim.
func superviseShim(ctx context.Context, shim *os.Process, sigs <-chan os.Signal, stop *shimStopper, done <-chan error) (int, error) {
	ctxDone := ctx.Done()
	var escalate <-chan time.Time
	// keep replaces the escalation timer only with a new one. A step that
	// does nothing returns nil, and that must not drop the pending timer.
	keep := func(ch <-chan time.Time) {
		if ch != nil {
			escalate = ch
		}
	}
	for {
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGTERM:
				// docker stop: send the stop signal, then kill the guest
				// after the timeout that the API process recorded.
				keep(stop.begin())
			case syscall.SIGUSR1:
				// docker kill: kill the guest now.
				keep(stop.killGuest())
			default:
				_ = shim.Signal(sig)
			}
		case <-ctxDone:
			// One SIGTERM ends ctx and also arrives on sigs. A closed channel
			// stays ready, so the loop stops reading it.
			ctxDone = nil
			keep(stop.begin())
		case <-escalate:
			escalate = stop.next()
		case err := <-done:
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					return 128 + int(status.Signal()), nil
				}
				return exitErr.ExitCode(), nil
			}
			if err != nil {
				return 125, err
			}
			return 0, nil
		}
	}
}

// stopSettings returns the stop signal of container id as a Linux number and
// the time that a stop waits before it kills the guest.
func stopSettings(store *containerStore, id string) (int, time.Duration) {
	signo, timeout := linuxSIGTERM, 10*time.Second
	rec, err := store.get(id)
	if err != nil {
		return signo, timeout
	}
	if rec.Config.StopSignal != "" {
		if n, err := parseSignal(rec.Config.StopSignal); err == nil {
			signo = n
		}
	}
	if rec.Config.StopTimeout != nil && *rec.Config.StopTimeout >= 0 {
		timeout = time.Duration(*rec.Config.StopTimeout) * time.Second
	}
	if rec.State.StopTimeout != nil && *rec.State.StopTimeout >= 0 {
		timeout = time.Duration(*rec.State.StopTimeout) * time.Second
	}
	return signo, timeout
}

// shimStopper ends one shim in steps: the stop signal of the container, then
// a kill of the guest through SIGUSR2, then SIGKILL of the shim. It signals
// only the shim process of this supervisor, which the supervisor has not
// reaped, so the PID cannot name another process.
type shimStopper struct {
	shim        *os.Process
	signalGuest func(int) error
	// settings returns the stop signal and the timeout. begin reads them at
	// the time of the stop, so it uses the timeout that the API process
	// recorded with the request.
	settings func() (int, time.Duration)
	signal   int
	timeout  time.Duration
	// step is 0 before the stop, 1 after the stop signal, 2 after the kill
	// of the guest, and 3 after SIGKILL of the shim.
	step int
}

// begin sends the stop signal once and returns the timer of the next step.
func (st *shimStopper) begin() <-chan time.Time {
	if st.step >= 1 {
		return nil
	}
	st.step = 1
	if st.settings != nil {
		st.signal, st.timeout = st.settings()
	}
	if st.signal == linuxSIGKILL {
		return st.killGuest()
	}
	if err := st.signalGuest(st.signal); err != nil {
		// The control socket is not up yet. SIGTERM of the shim reaches the
		// guest as its Linux SIGTERM.
		_ = st.shim.Signal(syscall.SIGTERM)
	}
	return time.After(st.timeout)
}

// killGuest makes the shim kill the guest with SIGKILL and close the sandbox,
// so the volumes and the root image write their state.
func (st *shimStopper) killGuest() <-chan time.Time {
	if st.step >= 2 {
		return nil
	}
	st.step = 2
	_ = st.shim.Signal(syscall.SIGUSR2)
	return time.After(shimKillGrace)
}

// next runs the step after a timer ends.
func (st *shimStopper) next() <-chan time.Time {
	switch st.step {
	case 1:
		return st.killGuest()
	case 2:
		st.step = 3
		_ = st.shim.Kill()
	}
	return nil
}

// shimOptions selects a variant of the shim.
type shimOptions struct {
	// idle runs the idle program for a copy into a stopped container.
	idle bool
	// control replaces the control socket path.
	control string
	// netFD is the inherited descriptor of the supervisor connection, or -1.
	netFD int
	// lifeFD is the inherited read end of the supervisor pipe, or -1. Its
	// end means that the supervisor died.
	lifeFD int
}

// runShim runs one sandbox in this process and returns the exit code of the
// guest. The sandbox locks down this process.
func runShim(ctx context.Context, p paths, store *containerStore, id string, opts shimOptions) (int, error) {
	rec, err := store.get(id)
	if err != nil {
		return 125, err
	}
	cfg, err := sandboxConfig(rec, store.rootfsPath(rec.ID), engineSocketLink(p.stateDir))
	if err != nil {
		return 125, err
	}
	cfg.Stdout = os.Stdout
	cfg.Stderr = os.Stderr
	if opts.idle {
		cfg.Idle = true
		cfg.PortForwards = nil
		cfg.NetworkMode = gvx.NetModeNone
	}
	var sio *shimIO
	if rec.Config.OpenStdin && !opts.idle {
		r, w := io.Pipe()
		cfg.Stdin = r
		sio = &shimIO{stdin: w}
		defer sio.close()
	}
	if rec.Config.Tty {
		cfg.TerminalLineDiscipline = true
		cfg.TerminalRows, cfg.TerminalCols = rec.ConsoleRows, rec.ConsoleCols
	}
	controlPath := controlSocketPath(p.stateDir, rec.ID)
	if opts.control != "" {
		controlPath = opts.control
	}
	control, err := listenControl(controlPath)
	if err != nil {
		return 125, err
	}
	cfg.ControlListener = control
	var client *netClient
	if opts.netFD >= 0 {
		conn, err := fileUnixConn(os.NewFile(uintptr(opts.netFD), "gvx-docker-net"))
		if err != nil {
			return 125, fmt.Errorf("open the supervisor connection: %w", err)
		}
		client = &netClient{conn: conn}
		defer func() { _ = conn.Close() }()
	}
	cfg.LocalNames, cfg.NATDialContext = shimNetworkHooks(client, rec, allInternal(rec))
	if client != nil && !opts.idle {
		_, sockets := socketBinds(rec.HostConfig.Binds, engineSocketLink(p.stateDir))
		for guestPath, hostPath := range sockets {
			hostPath := hostPath
			cfg.UnixBridges = append(cfg.UnixBridges, gvx.UnixBridge{
				GuestPath: guestPath,
				Dial: func(context.Context) (net.Conn, error) {
					return client.dialUnix(hostPath)
				},
			})
		}
	}
	if !opts.idle {
		cfg.GuestAddresses = guestAddresses(rec)
		cfg.GuestFiles = guestEtcFiles(rec)
	}
	sb, err := gvx.New(ctx, cfg)
	if err != nil {
		return 125, err
	}
	defer func() { _ = sb.Close() }()
	go serveControlIO(sb, sio)
	// The lockdown denies file removal, so the supervisor removes the socket
	// after the shim exits.

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGCONT)
	defer signal.Stop(sigs)
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if opts.lifeFD >= 0 {
		// The end of the supervisor pipe takes the path of SIGUSR2 in this
		// process. A kill(2) of the shim itself would need a host call that
		// the Linux seccomp filter denies after the lockdown.
		go watchSupervisor(os.NewFile(uintptr(opts.lifeFD), "gvx-docker-life"), supervisorDeathHandler(sb.Signal, cancel))
	}
	go func() {
		for {
			select {
			case sig := <-sigs:
				if s, ok := sig.(syscall.Signal); ok {
					_ = guestSignalFor(s, sb.Signal)
				}
			case <-runCtx.Done():
				return
			}
		}
	}()
	return sb.Run(runCtx)
}

// guestSignalFor sends the guest the Linux signal for the host signal s of
// the shim. SIGUSR2 stands for a SIGKILL of the guest: the shim survives it
// and closes the sandbox cleanly. It makes no host call itself, so it runs
// after the lockdown.
func guestSignalFor(s syscall.Signal, signal func(int) error) error {
	if s == syscall.SIGUSR2 {
		return signal(linuxSIGKILL)
	}
	if n, ok := hostToLinuxSignal[s]; ok {
		return signal(n)
	}
	return nil
}

// supervisorDeathHandler returns the action of a shim whose supervisor died:
// a SIGKILL of the guest through the sandbox, the path of SIGUSR2. The
// signal fails when no guest runs yet, so the handler also ends the run
// context: Run then kills the guest as soon as it starts.
func supervisorDeathHandler(signal func(int) error, endRun context.CancelFunc) func() {
	return func() {
		_ = guestSignalFor(syscall.SIGUSR2, signal)
		endRun()
	}
}

// watchSupervisor calls dead when the supervisor pipe ends. The supervisor
// writes nothing, so a read returns only when every write end is closed: the
// supervisor exited or died. The shim then kills its guest, as on SIGUSR2,
// so a shim never outlives its supervisor.
func watchSupervisor(life *os.File, dead func()) {
	var b [1]byte
	_, _ = life.Read(b[:])
	_ = life.Close()
	dead()
}

func sandboxConfig(rec containerRecord, rootfsPath, engineSocket string) (gvx.Config, error) {
	binds, _ := socketBinds(rec.HostConfig.Binds, engineSocket)
	mounts, err := parseBinds(binds, rec.SeedVolumes)
	if err != nil {
		return gvx.Config{}, err
	}
	for target, opts := range rec.HostConfig.Tmpfs {
		m := gvx.Mount{Kind: gvx.MountTmpfs, GuestPath: target}
		for _, opt := range strings.Split(opts, ",") {
			if opt == "ro" {
				m.ReadOnly = true
			}
		}
		mounts = append(mounts, m)
	}
	for _, m := range rec.HostConfig.Mounts {
		if m.Type == "tmpfs" {
			mounts = append(mounts, gvx.Mount{Kind: gvx.MountTmpfs, GuestPath: m.Target, ReadOnly: m.ReadOnly})
		}
	}
	ports, err := portForwards(rec.State.Ports)
	if err != nil {
		return gvx.Config{}, err
	}
	netMode := gvx.NetModeNAT
	if rec.HostConfig.NetworkMode == "none" {
		netMode = gvx.NetModeNone
	}
	cfg := gvx.Config{
		HostDNSServers:         rec.HostConfig.DNS,
		Platform:               gvx.PlatformAuto,
		NetworkMode:            netMode,
		NetRaw:                 true,
		AllowPacketSocketWrite: true,
		AllowSUID:              true,
		OCIImage:               rec.Config.Image,
		PullPolicy:             gvx.PullNever,
		RootfsUpperPath:        rootfsPath,
		Mounts:                 mounts,
		PortForwards:           ports,
		Envv:                   rec.Config.Env,
		WorkDir:                rec.Config.WorkingDir,
		User:                   rec.Config.User,
		Hostname:               hostnameOf(rec),
		ReadOnlyRoot:           rec.HostConfig.ReadonlyRootfs,
		ShmSizeBytes:           rec.HostConfig.ShmSize,
		ExtraGroups:            rec.HostConfig.GroupAdd,
		Ulimits:                ulimits(rec.HostConfig.Ulimits),
		Name:                   strings.TrimPrefix(rec.Name, "/"),
		TTY:                    rec.Config.Tty,
		Resources: gvx.Resources{
			MemoryBytes: rec.HostConfig.Memory,
			CPUMillis:   rec.HostConfig.NanoCPUs / 1_000_000,
		},
	}
	if rec.Config.Entrypoint != nil {
		cfg.Entrypoint = rec.Config.Entrypoint
		cfg.EntrypointSet = true
	}
	cfg.Cmd = rec.Config.Cmd
	return cfg, nil
}

// guestEtcFiles returns /etc/hosts and /etc/resolv.conf as Docker writes
// them. The resolver of the sandbox answers container names, so resolv.conf
// always names it. The --dns servers answer the other names: see
// sandboxConfig.
func guestEtcFiles(rec containerRecord) map[string][]byte {
	var hosts strings.Builder
	hosts.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	hostname := hostnameOf(rec)
	names := hostname
	if rec.Config.Domainname != "" {
		names = hostname + "." + rec.Config.Domainname + " " + hostname
	}
	own := "10.0.2.15"
	if addrs := guestAddresses(rec); len(addrs) > 0 {
		own, _, _ = strings.Cut(addrs[0], "/")
	}
	fmt.Fprintf(&hosts, "%s\t%s\n", own, names)
	hosts.WriteString("10.0.2.2\thost.docker.internal gateway.docker.internal\n")
	for _, entry := range rec.HostConfig.ExtraHosts {
		name, addr, ok := strings.Cut(entry, ":")
		if !ok {
			name, addr, ok = strings.Cut(entry, "=")
		}
		if !ok {
			continue
		}
		if addr == "host-gateway" {
			addr = "10.0.2.2"
		}
		fmt.Fprintf(&hosts, "%s\t%s\n", strings.Trim(addr, "[]"), name)
	}
	var resolv strings.Builder
	resolv.WriteString("nameserver 10.0.2.3\n")
	if len(rec.HostConfig.DNSSearch) > 0 {
		fmt.Fprintf(&resolv, "search %s\n", strings.Join(rec.HostConfig.DNSSearch, " "))
	}
	if len(rec.HostConfig.DNSOptions) > 0 {
		fmt.Fprintf(&resolv, "options %s\n", strings.Join(rec.HostConfig.DNSOptions, " "))
	}
	return map[string][]byte{"/etc/hosts": []byte(hosts.String()), "/etc/resolv.conf": []byte(resolv.String())}
}

// allInternal reports a container whose every network is internal.
func allInternal(rec containerRecord) bool {
	if len(rec.Networks) == 0 {
		return false
	}
	for _, ep := range rec.Networks {
		if !ep.Internal {
			return false
		}
	}
	return true
}

// guestAddresses returns the address of the container on each of its
// networks in CIDR form.
func guestAddresses(rec containerRecord) []string {
	var out []string
	for _, ep := range rec.Networks {
		prefix, err := netip.ParsePrefix(ep.Subnet)
		if err != nil {
			continue
		}
		out = append(out, ep.IPAddress+"/"+strconv.Itoa(prefix.Bits()))
	}
	sort.Strings(out)
	return out
}

func ulimits(in []dockerUlimit) map[string]gvx.Ulimit {
	if len(in) == 0 {
		return nil
	}
	out := map[string]gvx.Ulimit{}
	for _, u := range in {
		toLimit := func(v int64) uint64 {
			if v < 0 {
				return ^uint64(0)
			}
			return uint64(v)
		}
		out[u.Name] = gvx.Ulimit{Soft: toLimit(u.Soft), Hard: toLimit(u.Hard)}
	}
	return out
}

func hostnameOf(rec containerRecord) string {
	if rec.Config.Hostname != "" {
		return rec.Config.Hostname
	}
	return rec.ID[:12]
}

// socketBinds splits the binds whose host source is a Unix socket from the
// others. A guest reaches such a socket through a bridge, because a host
// directory mount cannot connect to a host socket. The Docker socket paths
// name this engine, as they do on a Docker host; another engine that owns
// them on the Mac is not reachable that way.
func socketBinds(binds []string, engineSocket string) (others []string, sockets map[string]string) {
	sockets = map[string]string{}
	for _, bind := range binds {
		parts := strings.Split(bind, ":")
		if len(parts) < 2 {
			others = append(others, bind)
			continue
		}
		src, dst := parts[0], parts[1]
		if src == "/var/run/docker.sock" || src == "/run/docker.sock" {
			src = engineSocket
		}
		if info, err := os.Stat(src); err == nil && info.Mode()&os.ModeSocket != 0 {
			sockets[dst] = src
			continue
		}
		others = append(others, bind)
	}
	return others, sockets
}

func parseBinds(binds []string, seeds []string) ([]gvx.Mount, error) {
	seedSet := map[string]bool{}
	for _, s := range seeds {
		seedSet[s] = true
	}
	mounts := make([]gvx.Mount, 0, len(binds))
	for _, bind := range binds {
		m, err := gvx.ParseVolume(bind)
		if err != nil {
			return nil, err
		}
		if m.Kind == gvx.MountBind {
			m.CreateHostPathIfMissing = true
			m.SeedFromImage = seedSet[m.HostPath+":"+m.GuestPath]
		}
		mounts = append(mounts, m)
	}
	return mounts, nil
}

func portForwards(bindings map[string][]portBinding) ([]gvx.PortForward, error) {
	return portForwardsFromBindings(bindings)
}

func portForwardsFromBindings(bindings map[string][]portBinding) ([]gvx.PortForward, error) {
	var out []gvx.PortForward
	for containerPort, hostBindings := range bindings {
		port, proto, _ := strings.Cut(containerPort, "/")
		if proto == "" {
			proto = "tcp"
		}
		guest, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid container port %q", containerPort)
		}
		for _, hb := range hostBindings {
			host := uint64(0)
			if hb.HostPort != "" {
				host, err = strconv.ParseUint(hb.HostPort, 10, 16)
				if err != nil {
					return nil, fmt.Errorf("invalid host port %q", hb.HostPort)
				}
			}
			// Docker publishes on every interface unless the client names an
			// address.
			addr := hb.HostIP
			if addr == "" {
				addr = "0.0.0.0"
			}
			out = append(out, gvx.PortForward{HostPort: uint16(host), GuestPort: uint16(guest), Protocol: proto, BindAddress: addr})
		}
	}
	return out, nil
}

// hostToLinuxSignal maps the host signals that a shim forwards to the guest
// to their Linux numbers. SIGUSR2 is absent: it stands for a SIGKILL.
var hostToLinuxSignal = map[syscall.Signal]int{
	syscall.SIGHUP: 1, syscall.SIGINT: 2, syscall.SIGQUIT: 3, syscall.SIGTERM: 15,
	syscall.SIGCONT: 18, syscall.SIGWINCH: 28,
}
