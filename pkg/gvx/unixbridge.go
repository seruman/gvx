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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"time"

	"z.gfire.dev/gvx/pkg/abi/linux"
	gvcontext "z.gfire.dev/gvx/pkg/context"
	"z.gfire.dev/gvx/pkg/errors/linuxerr"
	"z.gfire.dev/gvx/pkg/sentry/kernel/auth"
	"z.gfire.dev/gvx/pkg/sentry/socket/unix/transport"
	"z.gfire.dev/gvx/pkg/sentry/vfs"
	"z.gfire.dev/gvx/pkg/syserr"
	"z.gfire.dev/gvx/pkg/tcpip"
	"z.gfire.dev/gvx/pkg/waiter"
)

// UnixBridge publishes a Unix stream socket at a guest path. Each guest
// connection to the path calls Dial and relays bytes both ways. A guest sees
// an ordinary listening socket: connect(2) to the path succeeds while Dial
// succeeds. The bridge carries bytes only, without credentials or
// descriptors.
type UnixBridge struct {
	// GuestPath is the absolute path of the socket in the guest.
	GuestPath string
	// Dial opens the connection that serves one guest connection. It runs
	// after the host lockdown, so it uses a channel that the lockdown allows,
	// such as an inherited socket.
	Dial func(ctx context.Context) (net.Conn, error)
}

// installUnixBridges binds a listening endpoint at the guest path of each
// bridge and starts its accept loop. The loops end with ctx.
func (s *Sandbox) installUnixBridges(ctx context.Context, sctx, mntnsCtx gvcontext.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, uid interface{ UniqueID() uint64 }) error {
	for _, b := range s.cfg.UnixBridges {
		if !path.IsAbs(b.GuestPath) || b.Dial == nil {
			return fmt.Errorf("%w: unix bridge %q needs an absolute guest path and a dial function", ErrInvalidConfig, b.GuestPath)
		}
		dir := path.Dir(b.GuestPath)
		if err := vfsEnsureDirAll(sctx, vfsObj, creds, root, dir, 0o755); err != nil {
			return fmt.Errorf("gvx: create guest directory %s: %w", dir, err)
		}
		ep := transport.NewConnectioned(sctx, linux.SOCK_STREAM, uid)
		if err := ep.Bind(transport.Address{Addr: b.GuestPath}); err != nil {
			ep.Close(sctx)
			return fmt.Errorf("gvx: bind the bridge socket %s: %v", b.GuestPath, err)
		}
		pop := vfs.PathOperation{Root: root, Start: root, Path: makeFPath(b.GuestPath)}
		// A socket node that the image or an earlier run left at the path
		// gives way, as a host bind mount of a socket replaces it. The
		// overlay unlink reads the mount namespace of the caller, so the call
		// runs with the root mount namespace.
		if _, err := vfsObj.StatAt(sctx, creds, &pop, &vfs.StatOptions{Mask: linux.STATX_TYPE}); err == nil {
			if err := vfsObj.UnlinkAt(mntnsCtx, creds, &pop); err != nil {
				ep.Close(sctx)
				return fmt.Errorf("gvx: replace %s with the bridge socket: %w", b.GuestPath, err)
			}
		}
		bep, ok := ep.(transport.BoundEndpoint)
		if !ok {
			ep.Close(sctx)
			return fmt.Errorf("gvx: the bridge endpoint of %s cannot be bound", b.GuestPath)
		}
		if err := vfsObj.MknodAt(sctx, creds, &pop, &vfs.MknodOptions{Mode: linux.S_IFSOCK | 0o666, Endpoint: bep}); err != nil {
			ep.Close(sctx)
			return fmt.Errorf("gvx: create the bridge socket %s: %w", b.GuestPath, err)
		}
		if err := ep.Listen(sctx, 128); err != nil {
			ep.Close(sctx)
			return fmt.Errorf("gvx: listen on the bridge socket %s: %v", b.GuestPath, err)
		}
		go serveUnixBridge(ctx, sctx, ep, b)
	}
	return nil
}

func serveUnixBridge(ctx context.Context, sctx gvcontext.Context, ep transport.Endpoint, b UnixBridge) {
	defer ep.Close(sctx)
	entry, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
	if err := ep.EventRegister(&entry); err != nil {
		return
	}
	defer ep.EventUnregister(&entry)
	for {
		conn, err := ep.Accept(sctx, nil)
		if err == syserr.ErrWouldBlock {
			select {
			case <-ctx.Done():
				return
			case <-ch:
			}
			continue
		}
		if err != nil {
			return
		}
		go relayBridgeConn(ctx, sctx, conn, b)
	}
}

// guestStream reads and writes one accepted guest endpoint with blocking
// calls.
type guestStream struct {
	sctx gvcontext.Context
	ep   transport.Endpoint
}

func (g guestStream) wait(mask waiter.EventMask, ready func() bool) {
	entry, ch := waiter.NewChannelEntry(mask)
	if err := g.ep.EventRegister(&entry); err != nil {
		time.Sleep(time.Millisecond)
		return
	}
	defer g.ep.EventUnregister(&entry)
	if ready() {
		return
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
	}
}

func (g guestStream) Read(p []byte) (int, error) {
	for {
		out, notify, err := g.ep.RecvMsg(g.sctx, [][]byte{p}, transport.RecvArgs{})
		if notify != nil {
			notify()
		}
		if err == nil {
			if out.RecvLen == 0 {
				return 0, io.EOF
			}
			return int(out.RecvLen), nil
		}
		if err == syserr.ErrClosedForReceive {
			return 0, io.EOF
		}
		if err != syserr.ErrWouldBlock {
			return 0, errors.New(err.String())
		}
		g.wait(waiter.ReadableEvents|waiter.EventHUp|waiter.EventRdHUp, func() bool {
			return g.ep.Readiness(waiter.ReadableEvents|waiter.EventHUp|waiter.EventRdHUp) != 0
		})
	}
}

func (g guestStream) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n, notify, err := g.ep.SendMsg(g.sctx, [][]byte{p[written:]}, transport.ControlMessages{}, nil)
		if notify != nil {
			notify()
		}
		written += int(n)
		if err == nil {
			continue
		}
		if err != syserr.ErrWouldBlock {
			return written, linuxerr.EPIPE
		}
		g.wait(waiter.WritableEvents|waiter.EventHUp, func() bool {
			return g.ep.Readiness(waiter.WritableEvents|waiter.EventHUp) != 0
		})
	}
	return written, nil
}

func relayBridgeConn(ctx context.Context, sctx gvcontext.Context, ep transport.Endpoint, b UnixBridge) {
	defer ep.Close(sctx)
	host, err := b.Dial(ctx)
	if err != nil {
		return
	}
	defer func() { _ = host.Close() }()
	guest := guestStream{sctx: sctx, ep: ep}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(host, guest)
		if cw, ok := host.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(guest, host)
		ep.Shutdown(tcpip.ShutdownWrite)
		done <- struct{}{}
	}()
	<-done
	<-done
}
