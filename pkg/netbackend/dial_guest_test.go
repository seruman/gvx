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

package netbackend

import (
	"context"
	"io"
	"net/netip"
	"testing"
	"time"

	"z.gfire.dev/gvx/pkg/tcpip/adapters/gonet"
	"z.gfire.dev/gvx/pkg/tcpip/network/ipv4"
)

// TestDialGuestFromReusesASourcePortInTimeWait dials the guest twice from
// the same source address and port, as a peer container does when its guest
// picks a port number again. The first connection closes from the dialing
// side, so its endpoint stays in TIME-WAIT for a minute. The second dial
// must still connect.
func TestDialGuestFromReusesASourcePortInTimeWait(t *testing.T) {
	s, nf, err := NewNATStack(NATConfig{})
	if err != nil {
		t.Fatalf("NewNATStack: %v", err)
	}
	defer func() {
		_ = nf.Close()
		s.Close()
	}()
	ln, err := gonet.ListenTCP(s, guestFullAddr(7000), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 4)
				if _, err := io.ReadFull(c, b); err == nil {
					_, _ = c.Write(b)
				}
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()

	src := netip.MustParseAddrPort("172.18.0.3:40000")
	dst := netip.MustParseAddrPort("10.0.2.15:7000")
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := nf.DialGuestFrom(ctx, "tcp", src, dst)
		cancel()
		if err != nil {
			t.Fatalf("dial %d from %v: %v", i, src, err)
		}
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 4)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, b); err != nil {
			t.Fatalf("dial %d: read: %v", i, err)
		}
		_ = c.Close()
		// Let the close finish, so the endpoint reaches TIME-WAIT.
		time.Sleep(50 * time.Millisecond)
	}
}
