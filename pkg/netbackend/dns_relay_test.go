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
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"z.gfire.dev/gvx/pkg/gvx/tuning"
	"z.gfire.dev/gvx/pkg/tcpip"
	"z.gfire.dev/gvx/pkg/tcpip/adapters/gonet"
	"z.gfire.dev/gvx/pkg/tcpip/network/ipv4"
)

const (
	dnsHeaderSize = 12
	dnsTypeA      = uint16(dnsmessage.TypeA)
	dnsTypeAAAA   = uint16(dnsmessage.TypeAAAA)
	dnsClassIN    = uint16(dnsmessage.ClassINET)
	dnsFlagQR     = 1 << 15
	dnsFlagTC     = 1 << 9
	dnsRCodeNoErr = dnsmessage.RCodeSuccess
	dnsRCodeFmt   = dnsmessage.RCodeFormatError
	dnsRCodeSrv   = dnsmessage.RCodeServerFailure
	dnsRCodeNX    = dnsmessage.RCodeNameError
	dnsRCodeRefus = dnsmessage.RCodeRefused
)

// buildDNSQuery returns a standard query with recursion desired.
func buildDNSQuery(id uint16, name string, qtype uint16) []byte {
	q := []byte{byte(id >> 8), byte(id), 0x01, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	return append(q, 0x00, byte(qtype>>8), byte(qtype), 0x00, byte(dnsClassIN))
}

func dnsRCode(resp []byte) dnsmessage.RCode {
	var p dnsmessage.Parser
	hdr, err := p.Start(resp)
	if err != nil {
		return 0
	}
	return hdr.RCode
}

// answerIPs returns the A and AAAA addresses in the answer section of a response.
func answerIPs(t *testing.T, resp []byte) []netip.Addr {
	t.Helper()
	var p dnsmessage.Parser
	if _, err := p.Start(resp); err != nil {
		t.Fatalf("parse response header: %v", err)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatalf("skip response questions: %v", err)
	}
	var out []netip.Addr
	for {
		hdr, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			t.Fatalf("parse answer header: %v", err)
		}
		switch hdr.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				t.Fatalf("parse A resource: %v", err)
			}
			out = append(out, netip.AddrFrom4(r.A))
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				t.Fatalf("parse AAAA resource: %v", err)
			}
			out = append(out, netip.AddrFrom16(r.AAAA))
		default:
			if err := p.SkipAnswer(); err != nil {
				t.Fatalf("skip answer: %v", err)
			}
		}
	}
	return out
}

// fakeDNSServer is an upstream resolver on loopback. Its handler decides the
// answer for each query.
type fakeDNSServer struct {
	addr    string
	queries atomic.Int32
}

type dnsHandler func(req []byte, viaTCP bool) []byte

// listenUDPAndTCP opens a UDP and a TCP socket on one loopback port. Windows
// excludes different port ranges for UDP and TCP, and a hosted runner excludes
// large parts of the ephemeral range. So the attempts alternate: the first
// socket of an attempt takes a port that the OS allows for its protocol, and
// the second socket binds to that port.
func listenUDPAndTCP() (net.PacketConn, net.Listener, error) {
	var err error
	for i := range 100 {
		if i%2 == 0 {
			var ln net.Listener
			if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
				return nil, nil, err
			}
			var pc net.PacketConn
			if pc, err = net.ListenPacket("udp", ln.Addr().String()); err == nil {
				return pc, ln, nil
			}
			_ = ln.Close()
			continue
		}
		var pc net.PacketConn
		if pc, err = net.ListenPacket("udp", "127.0.0.1:0"); err != nil {
			return nil, nil, err
		}
		var ln net.Listener
		if ln, err = net.Listen("tcp", pc.LocalAddr().String()); err == nil {
			return pc, ln, nil
		}
		_ = pc.Close()
	}
	return nil, nil, err
}

// startFakeDNSServer listens on UDP and TCP at one loopback port. A handler
// result of nil sends no answer.
func startFakeDNSServer(t *testing.T, handler dnsHandler) *fakeDNSServer {
	t.Helper()
	pc, ln, err := listenUDPAndTCP()
	if err != nil {
		t.Fatalf("no port is free for both UDP and TCP: %v", err)
	}
	srv := &fakeDNSServer{addr: pc.LocalAddr().String()}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			srv.queries.Add(1)
			if resp := handler(append([]byte(nil), buf[:n]...), false); resp != nil {
				_, _ = pc.WriteTo(resp, from)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var l [2]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return
				}
				req := make([]byte, binary.BigEndian.Uint16(l[:]))
				if _, err := io.ReadFull(c, req); err != nil {
					return
				}
				srv.queries.Add(1)
				resp := handler(req, true)
				if resp == nil {
					return
				}
				out := binary.BigEndian.AppendUint16(nil, uint16(len(resp)))
				_, _ = c.Write(append(out, resp...))
			}()
		}
	}()
	t.Cleanup(func() {
		_ = pc.Close()
		_ = ln.Close()
		wg.Wait()
	})
	return srv
}

// answerWith returns a handler that answers every A query with ip.
func answerWith(ip string) dnsHandler {
	return func(req []byte, _ bool) []byte {
		reqHdr, q, _, err := parseDNSQuery(req)
		if err != nil {
			return nil
		}
		return buildDNSAddressResponse(reqHdr, q, []netip.Addr{netip.MustParseAddr(ip)})
	}
}

// rcodeResponse returns a handler that answers every query with rcode.
func rcodeResponse(rcode dnsmessage.RCode) dnsHandler {
	return func(req []byte, _ bool) []byte {
		reqHdr, q, _, err := parseDNSQuery(req)
		if err != nil {
			return nil
		}
		return buildDNSErrorResponse(reqHdr, &q, rcode)
	}
}

// newTestForwarder returns a NATForwarder whose relay uses only the given upstream servers.
func newTestForwarder(t *testing.T, cfg NATConfig, upstream ...string) *NATForwarder {
	t.Helper()
	s, nf, err := NewNATStack(cfg)
	if err != nil {
		t.Fatalf("NewNATStack: %v", err)
	}
	t.Cleanup(func() {
		_ = nf.Close()
		s.Close()
	})
	// NewNATStack reads the host resolver configuration when cfg has no
	// servers. Tests replace it so they never depend on the host.
	nf.upstreamDNS = upstream
	return nf
}

func TestDNSRelayForwardsQueriesToHostServers(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.7"))
	nf := newTestForwarder(t, NATConfig{}, srv.addr)

	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(0xabcd, "example.test", dnsTypeA))
	if binary.BigEndian.Uint16(resp[0:2]) != 0xabcd || dnsRCode(resp) != dnsRCodeNoErr {
		t.Fatalf("response header = %x, want ID 0xabcd and NOERROR", resp[:dnsHeaderSize])
	}
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("answer = %v, want [198.51.100.7]", got)
	}
	if srv.queries.Load() != 1 {
		t.Fatalf("upstream saw %d queries, want 1", srv.queries.Load())
	}
}

