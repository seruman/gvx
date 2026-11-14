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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var volumeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// volumeRecord is one named volume. The data is a host directory, so several
// containers can mount the volume at once, as Docker allows.
type volumeRecord struct {
	Name      string            `json:"Name"`
	Driver    string            `json:"Driver"`
	CreatedAt time.Time         `json:"CreatedAt"`
	Labels    map[string]string `json:"Labels"`
	Options   map[string]string `json:"Options"`
}

func (s *server) volumeRoot() string { return filepath.Join(s.stateDir, "volumes") }

func (s *server) volumeDataDir(name string) string {
	return filepath.Join(s.volumeRoot(), name, "_data")
}

func (s *server) volumeMetaPath(name string) string {
	return filepath.Join(s.volumeRoot(), name, "volume.json")
}

func (s *server) routeVolume(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/volumes" && r.Method == http.MethodGet:
		s.listVolumes(w)
	case path == "/volumes/create" && r.Method == http.MethodPost:
		s.createVolume(w, r)
	case path == "/volumes/prune" && r.Method == http.MethodPost:
		s.pruneVolumes(w)
	case strings.HasPrefix(path, "/volumes/") && r.Method == http.MethodGet:
		s.inspectVolume(w, strings.TrimPrefix(path, "/volumes/"))
	case strings.HasPrefix(path, "/volumes/") && r.Method == http.MethodDelete:
		s.removeVolume(w, r, strings.TrimPrefix(path, "/volumes/"))
	default:
		return false
	}
	return true
}

func (s *server) ensureVolume(name string, labels map[string]string) (volumeRecord, error) {
	if name == "" {
		id, err := newContainerID()
		if err != nil {
			return volumeRecord{}, err
		}
		name = id
	}
	if !volumeNamePattern.MatchString(name) {
		return volumeRecord{}, fmt.Errorf("invalid volume name %q", name)
	}
	var rec volumeRecord
	if err := readJSONFile(s.volumeMetaPath(name), &rec); err == nil {
		return rec, nil
	}
	if err := os.MkdirAll(s.volumeDataDir(name), 0o700); err != nil {
		return volumeRecord{}, fmt.Errorf("create volume directory: %w", err)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	rec = volumeRecord{Name: name, Driver: "local", CreatedAt: time.Now().UTC(), Labels: labels, Options: map[string]string{}}
	return rec, writeJSONFile(s.volumeMetaPath(name), rec)
}

func (s *server) volumeView(rec volumeRecord) map[string]any {
	return map[string]any{
		"Name": rec.Name, "Driver": rec.Driver, "Mountpoint": s.volumeDataDir(rec.Name),
		"CreatedAt": rec.CreatedAt.Format(time.RFC3339), "Labels": rec.Labels,
		"Options": rec.Options, "Scope": "local",
	}
}

func (s *server) allVolumes() ([]volumeRecord, error) {
	entries, err := os.ReadDir(s.volumeRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []volumeRecord
	for _, e := range entries {
		var rec volumeRecord
		if e.IsDir() && readJSONFile(s.volumeMetaPath(e.Name()), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *server) listVolumes(w http.ResponseWriter) {
	vols, err := s.allVolumes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := []map[string]any{}
	for _, v := range vols {
		views = append(views, s.volumeView(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"Volumes": views, "Warnings": []string{}})
}

func (s *server) createVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string            `json:"Name"`
		Driver string            `json:"Driver"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Driver != "" && req.Driver != "local" {
		writeError(w, http.StatusBadRequest, "gvx supports only the local volume driver")
		return
	}
	rec, err := s.ensureVolume(req.Name, req.Labels)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.volumeView(rec))
}

func (s *server) inspectVolume(w http.ResponseWriter, name string) {
	var rec volumeRecord
	if err := readJSONFile(s.volumeMetaPath(name), &rec); err != nil {
		writeError(w, http.StatusNotFound, "get "+name+": no such volume")
		return
	}
	writeJSON(w, http.StatusOK, s.volumeView(rec))
}

func (s *server) volumeUsers(name string) ([]string, error) {
	records, err := s.containers.list()
	if err != nil {
		return nil, err
	}
	var users []string
	for _, c := range records {
		for _, b := range c.HostConfig.Binds {
			if src, _, _ := strings.Cut(b, ":"); src == s.volumeDataDir(name) {
				users = append(users, c.Name)
			}
		}
	}
	return users, nil
}

func (s *server) removeVolume(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := os.Stat(s.volumeMetaPath(name)); err != nil {
		if r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusNotFound, "get "+name+": no such volume")
		return
	}
	users, err := s.volumeUsers(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(users) > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("remove %s: volume is in use - [%s]", name, strings.Join(users, ", ")))
		return
	}
	if err := os.RemoveAll(filepath.Join(s.volumeRoot(), name)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) pruneVolumes(w http.ResponseWriter) {
	vols, err := s.allVolumes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	deleted := []string{}
	for _, v := range vols {
		users, err := s.volumeUsers(v.Name)
		if err != nil || len(users) > 0 {
			continue
		}
		if os.RemoveAll(filepath.Join(s.volumeRoot(), v.Name)) == nil {
			deleted = append(deleted, v.Name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"VolumesDeleted": deleted, "SpaceReclaimed": 0})
}

// resolveBinds turns named volumes in binds and mounts into host directories.
// A bind with an absolute source stays.
func (s *server) resolveBinds(binds []string, mounts []mountSpec) ([]string, []string, error) {
	out := make([]string, 0, len(binds)+len(mounts))
	var seeds []string
	for _, b := range binds {
		src, rest, ok := strings.Cut(b, ":")
		if !ok {
			return nil, nil, fmt.Errorf("invalid bind %q", b)
		}
		if !strings.HasPrefix(src, "/") {
			if _, err := s.ensureVolume(src, nil); err != nil {
				return nil, nil, err
			}
			src = s.volumeDataDir(src)
			target, _, _ := strings.Cut(rest, ":")
			if !strings.Contains(rest, "nocopy") {
				seeds = append(seeds, src+":"+target)
			}
			rest = strings.ReplaceAll(rest, ",nocopy", "")
			rest = strings.ReplaceAll(rest, ":nocopy", "")
		}
		out = append(out, src+":"+rest)
	}
	for _, m := range mounts {
		mode := ""
		if m.ReadOnly {
			mode = ":ro"
		}
		switch m.Type {
		case "bind":
			out = append(out, m.Source+":"+m.Target+mode)
		case "volume", "":
			name := m.Source
			if name == "" {
				id, err := newContainerID()
				if err != nil {
					return nil, nil, err
				}
				name = id
			}
			if _, err := s.ensureVolume(name, nil); err != nil {
				return nil, nil, err
			}
			out = append(out, s.volumeDataDir(name)+":"+m.Target+mode)
			if m.VolumeOptions == nil || !m.VolumeOptions.NoCopy {
				seeds = append(seeds, s.volumeDataDir(name)+":"+m.Target)
			}
		case "tmpfs":
			// handled by sandboxConfig through Tmpfs.
		default:
			return nil, nil, fmt.Errorf("unsupported mount type %q", m.Type)
		}
	}
	return out, seeds, nil
}
