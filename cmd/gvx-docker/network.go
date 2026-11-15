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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"z.gfire.dev/gvx/pkg/gvx"
)

// Each container gets one virtual IPv4 address on each of its networks. The
// guest keeps its own NAT address. A guest finds another container through
// the DNS relay of its sandbox. A connection to a virtual address leaves the
// guest through the NAT forwarder, and the shim routes it to the control
// socket of the target shim, which dials the guest of the target.

const (
	defaultNetworkName = "bridge"
	composeServiceKey  = "com.docker.compose.service"
)

type networkRecord struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver"`
	Subnet   string            `json:"Subnet"`
	Gateway  string            `json:"Gateway"`
	Internal bool              `json:"Internal"`
	Labels   map[string]string `json:"Labels"`
	Created  time.Time         `json:"Created"`
}

type endpointSettings struct {
	// Internal reports a network without a way out.
	Internal  bool     `json:"Internal,omitempty"`
	NetworkID string   `json:"NetworkID"`
	IPAddress string   `json:"IPAddress"`
	Gateway   string   `json:"Gateway"`
	Subnet    string   `json:"Subnet,omitempty"`
	Aliases   []string `json:"Aliases"`
}

type networkStore struct {
	root string
	mu   sync.Mutex
}

func newNetworkStore(stateDir string) *networkStore {
	return &networkStore{root: filepath.Join(stateDir, "networks")}
}

func (s *networkStore) path(id string) string { return filepath.Join(s.root, id+".json") }

func (s *networkStore) list() ([]networkRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaultLocked(); err != nil {
		return nil, err
	}
	return s.listLocked()
}