func TestDNSRelayForwardsNonAddressQueriesUnchanged(t *testing.T) {
	var got atomic.Value
	srv := startFakeDNSServer(t, func(req []byte, _ bool) []byte {
		got.Store(append([]byte(nil), req...))
		return rcodeResponse(dnsRCodeNoErr)(req, false)
	})
	nf := newTestForwarder(t, NATConfig{}, srv.addr)

	const typeTXT = 16
	req := buildDNSQuery(7, "txt.example.test", typeTXT)
	resp := nf.handleDNSQuery(t.Context(), req)
	if dnsRCode(resp) != dnsRCodeNoErr {
		t.Fatalf("response rcode = %d, want NOERROR", dnsRCode(resp))
	}
	if binary.BigEndian.Uint16(resp[0:2]) != 7 {
		t.Fatalf("response transaction ID = %d, want 7", binary.BigEndian.Uint16(resp[0:2]))
	}
	forwarded, _ := got.Load().([]byte)
	if len(forwarded) != len(req) || string(forwarded[2:]) != string(req[2:]) {
		t.Fatalf("upstream received body %x, want %x", forwarded, req)
	}
}

func TestDNSRelayAnswersSERVFAILWithoutHostServers(t *testing.T) {
	nf := newTestForwarder(t, NATConfig{})
	req := buildDNSQuery(0x1111, "example.test", dnsTypeA)
	resp := nf.handleDNSQuery(t.Context(), req)
	if binary.BigEndian.Uint16(resp[0:2]) != 0x1111 {
		t.Fatalf("transaction ID = %x, want 1111", resp[0:2])
	}
	if flags := binary.BigEndian.Uint16(resp[2:4]); flags&dnsFlagQR == 0 || dnsRCode(resp) != dnsRCodeSrv {
		t.Fatalf("flags = %#x, want a response with SERVFAIL", flags)
	}
	if binary.BigEndian.Uint16(resp[4:6]) != 1 || string(resp[dnsHeaderSize:]) != string(req[dnsHeaderSize:]) {
		t.Fatalf("response does not echo the question: %x", resp)
	}

	if _, err := nf.forwardRawDNSUpstream(t.Context(), req); !errors.Is(err, errNoDNSServers) {
		t.Fatalf("forwardRawDNSUpstream = %v, want errNoDNSServers", err)
	}
}

func TestDNSRelayTriesNextServerAfterFailure(t *testing.T) {
	silent := startFakeDNSServer(t, func([]byte, bool) []byte { return nil })
	servfail := startFakeDNSServer(t, rcodeResponse(dnsRCodeSrv))
	refused := startFakeDNSServer(t, rcodeResponse(dnsRCodeRefus))
	good := startFakeDNSServer(t, answerWith("203.0.113.9"))
	tuned, err := tuning.Values{}.WithDuration(tuning.DNSUpstreamTimeout, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	nf := newTestForwarder(t, NATConfig{Tuning: tuned}, silent.addr, servfail.addr, refused.addr, good.addr)

	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(5, "example.test", dnsTypeA))
	if dnsRCode(resp) != dnsRCodeNoErr {
		t.Fatalf("rcode = %d, want NOERROR from the last server", dnsRCode(resp))
	}
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("203.0.113.9") {
		t.Fatalf("answer = %v, want [203.0.113.9]", got)
	}
	if silent.queries.Load() == 0 || servfail.queries.Load() != 1 || refused.queries.Load() != 1 {
		t.Fatalf("failing servers saw %d, %d, and %d queries", silent.queries.Load(), servfail.queries.Load(), refused.queries.Load())
	}
}

func TestDNSRelayAnswersSERVFAILWhenEveryServerFails(t *testing.T) {
	a := startFakeDNSServer(t, rcodeResponse(dnsRCodeSrv))
	b := startFakeDNSServer(t, rcodeResponse(dnsRCodeRefus))
	nf := newTestForwarder(t, NATConfig{}, a.addr, b.addr)
	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(5, "example.test", dnsTypeA))
	if dnsRCode(resp) != dnsRCodeSrv {
		t.Fatalf("rcode = %d, want SERVFAIL", dnsRCode(resp))
	}
}

func TestDNSRelayPassesNXDOMAINFromUpstream(t *testing.T) {
	srv := startFakeDNSServer(t, rcodeResponse(dnsRCodeNX))
	nf := newTestForwarder(t, NATConfig{}, srv.addr)
	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(5, "missing.test", dnsTypeA))
	if dnsRCode(resp) != dnsRCodeNX {
		t.Fatalf("rcode = %d, want NXDOMAIN", dnsRCode(resp))
	}
	if srv.queries.Load() != 1 {
		t.Fatalf("upstream saw %d queries, want 1: a name error ends the search", srv.queries.Load())
	}
}

func TestDNSRelayRetriesTruncatedAnswerOverTCP(t *testing.T) {
	var tcpQueries atomic.Int32
	srv := startFakeDNSServer(t, func(req []byte, viaTCP bool) []byte {
		if !viaTCP {
			resp := answerWith("192.0.2.1")(req, false)
			binary.BigEndian.PutUint16(resp[2:4], binary.BigEndian.Uint16(resp[2:4])|dnsFlagTC)
			return resp
		}
		tcpQueries.Add(1)
		return answerWith("192.0.2.200")(req, true)
	})
	nf := newTestForwarder(t, NATConfig{}, srv.addr)
	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(5, "big.test", dnsTypeA))
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("192.0.2.200") {
		t.Fatalf("answer = %v, want the TCP answer 192.0.2.200", got)
	}
	if tcpQueries.Load() != 1 {
		t.Fatalf("TCP queries = %d, want 1", tcpQueries.Load())
	}
}

func TestDNSRelayIgnoresAnswersWithWrongTransactionID(t *testing.T) {
	// A dedicated server sends a spoofed answer first, then the right one.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		req := buf[:n]
		spoof := answerWith("6.6.6.6")(req, false)
		binary.BigEndian.PutUint16(spoof[0:2], binary.BigEndian.Uint16(req[0:2])+1)
		_, _ = pc.WriteTo(spoof, from)
		_, _ = pc.WriteTo(answerWith("192.0.2.55")(req, false), from)
	}()
	nf := newTestForwarder(t, NATConfig{}, pc.LocalAddr().String())
	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(0x4242, "example.test", dnsTypeA))
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("192.0.2.55") {
		t.Fatalf("answer = %v, want the answer with the matching ID", got)
	}
}

