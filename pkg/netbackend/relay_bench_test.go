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
	"io"
	"net"
	"strconv"
	"testing"

	"z.gfire.dev/gvx/pkg/gvx/tuning"
	"z.gfire.dev/gvx/pkg/tcpip/adapters/gonet"
	"z.gfire.dev/gvx/pkg/tcpip/network/ipv4"
	"z.gfire.dev/gvx/pkg/tcpip/stack"
)

// BenchmarkPortForwardTCPThroughput streams from a host client through a
// published TCP port to a guest socket that discards the data, as a client
// on the Mac uploads to a container. b.SetBytes reports MB/s.
func BenchmarkPortForwardTCPThroughput(b *testing.B) {
	s, nf, err := NewNATStack(NATConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = nf.Close()
		s.Close()
	})
	benchPortForward(b, s)
}

func benchPortForward(b *testing.B, s *stack.Stack) {
	ln, err := gonet.ListenTCP(s, guestFullAddr(9100), ipv4.ProtocolNumber)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}()
		}
	}()
	pf := NewPortForwarder(tuning.Values{})
	b.Cleanup(func() { _ = pf.Close() })
	if err := pf.PreBind([]PortForwardRule{{Proto: "tcp", HostIP: "127.0.0.1", GuestPort: 9100}}); err != nil {
		b.Fatal(err)
	}
	pf.Start(s)
	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(pf.BoundRules()[0].HostPort))))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = client.Close() })
	buf := make([]byte, 128<<10)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for range b.N {
		if _, err := client.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
}
