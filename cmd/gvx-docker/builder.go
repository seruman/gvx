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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"z.gfire.dev/gvx/pkg/gvx"
)

const buildKitGuestPort = 1234

type builderProcess struct {
	exe   string
	paths paths
	log   io.Writer

	mu  sync.Mutex
	cmd *exec.Cmd
	// exited closes when the builder process in cmd ends. The goroutine that
	// waits for the process closes it, so readers need no access to
	// cmd.ProcessState.
	exited chan struct{}
}

// ensure starts the builder sandbox when its socket does not accept a
// connection. It returns after the socket accepts one.
func (b *builderProcess) ensure(ctx context.Context) error {
	if socketReady(b.paths.backendSock) {
		return nil
	}
	b.mu.Lock()
	if b.cmd == nil || isClosed(b.exited) {
		cmd := exec.Command(b.exe, b.builderArgs()...)
		cmd.Stdout = b.log
		cmd.Stderr = b.log
		if err := cmd.Start(); err != nil {
			b.mu.Unlock()
			return fmt.Errorf("start builder: %w", err)
		}
		exited := make(chan struct{})
		b.cmd, b.exited = cmd, exited
		go func() {
			_ = cmd.Wait()
			close(exited)
		}()
	}
	exited := b.exited
	b.mu.Unlock()
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-exited:
			cancel()
		case <-waitCtx.Done():
		}
	}()
	err := waitForSocket(waitCtx, b.paths.backendSock, 60*time.Second)
	if err != nil && isClosed(exited) && ctx.Err() == nil {
		return errors.New("the BuildKit builder exited before it opened its socket; see engine.log in the state directory")
	}
	return err
}

// builderArgs returns the arguments of the builder subcommand, with the
// builder options of serve.
func (b *builderProcess) builderArgs() []string {
	args := []string{"builder", "-state-dir", b.paths.stateDir, "-buildkit-image", b.paths.buildkitImage}
	if b.paths.builderHostLoopback {
		args = append(args, "-builder-host-loopback")
	}
	return args
}

// isClosed reports whether ch is closed. A nil channel is open.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (b *builderProcess) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd != nil && !isClosed(b.exited) {
		_ = b.cmd.Process.Signal(os.Interrupt)
	}
}

func runBuildKit(ctx context.Context, p paths, stderr io.Writer) (int, error) {
	if err := os.MkdirAll(filepath.Dir(p.buildkitSock), 0o700); err != nil {
		return 0, fmt.Errorf("create run directory: %w", err)
	}
	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		return 0, fmt.Errorf("create state directory: %w", err)
	}
	cfg := buildKitConfig(p, stderr)
	sb, err := gvx.New(ctx, cfg)
	if err != nil {
		return 0, fmt.Errorf("start BuildKit sandbox: %w", err)
	}
	defer func() { _ = sb.Close() }()
	return sb.Run(ctx)
}

func serveBuildKitProxy(ctx context.Context, path, backend string, builder builderStarter) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create BuildKit socket directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", path, err)
	}
	defer func() { _ = os.Remove(path) }()
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("restrict %s: %w", path, err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		client, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept BuildKit connection: %w", err)
		}
		go proxyBuildKitConn(ctx, client, backend, builder)
	}
}

func proxyBuildKitConn(ctx context.Context, client net.Conn, backend string, builder builderStarter) {
	defer func() { _ = client.Close() }()
	if err := builder.ensure(ctx); err != nil {
		return
	}
	server, err := net.Dial("unix", backend)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(server, client)
		closeWrite(server)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, server)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(conn net.Conn) {
	if c, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
	}
}

func socketReady(path string) bool {
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if socketReady(path) {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("BuildKit socket %s did not accept a connection within %s", path, timeout)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// buildKitConfig returns the sandbox config of the BuildKit builder.
func buildKitConfig(p paths, stderr io.Writer) gvx.Config {
	cfg := gvx.Config{
		Platform:    gvx.PlatformAuto,
		NetworkMode: gvx.NetModeNAT,
		// RUN steps share this network. With the flag, a build reaches
		// the services on the loopback of the Mac, such as a registry that
		// publishes its port there; any Dockerfile then reaches them too.
		LoopbackToHost:         p.builderHostLoopback,
		NetRaw:                 true,
		AllowPacketSocketWrite: true,
		AllowSUID:              true,
		OCIImage:               p.buildkitImage,
		PullPolicy:             gvx.PullMissing,
		Warnings:               stderr,
		Stdin:                  nil,
		Stdout:                 stderr,
		Stderr:                 stderr,
		PortForwards: []gvx.PortForward{{
			GuestPort:  buildKitGuestPort,
			Protocol:   "tcp",
			HostSocket: p.backendSock,
		}},
		Mounts: []gvx.Mount{{
			Kind:      gvx.MountVolume,
			ImagePath: filepath.Join(p.stateDir, "buildkit.ext4"),
			GuestPath: "/var/lib/buildkit",
		}},
		Argv:      []string{"buildkitd", "--addr", fmt.Sprintf("tcp://0.0.0.0:%d", buildKitGuestPort)},
		Resources: gvx.Resources{MemoryBytes: 4 << 30},
		Name:      "gvx-docker-buildkit",
	}
	// An optional buildkitd.toml in the state directory configures the
	// builder, as --buildkitd-config does for the docker-container driver:
	// for example, registry mirrors or a plain HTTP registry.
	if conf := filepath.Join(p.stateDir, "buildkitd.toml"); fileExists(conf) {
		cfg.Mounts = append(cfg.Mounts, gvx.Mount{Kind: gvx.MountBind, HostPath: conf, GuestPath: "/etc/buildkit/buildkitd.toml", ReadOnly: true})
		cfg.Argv = append(cfg.Argv, "--config", "/etc/buildkit/buildkitd.toml")
	}
	return cfg
}