func TestDNSRelayAnswersOwnNamesWithoutUpstream(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.99"))
	nf := newTestForwarder(t, NATConfig{}, srv.addr)
	tests := []struct {
		name  string
		qtype uint16
		want  []string
	}{
		{"localhost", dnsTypeA, []string{"127.0.0.1"}},
		{"localhost", dnsTypeAAAA, []string{"::1"}},
		{"LOCALHOST.", dnsTypeA, []string{"127.0.0.1"}},
		{"host.docker.internal", dnsTypeA, []string{HostGatewayIPv4}},
		{"gateway.docker.internal", dnsTypeA, []string{HostGatewayIPv4}},
		{"host.docker.internal", dnsTypeAAAA, nil},
		{"gvx", dnsTypeA, []string{DefaultGuestIPv4}},
	}
	for _, tt := range tests {
		resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(9, tt.name, tt.qtype))
		if dnsRCode(resp) != dnsRCodeNoErr {
			t.Fatalf("%s type %d: rcode = %d, want NOERROR", tt.name, tt.qtype, dnsRCode(resp))
		}
		var got []string
		for _, a := range answerIPs(t, resp) {
			got = append(got, a.String())
		}
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Fatalf("%s type %d: answer = %v, want %v", tt.name, tt.qtype, got, tt.want)
		}
	}
	if srv.queries.Load() != 0 {
		t.Fatalf("upstream saw %d queries for relay-owned names, want 0", srv.queries.Load())
	}

	isolated := newTestForwarder(t, NATConfig{IsolateLAN: true}, srv.addr)
	resp := isolated.handleDNSQuery(t.Context(), buildDNSQuery(9, "host.docker.internal", dnsTypeA))
	if dnsRCode(resp) != dnsRCodeNX {
		t.Fatalf("host.docker.internal under IsolateLAN: rcode = %d, want NXDOMAIN", dnsRCode(resp))
	}
}

func TestDNSRelayLookupHook(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.1"))
	nf := newTestForwarder(t, NATConfig{
		LookupHost: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "hook.test":
				return []netip.Addr{netip.MustParseAddr("192.0.2.77")}, nil
			case "absent.test":
				return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
			}
			return nil, errors.New("resolver unavailable")
		},
	}, srv.addr)

	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(1, "hook.test", dnsTypeA))
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("192.0.2.77") {
		t.Fatalf("hook answer = %v", got)
	}
	if resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(2, "absent.test", dnsTypeA)); dnsRCode(resp) != dnsRCodeNX {
		t.Fatalf("absent name: rcode = %d, want NXDOMAIN", dnsRCode(resp))
	}
	if srv.queries.Load() != 0 {
		t.Fatalf("upstream saw %d queries although the hook answered", srv.queries.Load())
	}
	// A hook error that is not a name error falls back to the host servers.
	resp = nf.handleDNSQuery(t.Context(), buildDNSQuery(3, "broken.test", dnsTypeA))
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("fallback answer = %v, want the upstream answer", got)
	}
	// The hook serves only A and AAAA. Other types go to the host servers.
	if resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(4, "hook.test", 16)); srv.queries.Load() != 2 || dnsRCode(resp) != dnsRCodeNoErr {
		t.Fatalf("TXT query: upstream queries = %d, rcode = %d, want 2 and NOERROR", srv.queries.Load(), dnsRCode(resp))
	}
}

func TestDNSRelayLocalNamesWinOverUpstream(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.1"))
	nf := newTestForwarder(t, NATConfig{
		LocalNames: func(_ context.Context, name string) ([]netip.Addr, bool) {
			switch name {
			case "db":
				return []netip.Addr{netip.MustParseAddr("172.18.0.3")}, true
			case "stopped":
				return nil, true
			}
			return nil, false
		},
	}, srv.addr)

	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(1, "DB.", dnsTypeA))
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("172.18.0.3") {
		t.Fatalf("local name answer = %v, want 172.18.0.3", got)
	}
	if resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(2, "stopped", dnsTypeA)); dnsRCode(resp) != dnsRCodeNX {
		t.Fatalf("owned name without addresses: rcode = %d, want NXDOMAIN", dnsRCode(resp))
	}
	if srv.queries.Load() != 0 {
		t.Fatalf("upstream saw %d queries for local names, want 0", srv.queries.Load())
	}
	resp = nf.handleDNSQuery(t.Context(), buildDNSQuery(3, "example.test", dnsTypeA))
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("other name = %v, want the upstream answer", got)
	}
}

