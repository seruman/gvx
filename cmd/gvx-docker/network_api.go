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
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
)

type networkingConfig struct {
	EndpointsConfig map[string]struct {
		Aliases []string `json:"Aliases"`
	} `json:"EndpointsConfig"`
}

func (s *server) routeNetwork(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/networks" && r.Method == http.MethodGet:
		s.listNetworks(w, r)
	case path == "/networks/create" && r.Method == http.MethodPost:
		s.createNetwork(w, r)
	case path == "/networks/prune" && r.Method == http.MethodPost:
		s.pruneNetworks(w)
	case strings.HasPrefix(path, "/networks/"):
		id, action, _ := strings.Cut(strings.TrimPrefix(path, "/networks/"), "/")
		switch {
		case action == "" && r.Method == http.MethodGet:
			s.inspectNetwork(w, id)
		case action == "" && r.Method == http.MethodDelete:
			s.removeNetwork(w, id)
		case action == "connect" && r.Method == http.MethodPost:
			s.connectNetwork(w, r, id)
		case action == "disconnect" && r.Method == http.MethodPost:
			s.disconnectNetwork(w, r, id)
		default:
			return false
		}
	default:
		return false
	}
	return true
}

func (s *server) networkView(n networkRecord) map[string]any {
	containers := map[string]any{}
	records, _ := s.containers.list()
	for _, c := range records {
		if ep, ok := c.Networks[n.Name]; ok {
			containers[c.ID] = map[string]string{"Name": c.Name, "IPv4Address": ep.IPAddress + "/" + prefixBits(n.Subnet)}
		}
	}
	return map[string]any{
		"Name": n.Name, "Id": n.ID, "Created": n.Created, "Scope": "local", "Driver": n.Driver,
		"EnableIPv6": false, "Internal": n.Internal, "Attachable": false, "Ingress": false,
		"IPAM":       map[string]any{"Driver": "default", "Config": []map[string]string{{"Subnet": n.Subnet, "Gateway": n.Gateway}}},
		"Containers": containers, "Options": map[string]string{}, "Labels": n.Labels,
	}
}

func prefixBits(subnet string) string {
	_, bits, _ := strings.Cut(subnet, "/")
	return bits
}