func (s *networkStore) listLocked() ([]networkRecord, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []networkRecord
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var rec networkRecord
		if readJSONFile(filepath.Join(s.root, e.Name()), &rec) == nil {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

func (s *networkStore) ensureDefaultLocked() error {
	recs, err := s.listLocked()
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.Name == defaultNetworkName {
			return nil
		}
	}
	_, err = s.createLocked(defaultNetworkName, nil, false)
	return err
}

func (s *networkStore) get(ref string) (networkRecord, error) {
	recs, err := s.list()
	if err != nil {
		return networkRecord{}, err
	}
	for _, r := range recs {
		if r.ID == ref || r.Name == ref || (len(ref) >= 3 && strings.HasPrefix(r.ID, ref)) {
			return r, nil
		}
	}
	return networkRecord{}, fmt.Errorf("%w: network %s not found", errNotFound, ref)
}

func (s *networkStore) create(name string, labels map[string]string, internal bool) (networkRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaultLocked(); err != nil {
		return networkRecord{}, err
	}
	recs, err := s.listLocked()
	if err != nil {
		return networkRecord{}, err
	}
	for _, r := range recs {
		if r.Name == name {
			return networkRecord{}, fmt.Errorf("%w: network with name %s already exists", errConflict, name)
		}
	}
	return s.createLocked(name, labels, internal)
}

// createLocked takes the next free subnet: 172.17.0.0/16 for the default
// network, then 172.18 to 172.31, then 192.168.N.0/20.
func (s *networkStore) createLocked(name string, labels map[string]string, internal bool) (networkRecord, error) {
	recs, err := s.listLocked()
	if err != nil {
		return networkRecord{}, err
	}
	used := map[string]bool{}
	for _, r := range recs {
		used[r.Subnet] = true
	}
	var candidates []string
	for i := 17; i <= 31; i++ {
		candidates = append(candidates, fmt.Sprintf("172.%d.0.0/16", i))
	}
	for i := 0; i < 256; i += 16 {
		candidates = append(candidates, fmt.Sprintf("192.168.%d.0/20", i))
	}
	subnet := ""
	for _, c := range candidates {
		if !used[c] {
			subnet = c
			break
		}
	}
	if subnet == "" {
		return networkRecord{}, errors.New("no free network subnet")
	}
	prefix := netip.MustParsePrefix(subnet)
	id, err := newContainerID()
	if err != nil {
		return networkRecord{}, err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	rec := networkRecord{
		ID: id, Name: name, Driver: "bridge", Subnet: subnet,
		Gateway: prefix.Addr().Next().String(), Internal: internal,
		Labels: labels, Created: time.Now().UTC(),
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return networkRecord{}, err
	}
	return rec, writeJSONFile(s.path(id), rec)
}

// remove deletes a network. A network that another request removed already
// counts as removed.
func (s *networkStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// allocateIP returns a free address of the network, skipping the gateway.
func allocateIP(network networkRecord, containers []containerRecord) (string, error) {
	prefix, err := netip.ParsePrefix(network.Subnet)
	if err != nil {
		return "", err
	}
	used := map[string]bool{network.Gateway: true}
	for _, c := range containers {
		if ep, ok := c.Networks[network.Name]; ok {
			used[ep.IPAddress] = true
		}
	}
	for addr := prefix.Addr().Next().Next(); prefix.Contains(addr); addr = addr.Next() {
		if !used[addr.String()] {
			return addr.String(), nil
		}
	}
	return "", fmt.Errorf("network %s has no free address", network.Name)
}

// containerNames returns the names that other containers on its networks use
// for c: the container name, the ID prefix, the hostname, the Compose service,
// and the aliases of the network.
func containerNames(c containerRecord, network string) []string {
	names := []string{c.Name, c.ID[:12], c.Config.Hostname}
	if svc := c.Config.Labels[composeServiceKey]; svc != "" {
		names = append(names, svc)
	}
	if ep, ok := c.Networks[network]; ok {
		names = append(names, ep.Aliases...)
	}
	return names
}

// resolveName returns the addresses of a running container that shares a
// network with self and has the name.
func resolveName(self containerRecord, all []containerRecord, name string) []netip.Addr {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	var out []netip.Addr
	for netName, selfEP := range self.Networks {
		for _, c := range all {
			if !c.State.Running {
				continue
			}
			ep, ok := c.Networks[netName]
			if !ok || ep.NetworkID != selfEP.NetworkID {
				continue
			}
			for _, n := range containerNames(c, netName) {
				if n != "" && strings.ToLower(n) == name {
					if addr, err := netip.ParseAddr(ep.IPAddress); err == nil {
						out = append(out, addr)
					}
					break
				}
			}
		}
	}
	return out
}

// routeAddress returns the running container that owns addr on a network that
// self shares.
func routeAddress(self containerRecord, all []containerRecord, addr string) (containerRecord, bool) {
	for netName, selfEP := range self.Networks {
		for _, c := range all {
			if !c.State.Running {
				continue
			}
			if ep, ok := c.Networks[netName]; ok && ep.NetworkID == selfEP.NetworkID && ep.IPAddress == addr {
				return c, true
			}
		}
	}
	return containerRecord{}, false
}

// sourceAddress returns the address of self on a network that it shares with
// target.
func sourceAddress(self, target containerRecord) (netip.Addr, bool) {
	for name, ep := range self.Networks {
		if tep, ok := target.Networks[name]; ok && tep.NetworkID == ep.NetworkID {
			if addr, err := netip.ParseAddr(ep.IPAddress); err == nil {
				return addr, true
			}
		}
	}
	return netip.Addr{}, false
}

// isNetworkAddress reports whether addr lies in a subnet of a network of self.
func isNetworkAddress(self containerRecord, addr netip.Addr) bool {
	for _, ep := range self.Networks {
		if prefix, err := netip.ParsePrefix(ep.Subnet); err == nil && prefix.Contains(addr) {
			return true
		}
	}
	return false
}

type netRequest struct {
	Op    string `json:"op"`
	Name  string `json:"name,omitempty"`
	Addr  string `json:"addr,omitempty"`
	Port  uint16 `json:"port,omitempty"`
	Proto string `json:"proto,omitempty"`
	// SrcPort is the source port of the guest connection.
	SrcPort uint16 `json:"srcPort,omitempty"`
}

type netResponse struct {
	Addrs []string `json:"addrs,omitempty"`
	Found bool     `json:"found"`
	Error string   `json:"error,omitempty"`
}

// The supervisor and its shim share one connected socket pair. The shim
// inherits its end, because the host lockdown denies new Unix connections.
// The shim sends one JSON request per line and reads one JSON response per
// line, in order. A "dial" response passes a connected socket to the target
// guest with SCM_RIGHTS.
//
// A dial can wait for seconds on a target that does not answer, and the
// requests on one connection run in order. The shim therefore sends name
// lookups on the shared connection and asks for a "session" for each dial:
// the supervisor creates a new socket pair, serves one end like the shared
// connection, and passes the other end with SCM_RIGHTS. No request on the
// shared connection waits for a dial.

func serveNetworkConn(ctx context.Context, conn *net.UnixConn, stateDir string, store *containerStore, id string) {
	// The connection closes when the shim closes its end or the run ends.
	// A dial session lives for one request, so a short connection to
	// another container would leak one socket otherwise.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() {
		stop()
		_ = conn.Close()
	}()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req netRequest
		if json.Unmarshal(line, &req) != nil {
			return
		}
		resp, file := answerNetwork(ctx, stateDir, store, id, req)
		data, _ := json.Marshal(resp)
		data = append(data, '\n')
		if file != nil {
			_, _, err = conn.WriteMsgUnix(data, unixRights(file), nil)
			_ = file.Close()
		} else {
			_, err = conn.Write(data)
		}
		if err != nil {
			return
		}
	}
}

func answerNetwork(ctx context.Context, stateDir string, store *containerStore, id string, req netRequest) (netResponse, *os.File) {
	self, err := store.get(id)
	if err != nil {
		return netResponse{}, nil
	}
	all, err := store.list()
	if err != nil {
		return netResponse{}, nil
	}
	switch req.Op {
	case "resolve":
		addrs := resolveName(self, all, req.Name)
		resp := netResponse{Found: len(addrs) > 0}
		for _, a := range addrs {
			resp.Addrs = append(resp.Addrs, a.String())
		}
		return resp, nil
	case "unix":
		// Only a socket that a bind of the container names may be dialed.
		_, sockets := socketBinds(self.HostConfig.Binds, engineSocketLink(stateDir))
		allowed := false
		for _, src := range sockets {
			if src == req.Addr {
				allowed = true
			}
		}
		if !allowed {
			return netResponse{Error: "not a socket bind of the container"}, nil
		}
		conn, err := net.Dial("unix", req.Addr)
		if err != nil {
			return netResponse{Found: true, Error: err.Error()}, nil
		}
		f, err := conn.(*net.UnixConn).File()
		_ = conn.Close()
		if err != nil {
			return netResponse{Found: true, Error: err.Error()}, nil
		}
		return netResponse{Found: true}, f
	case "session":
		conn, child, err := socketPair()
		if err != nil {
			return netResponse{Error: err.Error()}, nil
		}
		syscall.CloseOnExec(int(child.Fd()))
		go serveNetworkConn(ctx, conn, stateDir, store, id)
		return netResponse{Found: true}, child
	case "dial":
		target, ok := routeAddress(self, all, req.Addr)
		if !ok {
			return netResponse{}, nil
		}
		dialReq := controlRequest{Op: "dial", Port: req.Port, Proto: req.Proto}
		if src, ok := sourceAddress(self, target); ok {
			// The target guest sees the address of the caller on the shared
			// network, as on a Docker bridge.
			dialReq.Src = netip.AddrPortFrom(src, req.SrcPort).String()
			dialReq.Dst = net.JoinHostPort(req.Addr, strconv.Itoa(int(req.Port)))
		}
		// The bare connection goes to the shim of the caller, so no reply
		// byte may stay in a buffer of this process.
		conn, err := controlCallRaw(ctx, controlSocketPath(stateDir, target.ID), dialReq)
		if err != nil {
			return netResponse{Found: true, Error: err.Error()}, nil
		}
		uc, ok := conn.(*net.UnixConn)
		if !ok {
			_ = conn.Close()
			return netResponse{Found: true, Error: "control connection is not a Unix socket"}, nil
		}
		f, err := uc.File()
		_ = uc.Close()
		if err != nil {
			return netResponse{Found: true, Error: err.Error()}, nil
		}
		return netResponse{Found: true}, f
	}
	return netResponse{}, nil
}

// netClient is the shim end of the supervisor connection.
type netClient struct {
	mu     sync.Mutex
	conn   *net.UnixConn
	reader *bufio.Reader
}

func (c *netClient) call(req netRequest) (netResponse, *os.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, _ := json.Marshal(req)
	if _, err := c.conn.Write(append(data, '\n')); err != nil {
		return netResponse{}, nil, err
	}
	buf := make([]byte, 64*1024)
	oob := make([]byte, 64)
	n, oobn, _, _, err := c.conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return netResponse{}, nil, err
	}
	var resp netResponse
	if err := json.Unmarshal(bytes.TrimSpace(buf[:n]), &resp); err != nil {
		return netResponse{}, nil, err
	}
	return resp, parseRights(oob[:oobn]), nil
}