func TestDNSRelayRejectsMalformedQueries(t *testing.T) {
	nf := newTestForwarder(t, NATConfig{})
	if resp := nf.handleDNSQuery(t.Context(), []byte{1, 2, 3}); resp != nil {
		t.Fatalf("short message answered with %x, want no answer", resp)
	}
	noQuestion := []byte{0, 9, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if resp := nf.handleDNSQuery(t.Context(), noQuestion); dnsRCode(resp) != dnsRCodeFmt {
		t.Fatalf("query without question: rcode = %d, want FORMERR", dnsRCode(resp))
	}
	truncated := append([]byte{0, 9, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0}, 5, 'a', 'b')
	if resp := nf.handleDNSQuery(t.Context(), truncated); dnsRCode(resp) != dnsRCodeFmt {
		t.Fatalf("truncated question: rcode = %d, want FORMERR", dnsRCode(resp))
	}
	compressed := append([]byte{0, 9, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0}, 0xc0, 0x0c, 0, 1, 0, 1)
	if resp := nf.handleDNSQuery(t.Context(), compressed); dnsRCode(resp) != dnsRCodeFmt {
		t.Fatalf("compressed question: rcode = %d, want FORMERR", dnsRCode(resp))
	}
}

func TestDNSRelayStopsWaitingWhenContextEnds(t *testing.T) {
	// The upstream reads queries and never answers. Set a 30s upstream timeout
	// so returning within testTimeout proves cancellation unblocked the relay.
	received := make(chan struct{}, 4)
	silent := startFakeDNSServer(t, func([]byte, bool) []byte {
		received <- struct{}{}
		return nil
	})
	tuned, err := tuning.Values{}.WithDuration(tuning.DNSUpstreamTimeout, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	nf := newTestForwarder(t, NATConfig{Tuning: tuned}, silent.addr)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan []byte, 1)
	go func() { done <- nf.handleDNSQuery(ctx, buildDNSQuery(1, "slow.test", dnsTypeA)) }()
	// The upstream saw the query, so the relay now waits for an answer.
	select {
	case <-received:
	case <-time.After(testTimeout):
		t.Fatal("upstream never received the query")
	}
	cancel()
	select {
	case resp := <-done:
		if dnsRCode(resp) != dnsRCodeSrv {
			t.Fatalf("rcode = %d, want SERVFAIL after cancellation", dnsRCode(resp))
		}
	case <-time.After(testTimeout):
		t.Fatal("relay kept waiting for the upstream after the context ended")
	}
}

func TestDNSUpstreamTimeoutComesFromTheTunable(t *testing.T) {
	silent := startFakeDNSServer(t, func([]byte, bool) []byte { return nil })
	tuned, err := tuning.Values{}.WithDuration(tuning.DNSUpstreamTimeout, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	nf := newTestForwarder(t, NATConfig{Tuning: tuned}, silent.addr)
	start := time.Now()
	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(1, "slow.test", dnsTypeA))
	if dnsRCode(resp) != dnsRCodeSrv {
		t.Fatalf("rcode = %d, want SERVFAIL after the timeout", dnsRCode(resp))
	}
	if took := time.Since(start); took < 90*time.Millisecond || took > testTimeout {
		t.Errorf("relay waited %v with a 100 ms upstream timeout", took)
	}
}

func TestNATDNSRelayEndToEndThroughGuestSockets(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.42"))
	s, nf, err := NewNATStack(NATConfig{DNSServers: []string{srv.addr}})
	if err != nil {
		t.Fatalf("NewNATStack: %v", err)
	}
	defer func() {
		_ = nf.Close()
		s.Close()
	}()
	dns := tcpip.FullAddress{NIC: Eth0NICID, Addr: tcpip.AddrFrom4([4]byte{10, 0, 2, 3}), Port: 53}
	query := buildDNSQuery(0x7777, "service.example.test", dnsTypeA)

	u, err := gonet.DialUDP(s, nil, &dns, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if _, err := u.Write(query); err != nil {
		t.Fatal(err)
	}
	if err := u.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := u.Read(buf)
	if err != nil {
		t.Fatalf("UDP DNS read: %v", err)
	}
	if got := answerIPs(t, buf[:n]); len(got) != 1 || got[0] != netip.MustParseAddr("198.51.100.42") {
		t.Fatalf("UDP answer = %v", got)
	}

	c, err := gonet.DialTCP(s, dns, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	framed := binary.BigEndian.AppendUint16(nil, uint16(len(query)))
	if _, err := c.Write(append(framed, query...)); err != nil {
		t.Fatal(err)
	}
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("198.51.100.42") {
		t.Fatalf("TCP answer = %v", got)
	}
}

func TestWireGuardDNSRelayForwardsToHostDNSServers(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.43"))
	guestKey, peerKey := newWGKeyPair(t), newWGKeyPair(t)
	w, err := NewWireGuardEndpoint(WireGuardConfig{
		PrivateKey:     guestKey.private,
		PeerPublicKey:  peerKey.public,
		HostDNSServers: []string{srv.addr},
	})
	if err != nil {
		t.Fatalf("NewWireGuardEndpoint: %v", err)
	}
	defer func() { _ = w.Close() }()
	s, err := w.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	dns := tcpip.FullAddress{NIC: Eth0NICID, Addr: tcpip.AddrFrom4([4]byte{10, 0, 2, 3}), Port: 53}
	u, err := gonet.DialUDP(s, nil, &dns, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if _, err := u.Write(buildDNSQuery(0x4242, "service.example.test", dnsTypeA)); err != nil {
		t.Fatal(err)
	}
	if err := u.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := u.Read(buf)
	if err != nil {
		t.Fatalf("UDP DNS read: %v", err)
	}
	if got := answerIPs(t, buf[:n]); len(got) != 1 || got[0] != netip.MustParseAddr("198.51.100.43") {
		t.Fatalf("answer = %v, want [198.51.100.43] from the host DNS server %s", got, srv.addr)
	}
}

func TestWireGuardRejectsAnInvalidHostDNSServer(t *testing.T) {
	guestKey, peerKey := newWGKeyPair(t), newWGKeyPair(t)
	_, err := NewWireGuardEndpoint(WireGuardConfig{
		PrivateKey:     guestKey.private,
		PeerPublicKey:  peerKey.public,
		HostDNSServers: []string{"dns.example.test"},
	})
	if err == nil || !strings.Contains(err.Error(), "host DNS servers") {
		t.Fatalf("NewWireGuardEndpoint with a host name as DNS server = %v, want a host DNS servers error", err)
	}
}

func TestParseDNSServersAcceptsBareIPsAndRejectsNames(t *testing.T) {
	got, err := ParseDNSServers([]string{" 10.0.2.3 ", "", "[2001:db8::1]:5353", "::1"})
	if err != nil {
		t.Fatalf("ParseDNSServers: %v", err)
	}
	want := []netip.AddrPort{
		netip.MustParseAddrPort("10.0.2.3:53"),
		netip.MustParseAddrPort("[2001:db8::1]:5353"),
		netip.MustParseAddrPort("[::1]:53"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("ParseDNSServers = %v, want %v", got, want)
	}
	if _, err := ParseDNSServers([]string{"dns.example.test"}); err == nil {
		t.Error("ParseDNSServers accepted a host name")
	}
}

func TestDNSRelayRandomizesUpstreamTXIDAndRejectsQuestionMismatch(t *testing.T) {
	seenIDs := make(map[uint16]struct{})
	var mu sync.Mutex
	srv := startFakeDNSServer(t, func(req []byte, _ bool) []byte {
		mu.Lock()
		seenIDs[binary.BigEndian.Uint16(req[0:2])] = struct{}{}
		mu.Unlock()
		return answerWith("198.51.100.12")(req, false)
	})
	nf := newTestForwarder(t, NATConfig{}, srv.addr)
	for range 16 {
		resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(0x1234, "rand.example.test", dnsTypeA))
		if binary.BigEndian.Uint16(resp[0:2]) != 0x1234 || dnsRCode(resp) != dnsRCodeNoErr {
			t.Fatalf("response = %x, want ID 0x1234 and NOERROR", resp[:dnsHeaderSize])
		}
	}
	mu.Lock()
	uniqueCount := len(seenIDs)
	mu.Unlock()
	if uniqueCount < 2 {
		t.Fatalf("upstream saw %d distinct TXIDs across 16 queries, want randomized TXIDs", uniqueCount)
	}

	// Verify UDP rejects spoofed responses that match the upstream TXID yet
	// carry a mismatched QNAME or QTYPE, while accepting case-folded QNAMEs.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		req := buf[:n]
		upID := binary.BigEndian.Uint16(req[0:2])

		wrongNameHdr, wrongNameQ, _, _ := parseDNSQuery(buildDNSQuery(upID, "evil.example.test", dnsTypeA))
		spoofName := buildDNSAddressResponse(wrongNameHdr, wrongNameQ, []netip.Addr{netip.MustParseAddr("6.6.6.1")})
		_, _ = pc.WriteTo(spoofName, from)

		wrongTypeHdr, wrongTypeQ, _, _ := parseDNSQuery(buildDNSQuery(upID, "target.example.test", dnsTypeAAAA))
		spoofType := buildDNSAddressResponse(wrongTypeHdr, wrongTypeQ, []netip.Addr{netip.MustParseAddr("2001:db8::6")})
		_, _ = pc.WriteTo(spoofType, from)

		upperHdr, upperQ, _, _ := parseDNSQuery(buildDNSQuery(upID, "TARGET.EXAMPLE.TEST", dnsTypeA))
		valid := buildDNSAddressResponse(upperHdr, upperQ, []netip.Addr{netip.MustParseAddr("198.51.100.99")})
		_, _ = pc.WriteTo(valid, from)
	}()

	nfMismatch := newTestForwarder(t, NATConfig{}, pc.LocalAddr().String())
	resp := nfMismatch.handleDNSQuery(t.Context(), buildDNSQuery(0xbeef, "target.example.test", dnsTypeA))
	if binary.BigEndian.Uint16(resp[0:2]) != 0xbeef {
		t.Fatalf("guest TXID = %#x, want 0xbeef", binary.BigEndian.Uint16(resp[0:2]))
	}
	if got := answerIPs(t, resp); len(got) != 1 || got[0] != netip.MustParseAddr("198.51.100.99") {
		t.Fatalf("answer = %v, want [198.51.100.99]", got)
	}
}

func TestDNSRelayEnforcesRFC1035QNAMELength(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.50"))
	nf := newTestForwarder(t, NATConfig{}, srv.addr)

	// 3 labels of 63 bytes + 1 label of 61 bytes: (1+63)*3 + (1+61) + 1 = 255 wire bytes.
	maxValid := strings.Join([]string{
		strings.Repeat("a", 63),
		strings.Repeat("b", 63),
		strings.Repeat("c", 63),
		strings.Repeat("d", 61),
	}, ".")
	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(1, maxValid, dnsTypeA))
	if dnsRCode(resp) != dnsRCodeNoErr {
		t.Fatalf("255-byte wire QNAME rcode = %d, want NOERROR", dnsRCode(resp))
	}

	// 1 byte longer (256 wire bytes) must be rejected with FORMERR.
	tooLong := strings.Join([]string{
		strings.Repeat("a", 63),
		strings.Repeat("b", 63),
		strings.Repeat("c", 63),
		strings.Repeat("d", 62),
	}, ".")
	resp = nf.handleDNSQuery(t.Context(), buildDNSQuery(2, tooLong, dnsTypeA))
	if dnsRCode(resp) != dnsRCodeFmt {
		t.Fatalf("256-byte wire QNAME rcode = %d, want FORMERR", dnsRCode(resp))
	}
}

func TestDNSRelayLookupHookIsolateLANBlocksRebinding(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.250"))
	nf := newTestForwarder(t, NATConfig{
		IsolateLAN: true,
		LookupHost: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "mixed.test":
				return []netip.Addr{
					netip.MustParseAddr("127.0.0.1"),
					netip.MustParseAddr("192.168.1.10"),
					netip.MustParseAddr("10.0.2.2"),
					netip.MustParseAddr("10.0.2.15"),
					netip.MustParseAddr("169.254.169.254"),
					netip.MustParseAddr("::ffff:127.0.0.1"),
					netip.MustParseAddr("198.51.100.88"),
				}, nil
			case "rebind.test":
				return []netip.Addr{
					netip.MustParseAddr("127.0.0.1"),
					netip.MustParseAddr("10.0.0.9"),
				}, nil
			}
			return nil, errors.New("unexpected name")
		},
	}, srv.addr)

	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(10, "mixed.test", dnsTypeA))
	if dnsRCode(resp) != dnsRCodeNoErr {
		t.Fatalf("mixed.test rcode = %d, want NOERROR", dnsRCode(resp))
	}
	if got := answerIPs(t, resp); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("198.51.100.88")}) {
		t.Fatalf("mixed.test answers = %v, want [198.51.100.88]", got)
	}

	resp = nf.handleDNSQuery(t.Context(), buildDNSQuery(11, "rebind.test", dnsTypeA))
	if dnsRCode(resp) != dnsRCodeNoErr {
		t.Fatalf("rebind.test rcode = %d, want NOERROR", dnsRCode(resp))
	}
	if got := answerIPs(t, resp); len(got) != 0 {
		t.Fatalf("rebind.test answers = %v, want empty", got)
	}
	if srv.queries.Load() != 0 {
		t.Fatalf("upstream queries = %d, want 0", srv.queries.Load())
	}
}