func (s *server) listNetworks(w http.ResponseWriter, r *http.Request) {
	filters, err := parseFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	recs, err := s.networks.list()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]any{}
	for _, n := range recs {
		if !networkMatches(n, filters) {
			continue
		}
		out = append(out, s.networkView(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func networkMatches(n networkRecord, filters map[string][]string) bool {
	for key, values := range filters {
		ok := false
		for _, v := range values {
			switch key {
			case "name":
				ok = n.Name == v || strings.Contains(n.Name, v)
			case "id":
				ok = strings.HasPrefix(n.ID, v)
			case "label":
				k, val, hasVal := strings.Cut(v, "=")
				got, exists := n.Labels[k]
				ok = exists && (!hasVal || got == val)
			default:
				ok = true
			}
			if ok {
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (s *server) createNetwork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string            `json:"Name"`
		Driver   string            `json:"Driver"`
		Internal bool              `json:"Internal"`
		Labels   map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Driver != "" && req.Driver != "bridge" {
		writeError(w, http.StatusBadRequest, "gvx supports only the bridge network driver")
		return
	}
	n, err := s.networks.create(req.Name, req.Labels, req.Internal)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"Id": n.ID, "Warning": ""})
}

func (s *server) inspectNetwork(w http.ResponseWriter, ref string) {
	n, err := s.networks.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.networkView(n))
}

func (s *server) networkInUse(name string) bool {
	records, _ := s.containers.list()
	for _, c := range records {
		if _, ok := c.Networks[name]; ok {
			return true
		}
	}
	return false
}

func (s *server) removeNetwork(w http.ResponseWriter, ref string) {
	n, err := s.networks.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	if n.Name == defaultNetworkName {
		writeError(w, http.StatusForbidden, "bridge is a pre-defined network and cannot be removed")
		return
	}
	if s.networkInUse(n.Name) {
		writeError(w, http.StatusConflict, "error while removing network: network "+n.Name+" has active endpoints")
		return
	}
	if err := s.networks.remove(n.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) pruneNetworks(w http.ResponseWriter) {
	recs, err := s.networks.list()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	deleted := []string{}
	for _, n := range recs {
		if n.Name != defaultNetworkName && !s.networkInUse(n.Name) && s.networks.remove(n.ID) == nil {
			deleted = append(deleted, n.Name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"NetworksDeleted": deleted})
}

// attachNetwork adds an endpoint of network n to rec.
func (s *server) attachNetwork(rec *containerRecord, n networkRecord, aliases []string) error {
	if rec.Networks == nil {
		rec.Networks = map[string]endpointSettings{}
	}
	if _, ok := rec.Networks[n.Name]; ok {
		return nil
	}
	records, err := s.containers.list()
	if err != nil {
		return err
	}
	ip, err := allocateIP(n, records)
	if err != nil {
		return err
	}
	rec.Networks[n.Name] = endpointSettings{NetworkID: n.ID, IPAddress: ip, Gateway: n.Gateway, Subnet: n.Subnet, Aliases: aliases, Internal: n.Internal}
	return nil
}

func (s *server) connectNetwork(w http.ResponseWriter, r *http.Request, ref string) {
	n, err := s.networks.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Container      string `json:"Container"`
		EndpointConfig struct {
			Aliases []string `json:"Aliases"`
		} `json:"EndpointConfig"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := s.containers.get(req.Container)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.attachNetwork(&rec, n, req.EndpointConfig.Aliases); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	endpoint := rec.Networks[n.Name]
	if _, err := s.containers.update(rec.ID, func(cur *containerRecord) error {
		if cur.Networks == nil {
			cur.Networks = map[string]endpointSettings{}
		}
		cur.Networks[n.Name] = endpoint
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) disconnectNetwork(w http.ResponseWriter, r *http.Request, ref string) {
	n, err := s.networks.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Container string `json:"Container"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := s.containers.get(req.Container)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if _, err := s.containers.update(rec.ID, func(cur *containerRecord) error {
		delete(cur.Networks, n.Name)
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// containerNetworks attaches the networks that a create request names.
func (s *server) containerNetworks(rec *containerRecord, mode string, cfg networkingConfig) error {
	if mode == "none" || mode == "host" {
		return nil
	}
	names := map[string][]string{}
	for name, ep := range cfg.EndpointsConfig {
		if name == "default" {
			name = defaultNetworkName
		}
		names[name] = ep.Aliases
	}
	if mode != "" && mode != "default" && mode != defaultNetworkName {
		if _, ok := names[mode]; !ok {
			names[mode] = nil
		}
	}
	if len(names) == 0 {
		names[defaultNetworkName] = nil
	}
	for name, aliases := range names {
		n, err := s.networks.get(name)
		if err != nil {
			return err
		}
		if n.Name == defaultNetworkName {
			aliases = nil
		}
		if err := s.attachNetwork(rec, n, aliases); err != nil {
			return err
		}
	}
	return nil
}

func networkSettings(rec containerRecord) map[string]any {
	networks := map[string]any{}
	ip, gateway := "", ""
	for name, ep := range rec.Networks {
		bits := 16
		if p, err := netip.ParsePrefix(ep.Subnet); err == nil {
			bits = p.Bits()
		}
		networks[name] = map[string]any{
			"NetworkID": ep.NetworkID, "EndpointID": rec.ID, "IPAddress": ep.IPAddress,
			"IPPrefixLen": bits, "Gateway": ep.Gateway, "Aliases": ep.Aliases,
		}
		if ip == "" || name == defaultNetworkName {
			ip, gateway = ep.IPAddress, ep.Gateway
		}
	}
	return map[string]any{"Ports": rec.State.Ports, "Networks": networks, "IPAddress": ip, "Gateway": gateway}
}