// session opens a supervisor connection of its own for one request that can
// take long, so the shared connection stays free for name lookups.
func (c *netClient) session() (*netClient, error) {
	resp, file, err := c.call(netRequest{Op: "session"})
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("open a supervisor session: %s", resp.Error)
	}
	conn, err := fileUnixConn(file)
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	return &netClient{conn: conn}, nil
}

// callInSession sends one request on a new session and closes the session.
func (c *netClient) callInSession(req netRequest) (netResponse, *os.File, error) {
	s, err := c.session()
	if err != nil {
		return netResponse{}, nil, err
	}
	defer func() { _ = s.conn.Close() }()
	return s.call(req)
}

// dialUnix asks the supervisor for a connection to a host Unix socket.
func (c *netClient) dialUnix(path string) (net.Conn, error) {
	resp, file, err := c.callInSession(netRequest{Op: "unix", Addr: path})
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("connect to %s: %s", path, resp.Error)
	}
	defer func() { _ = file.Close() }()
	return net.FileConn(file)
}

// shimNetworkHooks returns the DNS and dial hooks of a shim.
func shimNetworkHooks(client *netClient, rec containerRecord, internal bool) (func(context.Context, string) ([]netip.Addr, bool), func(context.Context, string, string) (net.Conn, bool, error)) {
	if client == nil || len(rec.Networks) == 0 {
		return nil, nil
	}
	lookup := func(_ context.Context, name string) ([]netip.Addr, bool) {
		if strings.Contains(strings.TrimSuffix(name, "."), ".") {
			return nil, false
		}
		resp, _, err := client.call(netRequest{Op: "resolve", Name: name})
		if err != nil || !resp.Found {
			return nil, false
		}
		var addrs []netip.Addr
		for _, a := range resp.Addrs {
			if addr, err := netip.ParseAddr(a); err == nil {
				addrs = append(addrs, addr)
			}
		}
		return addrs, true
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, bool, error) {
		if network != "tcp" && network != "udp" {
			return nil, false, nil
		}
		host, portText, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, false, nil
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !isNetworkAddress(rec, ip) {
			if internal {
				// Every network of the container is internal, so it has no
				// way out, as in Docker.
				return nil, true, fmt.Errorf("network unreachable: %s", host)
			}
			return nil, false, nil
		}
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil {
			return nil, true, err
		}
		req := netRequest{Op: "dial", Addr: host, Port: uint16(port), Proto: network}
		if src, ok := gvx.NATGuestSource(ctx); ok {
			req.SrcPort = src.Port()
		}
		resp, file, err := client.callInSession(req)
		if err != nil {
			return nil, true, err
		}
		if !resp.Found {
			return nil, true, fmt.Errorf("no container has address %s", host)
		}
		if file == nil {
			return nil, true, fmt.Errorf("dial %s: %s", addr, resp.Error)
		}
		defer func() { _ = file.Close() }()
		conn, err := net.FileConn(file)
		if err != nil {
			return nil, true, err
		}
		if network == "udp" {
			return &framedPacketConn{Conn: conn}, true, nil
		}
		return conn, true, nil
	}
	return lookup, dial
}

// spliceConns copies both directions until one side ends.
func spliceConns(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		closeWrite(a)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		closeWrite(b)
		done <- struct{}{}
	}()
	<-done
	<-done
}