type closeTrackingConn struct {
	net.Conn
	closed atomic.Int32
}

func (c *closeTrackingConn) Close() error {
	c.closed.Add(1)
	return c.Conn.Close()
}

func TestNATForwarderServeUDPDNSClosesConnOnIdleTimeout(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.33"))
	tuned, err := tuning.Values{}.WithDuration(tuning.DNSTimeout, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	nf := newTestForwarder(t, NATConfig{Tuning: tuned}, srv.addr)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	tracked := &closeTrackingConn{Conn: serverConn}

	done := make(chan struct{})
	go func() {
		defer close(done)
		nf.serveUDPDNS(nf.ctx, tracked)
	}()

	req := buildDNSQuery(0x3131, "idle.example.test", dnsTypeA)
	if _, err := clientConn.Write(req); err != nil {
		t.Fatalf("write DNS query: %v", err)
	}
	buf := make([]byte, 512)
	_ = clientConn.SetReadDeadline(time.Now().Add(testTimeout))
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("read DNS reply: %v", err)
	}
	if got := answerIPs(t, buf[:n]); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("198.51.100.33")}) {
		t.Fatalf("answer = %v, want [198.51.100.33]", got)
	}

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("serveUDPDNS did not exit after idle timeout")
	}
	if tracked.closed.Load() == 0 {
		t.Fatal("serveUDPDNS returned on idle timeout without closing conn")
	}
}

func TestDNSRelayConcurrentUDPFlood(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	var (
		inFlight       atomic.Int32
		peakInFlight   atomic.Int32
		releaseGate    = make(chan struct{})
		saturationOnce sync.Once
		srvWG          sync.WaitGroup
	)
	srvWG.Add(1)
	go func() {
		defer srvWG.Done()
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			srvWG.Add(1)
			go func(req []byte, peer net.Addr) {
				defer srvWG.Done()
				cur := inFlight.Add(1)
				for {
					prev := peakInFlight.Load()
					if cur <= prev || peakInFlight.CompareAndSwap(prev, cur) {
						break
					}
				}
				if cur >= maxConcurrentDNS {
					saturationOnce.Do(func() { close(releaseGate) })
				}
				select {
				case <-releaseGate:
				case <-t.Context().Done():
				}
				inFlight.Add(-1)
				if resp := answerWith("198.51.100.77")(req, false); resp != nil {
					_, _ = pc.WriteTo(resp, peer)
				}
			}(pkt, from)
		}
	}()
	t.Cleanup(func() {
		saturationOnce.Do(func() { close(releaseGate) })
		_ = pc.Close()
		srvWG.Wait()
	})

	guestKey, peerKey := newWGKeyPair(t), newWGKeyPair(t)
	w, err := NewWireGuardEndpoint(WireGuardConfig{
		PrivateKey:     guestKey.private,
		PeerPublicKey:  peerKey.public,
		HostDNSServers: []string{pc.LocalAddr().String()},
	})
	if err != nil {
		t.Fatalf("NewWireGuardEndpoint: %v", err)
	}
	defer func() { _ = w.Close() }()
	s, err := w.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	const floodCount = 200
	dnsAddr := tcpip.FullAddress{NIC: Eth0NICID, Addr: tcpip.AddrFrom4([4]byte{10, 0, 2, 3}), Port: 53}

	var (
		clientWG sync.WaitGroup
		errCh    = make(chan error, floodCount)
	)
	for i := range floodCount {
		clientWG.Add(1)
		go func(idx int) {
			defer clientWG.Done()
			u, err := gonet.DialUDP(s, nil, &dnsAddr, ipv4.ProtocolNumber)
			if err != nil {
				errCh <- err
				return
			}
			defer u.Close()

			txID := uint16(0x1000 + idx)
			if _, err := u.Write(buildDNSQuery(txID, "flood.example.test", dnsTypeA)); err != nil {
				errCh <- err
				return
			}
			if err := u.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
				errCh <- err
				return
			}
			buf := make([]byte, 512)
			n, err := u.Read(buf)
			if err != nil {
				errCh <- err
				return
			}
			if gotID := binary.BigEndian.Uint16(buf[0:2]); gotID != txID {
				errCh <- errors.New("mismatched guest transaction ID in flood reply")
				return
			}
			if got := answerIPs(t, buf[:n]); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("198.51.100.77")}) {
				errCh <- errors.New("unexpected DNS flood answer IP")
			}
		}(i)
	}
	clientWG.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("UDP DNS flood query failed: %v", err)
	}
	if peak := peakInFlight.Load(); peak != maxConcurrentDNS {
		t.Fatalf("peak concurrent upstream DNS queries = %d, want %d", peak, maxConcurrentDNS)
	}
}

func TestDNSRelaySemaphoreCancellationSkipsUpstream(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.11"))
	nf := newTestForwarder(t, NATConfig{}, srv.addr)

	sem := nf.dnsSemaphore()
	for range maxConcurrentDNS {
		sem <- struct{}{}
	}
	t.Cleanup(func() {
		for len(sem) > 0 {
			<-sem
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if resp := nf.handleDNSQuery(ctx, buildDNSQuery(1, "blocked.example.test", dnsTypeA)); resp != nil {
		t.Fatalf("cancelled handleDNSQuery while semaphore full returned %x, want nil", resp)
	}
	if srv.queries.Load() != 0 {
		t.Fatalf("upstream saw %d queries despite saturated semaphore cancellation, want 0", srv.queries.Load())
	}

	guestKey, peerKey := newWGKeyPair(t), newWGKeyPair(t)
	w, err := NewWireGuardEndpoint(WireGuardConfig{
		PrivateKey:     guestKey.private,
		PeerPublicKey:  peerKey.public,
		HostDNSServers: []string{srv.addr},
	})
	if err != nil {
		t.Fatalf("NewWireGuardEndpoint: %v", err)
	}
	s, err := w.Start()
	if err != nil {
		_ = w.Close()
		t.Fatalf("Start: %v", err)
	}

	wgSem := w.dnsSemaphore()
	for range maxConcurrentDNS {
		wgSem <- struct{}{}
	}

	dnsAddr := tcpip.FullAddress{NIC: Eth0NICID, Addr: tcpip.AddrFrom4([4]byte{10, 0, 2, 3}), Port: 53}
	tcpConn, err := gonet.DialTCP(s, dnsAddr, ipv4.ProtocolNumber)
	if err != nil {
		_ = w.Close()
		t.Fatalf("DialTCP: %v", err)
	}
	defer tcpConn.Close()

	req := buildDNSQuery(2, "wg-blocked.example.test", dnsTypeA)
	framed := binary.BigEndian.AppendUint16(nil, uint16(len(req)))
	if _, err := tcpConn.Write(append(framed, req...)); err != nil {
		_ = w.Close()
		t.Fatalf("write TCP DNS query: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- w.Close()
	}()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("w.Close(): %v", err)
		}
	case <-time.After(testTimeout):
		for len(wgSem) > 0 {
			<-wgSem
		}
		t.Fatal("WireGuardEndpoint.Close hung waiting for TCP DNS listener with saturated semaphore")
	}
	if srv.queries.Load() != 0 {
		t.Fatalf("upstream saw %d queries after saturated TCP DNS cancellation, want 0", srv.queries.Load())
	}
}

func TestDNSRelayForwardsCNAMEAndPTRAnswersFromUpstream(t *testing.T) {
	srv := startFakeDNSServer(t, func(req []byte, _ bool) []byte {
		reqHdr, q, _, err := parseDNSQuery(req)
		if err != nil {
			return nil
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
			ID:                 reqHdr.ID,
			Response:           true,
			RecursionDesired:   reqHdr.RecursionDesired,
			RecursionAvailable: true,
			RCode:              dnsmessage.RCodeSuccess,
		})
		b.EnableCompression()
		if err := b.StartQuestions(); err != nil {
			return nil
		}
		if err := b.Question(q); err != nil {
			return nil
		}
		if err := b.StartAnswers(); err != nil {
			return nil
		}
		switch q.Type {
		case dnsmessage.TypeA:
			target := dnsmessage.MustNewName("target.example.test.")
			_ = b.CNAMEResource(dnsmessage.ResourceHeader{
				Name:  q.Name,
				Type:  dnsmessage.TypeCNAME,
				Class: dnsmessage.ClassINET,
				TTL:   60,
			}, dnsmessage.CNAMEResource{CNAME: target})
			_ = b.AResource(dnsmessage.ResourceHeader{
				Name:  target,
				Type:  dnsmessage.TypeA,
				Class: dnsmessage.ClassINET,
				TTL:   60,
			}, dnsmessage.AResource{A: [4]byte{198, 51, 100, 88}})
		case dnsmessage.TypePTR:
			_ = b.PTRResource(dnsmessage.ResourceHeader{
				Name:  q.Name,
				Type:  dnsmessage.TypePTR,
				Class: dnsmessage.ClassINET,
				TTL:   60,
			}, dnsmessage.PTRResource{PTR: dnsmessage.MustNewName("ptr.example.test.")})
		}
		out, err := b.Finish()
		if err != nil {
			return nil
		}
		return out
	})
	nf := newTestForwarder(t, NATConfig{}, srv.addr)

	respA := nf.handleDNSQuery(t.Context(), buildDNSQuery(0x2222, "alias.example.test", dnsTypeA))
	if got := answerIPs(t, respA); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("198.51.100.88")}) {
		t.Fatalf("CNAME+A answerIPs = %v, want [198.51.100.88]", got)
	}

	respPTR := nf.handleDNSQuery(t.Context(), buildDNSQuery(0x3333, "88.100.51.198.in-addr.arpa", uint16(dnsmessage.TypePTR)))
	var p dnsmessage.Parser
	hdr, err := p.Start(respPTR)
	if err != nil || hdr.ID != 0x3333 || hdr.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("PTR response header = %+v, err = %v", hdr, err)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatalf("SkipAllQuestions: %v", err)
	}
	ansHdr, err := p.AnswerHeader()
	if err != nil || ansHdr.Type != dnsmessage.TypePTR {
		t.Fatalf("AnswerHeader = %+v, err = %v", ansHdr, err)
	}
	ptrRes, err := p.PTRResource()
	if err != nil || ptrRes.PTR.String() != "ptr.example.test." {
		t.Fatalf("PTRResource = %+v, err = %v", ptrRes, err)
	}
}

// fallbackRecorder is a FallbackLookupHost that answers from a fixed table
// and records the names that it was asked for.
type fallbackRecorder struct {
	mu    sync.Mutex
	names []string
	addrs map[string][]netip.Addr
}

func (f *fallbackRecorder) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, host)
	if addrs, ok := f.addrs[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (f *fallbackRecorder) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.names)
}

func dnsBudget(t *testing.T, budget time.Duration) tuning.Values {
	t.Helper()
	tuned, err := tuning.Values{}.WithDuration(tuning.DNSTimeout, budget)
	if err != nil {
		t.Fatal(err)
	}
	return tuned
}

// TestDNSRelayFallsBackWhenEveryServerTimesOut uses two servers that never
// answer. Each gets a share of the query budget, so the fallback still
// answers the guest inside the budget.
func TestDNSRelayFallsBackWhenEveryServerTimesOut(t *testing.T) {
	a := startFakeDNSServer(t, func([]byte, bool) []byte { return nil })
	b := startFakeDNSServer(t, func([]byte, bool) []byte { return nil })
	fb := &fallbackRecorder{addrs: map[string][]netip.Addr{
		"registry.example.": {netip.MustParseAddr("198.51.100.20"), netip.MustParseAddr("2001:db8::20")},
	}}
	// The fallback keeps a quarter of the budget, so the answer comes a
	// quarter early: 500 ms of margin for a loaded CI runner.
	const budget = 2 * time.Second
	nf := newTestForwarder(t, NATConfig{Tuning: dnsBudget(t, budget), FallbackLookupHost: fb.lookup}, a.addr, b.addr)

	start := time.Now()
	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(3, "registry.example", dnsTypeA))
	took := time.Since(start)
	if dnsRCode(resp) != dnsRCodeNoErr {
		t.Fatalf("rcode = %d after %v, want NOERROR from the fallback", dnsRCode(resp), took)
	}
	if got := answerIPs(t, resp); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("198.51.100.20")}) {
		t.Fatalf("answer = %v, want [198.51.100.20]", got)
	}
	if took >= budget {
		t.Fatalf("relay answered after %v, want less than the %v budget", took, budget)
	}
	if a.queries.Load() == 0 || b.queries.Load() == 0 {
		t.Fatalf("servers saw %d and %d queries, want both asked first", a.queries.Load(), b.queries.Load())
	}
	if got := fb.asked(); !slices.Equal(got, []string{"registry.example."}) {
		t.Fatalf("fallback asked for %q, want the fully qualified name once", got)
	}
}

// TestDNSRelayFallsBackAfterSERVFAIL answers an AAAA query through the
// fallback when the only server answers SERVFAIL. A name that the fallback
// cannot resolve keeps SERVFAIL.
func TestDNSRelayFallsBackAfterSERVFAIL(t *testing.T) {
	srv := startFakeDNSServer(t, rcodeResponse(dnsRCodeSrv))
	fb := &fallbackRecorder{addrs: map[string][]netip.Addr{
		"registry.example.": {netip.MustParseAddr("198.51.100.20"), netip.MustParseAddr("2001:db8::20")},
	}}
	nf := newTestForwarder(t, NATConfig{FallbackLookupHost: fb.lookup}, srv.addr)

	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(4, "registry.example", dnsTypeAAAA))
	if dnsRCode(resp) != dnsRCodeNoErr {
		t.Fatalf("rcode = %d, want NOERROR from the fallback", dnsRCode(resp))
	}
	if got := answerIPs(t, resp); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("2001:db8::20")}) {
		t.Fatalf("answer = %v, want [2001:db8::20]", got)
	}
	// A failed fallback keeps SERVFAIL. A blocked host resolver also says
	// "no such host", and a false NXDOMAIN stays in the guest cache.
	resp = nf.handleDNSQuery(t.Context(), buildDNSQuery(5, "missing.example", dnsTypeA))
	if dnsRCode(resp) != dnsRCodeSrv {
		t.Fatalf("missing name: rcode = %d, want SERVFAIL", dnsRCode(resp))
	}
}

// TestDNSRelayKeepsUpstreamAnswersAndNonAddressFailures proves that the
// fallback runs only for A and AAAA queries that no server answered.
func TestDNSRelayKeepsUpstreamAnswersAndNonAddressFailures(t *testing.T) {
	nx := startFakeDNSServer(t, rcodeResponse(dnsRCodeNX))
	fb := &fallbackRecorder{addrs: map[string][]netip.Addr{"gone.example.": {netip.MustParseAddr("198.51.100.30")}}}
	nf := newTestForwarder(t, NATConfig{FallbackLookupHost: fb.lookup}, nx.addr)
	if resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(6, "gone.example", dnsTypeA)); dnsRCode(resp) != dnsRCodeNX {
		t.Fatalf("rcode = %d, want the NXDOMAIN of the server", dnsRCode(resp))
	}

	fail := startFakeDNSServer(t, rcodeResponse(dnsRCodeSrv))
	nf = newTestForwarder(t, NATConfig{FallbackLookupHost: fb.lookup}, fail.addr)
	const typeTXT = 16
	if resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(7, "gone.example", typeTXT)); dnsRCode(resp) != dnsRCodeSrv {
		t.Fatalf("TXT rcode = %d, want SERVFAIL", dnsRCode(resp))
	}
	if got := fb.asked(); len(got) != 0 {
		t.Fatalf("fallback asked for %q, want no call", got)
	}
}

// TestDNSRelayFallbackObeysIsolateLAN filters LAN and loopback addresses out
// of a fallback answer, as for the LookupHost hook.
func TestDNSRelayFallbackObeysIsolateLAN(t *testing.T) {
	srv := startFakeDNSServer(t, rcodeResponse(dnsRCodeSrv))
	fb := &fallbackRecorder{addrs: map[string][]netip.Addr{"rebind.example.": {
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("172.16.5.10"),
		netip.MustParseAddr("10.0.2.2"),
		netip.MustParseAddr("169.254.169.254"),
		netip.MustParseAddr("198.51.100.40"),
	}}}
	nf := newTestForwarder(t, NATConfig{IsolateLAN: true, FallbackLookupHost: fb.lookup}, srv.addr)
	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(8, "rebind.example", dnsTypeA))
	if got := answerIPs(t, resp); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("198.51.100.40")}) {
		t.Fatalf("answer = %v, want only [198.51.100.40]", got)
	}
}

// TestDNSRelaySlowServerLeavesTimeForTheNext puts two silent servers before
// a working one. Without a fallback, each server still gets only a share of
// the query budget, so the third server answers inside it.
func TestDNSRelaySlowServerLeavesTimeForTheNext(t *testing.T) {
	a := startFakeDNSServer(t, func([]byte, bool) []byte { return nil })
	b := startFakeDNSServer(t, func([]byte, bool) []byte { return nil })
	good := startFakeDNSServer(t, answerWith("203.0.113.50"))
	// The fallback keeps a quarter of the budget, so the answer comes a
	// quarter early: 500 ms of margin for a loaded CI runner.
	const budget = 2 * time.Second
	nf := newTestForwarder(t, NATConfig{Tuning: dnsBudget(t, budget)}, a.addr, b.addr, good.addr)

	start := time.Now()
	resp := nf.handleDNSQuery(t.Context(), buildDNSQuery(9, "slow.example", dnsTypeA))
	took := time.Since(start)
	if got := answerIPs(t, resp); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("203.0.113.50")}) {
		t.Fatalf("answer = %v after %v, want [203.0.113.50] from the third server", got, took)
	}
	if took >= budget {
		t.Fatalf("relay answered after %v, want less than the %v budget", took, budget)
	}
}

// TestWireGuardDNSRelayHasNoFallback keeps every guest query of the
// WireGuard backend on its configured servers: a fallback through the host
// resolver would send the name outside the tunnel.
func TestWireGuardDNSRelayHasNoFallback(t *testing.T) {
	guestKey, peerKey := newWGKeyPair(t), newWGKeyPair(t)
	w, err := NewWireGuardEndpoint(WireGuardConfig{
		PrivateKey:     guestKey.private,
		PeerPublicKey:  peerKey.public,
		DNSServers:     []string{"192.0.2.53"},
		HostDNSServers: []string{"192.0.2.54"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if w.dnsRelay(nil).fallback != nil {
		t.Fatal("the WireGuard DNS relay has a fallback")
	}
}

// TestNATDNSRelayServesManyShortLookups sends one query from each of many
// guest UDP sockets in turn, as fast as the relay answers, as the Go and
// musl resolvers do: they open a socket for each query. A flow must end soon
// after it answered, so the flows of past lookups do not fill the flow limit
// and drop new queries.
func TestNATDNSRelayServesManyShortLookups(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.42"))
	s, nf, err := NewNATStack(NATConfig{DNSServers: []string{srv.addr}})
	if err != nil {
		t.Fatalf("NewNATStack: %v", err)
	}
	defer func() {
		_ = nf.Close()
		s.Close()
	}()
	dns := tcpip.FullAddress{NIC: Eth0NICID, Addr: tcpip.AddrFrom4([4]byte{10, 0, 2, 3}), Port: 53}
	const lookups = 3 * maxNATDNSFlows
	buf := make([]byte, 512)
	for i := 0; i < lookups; i++ {
		u, err := gonet.DialUDP(s, nil, &dns, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := u.Write(buildDNSQuery(uint16(i), "service.example.test", dnsTypeA)); err != nil {
			t.Fatal(err)
		}
		_ = u.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := u.Read(buf)
		_ = u.Close()
		if err != nil {
			t.Fatalf("lookup %d of %d: %v", i, lookups, err)
		}
		if got := answerIPs(t, buf[:n]); len(got) != 1 {
			t.Fatalf("lookup %d answer = %v", i, got)
		}
	}
}

// TestNATDNSRelayAnswersAQueryAfterTheFlowLinger sends two queries on one
// guest UDP socket, 50 ms apart, past udpDNSFlowLinger, as glibc does with
// "options single-request" under load. The first flow ends between them, so
// the second datagram opens a new flow. It must get an answer on the first
// try, well before a resolver would retry.
func TestNATDNSRelayAnswersAQueryAfterTheFlowLinger(t *testing.T) {
	srv := startFakeDNSServer(t, answerWith("198.51.100.42"))
	s, nf, err := NewNATStack(NATConfig{DNSServers: []string{srv.addr}})
	if err != nil {
		t.Fatalf("NewNATStack: %v", err)
	}
	defer func() {
		_ = nf.Close()
		s.Close()
	}()
	dns := tcpip.FullAddress{NIC: Eth0NICID, Addr: tcpip.AddrFrom4([4]byte{10, 0, 2, 3}), Port: 53}
	u, err := gonet.DialUDP(s, nil, &dns, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	buf := make([]byte, 512)
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(5 * udpDNSFlowLinger)
		}
		if _, err := u.Write(buildDNSQuery(uint16(100+i), "service.example.test", dnsTypeA)); err != nil {
			t.Fatal(err)
		}
		// A resolver retries after 1 s at the earliest; one try must do.
		_ = u.SetReadDeadline(time.Now().Add(time.Second))
		n, err := u.Read(buf)
		if err != nil {
			t.Fatalf("query %d on the same socket after the linger: %v", i, err)
		}
		if got := answerIPs(t, buf[:n]); len(got) != 1 {
			t.Fatalf("query %d answer = %v", i, got)
		}
	}
}
