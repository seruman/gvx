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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"z.gfire.dev/gvx/pkg/gvx"
)

// routeContainerExtra serves pause, unpause, top, stats, rename, update, and
// export.
func (s *server) routeContainerExtra(w http.ResponseWriter, r *http.Request, id, action string) bool {
	switch {
	case action == "pause" && r.Method == http.MethodPost:
		s.pauseContainer(w, r, id, true)
	case action == "unpause" && r.Method == http.MethodPost:
		s.pauseContainer(w, r, id, false)
	case action == "top" && r.Method == http.MethodGet:
		s.topContainer(w, r, id)
	case action == "stats" && r.Method == http.MethodGet:
		s.statsContainer(w, r, id)
	case action == "rename" && r.Method == http.MethodPost:
		s.renameContainer(w, r, id)
	case action == "update" && r.Method == http.MethodPost:
		s.updateContainer(w, r, id)
	case action == "export" && r.Method == http.MethodGet && s.cache != nil:
		s.exportContainer(w, r, id)
	default:
		return false
	}
	return true
}

func (s *server) runningRecord(w http.ResponseWriter, ref string) (containerRecord, bool) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return containerRecord{}, false
	}
	if !rec.State.Running {
		writeError(w, http.StatusConflict, "Container "+ref+" is not running")
		return containerRecord{}, false
	}
	return rec, true
}

func (s *server) pauseContainer(w http.ResponseWriter, r *http.Request, ref string, pause bool) {
	rec, ok := s.runningRecord(w, ref)
	if !ok {
		return
	}
	if rec.State.Paused == pause {
		if pause {
			writeError(w, http.StatusConflict, "Container "+ref+" is already paused")
		} else {
			writeError(w, http.StatusConflict, "Container "+ref+" is not paused")
		}
		return
	}
	op := "resume"
	if pause {
		op = "pause"
	}
	conn, _, err := controlCall(r.Context(), controlSocketPath(s.stateDir, rec.ID), controlRequest{Op: op})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = conn.Close()
	_ = s.containers.updateState(rec.ID, func(st *containerState) {
		st.Paused = pause
		if pause {
			st.Status = "paused"
		} else if st.Running {
			st.Status = "running"
		}
	})
	w.WriteHeader(http.StatusNoContent)
}

func controlJSON(ctx context.Context, sock, op string, out any) error {
	conn, reader, err := controlCall(ctx, sock, controlRequest{Op: op})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return json.NewDecoder(reader).Decode(out)
}

func (s *server) topContainer(w http.ResponseWriter, r *http.Request, ref string) {
	rec, ok := s.runningRecord(w, ref)
	if !ok {
		return
	}
	var procs []gvx.Process
	if err := controlJSON(r.Context(), controlSocketPath(s.stateDir, rec.ID), "top", &procs); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	titles := []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}
	rows := [][]string{}
	for _, p := range procs {
		cpu := p.CPU
		rows = append(rows, []string{
			strconv.Itoa(int(p.UID)), strconv.Itoa(int(p.PID)), strconv.Itoa(int(p.PPID)), "0",
			rec.State.StartedAt.Local().Format("15:04"), "?",
			fmt.Sprintf("%02d:%02d:%02d", int(cpu.Hours()), int(cpu.Minutes())%60, int(cpu.Seconds())%60),
			strings.Join(p.Args, " "),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"Titles": titles, "Processes": rows})
}

// statsContainer streams Docker stats samples, or writes one with stream=0.
func (s *server) statsContainer(w http.ResponseWriter, r *http.Request, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	stream := r.URL.Query().Get("stream") != "0" && r.URL.Query().Get("stream") != "false"
	oneShot := r.URL.Query().Get("one-shot") == "1" || r.URL.Query().Get("one-shot") == "true"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	var prev gvx.Usage
	var prevAt time.Time
	sample := func() bool {
		var u gvx.Usage
		if rec.State.Running {
			if err := controlJSON(r.Context(), controlSocketPath(s.stateDir, rec.ID), "stats", &u); err != nil {
				return false
			}
		}
		now := time.Now()
		_ = enc.Encode(dockerStats(rec, u, prev, now, prevAt))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		prev, prevAt = u, now
		return true
	}
	if !stream || oneShot {
		sample()
		return
	}
	for {
		if !sample() {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
		current, err := s.containers.get(rec.ID)
		if err != nil || !current.State.Running {
			return
		}
	}
}

func dockerStats(rec containerRecord, u, prev gvx.Usage, now, prevAt time.Time) map[string]any {
	ncpu := runtime.NumCPU()
	system := uint64(now.UnixNano()) * uint64(ncpu)
	preSystem := uint64(0)
	if !prevAt.IsZero() {
		preSystem = uint64(prevAt.UnixNano()) * uint64(ncpu)
	}
	return map[string]any{
		"id": rec.ID, "name": "/" + rec.Name, "read": now.Format(time.RFC3339Nano), "preread": prevAt.Format(time.RFC3339Nano),
		"pids_stats": map[string]any{"current": u.PIDs},
		"cpu_stats": map[string]any{
			"cpu_usage":        map[string]any{"total_usage": u.CPU.Nanoseconds()},
			"system_cpu_usage": system, "online_cpus": ncpu,
		},
		"precpu_stats": map[string]any{
			"cpu_usage":        map[string]any{"total_usage": prev.CPU.Nanoseconds()},
			"system_cpu_usage": preSystem, "online_cpus": ncpu,
		},
		"memory_stats": map[string]any{"usage": u.MemoryBytes, "limit": u.MemoryLimit, "stats": map[string]any{}},
		"networks": map[string]any{"eth0": map[string]any{
			"rx_bytes": u.RxBytes, "tx_bytes": u.TxBytes, "rx_packets": u.RxPackets, "tx_packets": u.TxPackets,
		}},
		"blkio_stats": map[string]any{},
	}
}

func (s *server) renameContainer(w http.ResponseWriter, r *http.Request, ref string) {
	name := strings.TrimPrefix(r.URL.Query().Get("name"), "/")
	if !containerNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid container name "+strconv.Quote(name))
		return
	}
	if _, err := s.containers.get(name); err == nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("Conflict. The container name %q is already in use", "/"+name))
		return
	}
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if s.beforeRename != nil {
		s.beforeRename()
	}
	// Check the name again under the store lock, so two renames or a
	// rename and a create cannot give one name to two containers.
	if _, err := s.containers.renameLocked(rec.ID, name); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateContainer changes the restart policy at once. Resource limits take
// effect at the next start, because a sandbox reads them when it boots.
func (s *server) updateContainer(w http.ResponseWriter, r *http.Request, ref string) {
	var req struct {
		RestartPolicy *restartPolicy `json:"RestartPolicy"`
		Memory        int64          `json:"Memory"`
		NanoCPUs      int64          `json:"NanoCpus"`
		CPUQuota      int64          `json:"CpuQuota"`
		CPUPeriod     int64          `json:"CpuPeriod"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var warnings []string
	if _, err := s.containers.update(rec.ID, func(cur *containerRecord) error {
		if req.RestartPolicy != nil && req.RestartPolicy.Name != "" {
			cur.HostConfig.RestartPolicy = *req.RestartPolicy
		}
		if req.Memory > 0 {
			cur.HostConfig.Memory = req.Memory
		}
		if req.NanoCPUs > 0 {
			cur.HostConfig.NanoCPUs = req.NanoCPUs
		}
		if req.CPUQuota > 0 && req.CPUPeriod > 0 {
			cur.HostConfig.NanoCPUs = req.CPUQuota * 1_000_000_000 / req.CPUPeriod
		}
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rec.State.Running && (req.Memory > 0 || req.NanoCPUs > 0 || req.CPUQuota > 0) {
		warnings = append(warnings, "gvx applies the new memory and CPU limits at the next start of the container")
	}
	writeJSON(w, http.StatusOK, map[string]any{"Warnings": warnings})
}

// systemDF answers docker system df.
func (s *server) systemDF(w http.ResponseWriter) {
	images, _ := s.images.ListImages(s.arch)
	records, _ := s.containers.list()
	used := map[string]int{}
	for _, c := range records {
		used[c.ImageID]++
	}
	imageViews := []map[string]any{}
	for _, img := range images {
		imageViews = append(imageViews, map[string]any{
			"Id": img.ID, "RepoTags": img.RepoTags(), "Created": img.Created.Unix(), "Size": img.Size,
			"SharedSize": 0, "Containers": used[img.ID],
		})
	}
	containerViews := []map[string]any{}
	for _, c := range records {
		size := fileSize(s.containers.rootfsPath(c.ID))
		v := s.containerSummary(c)
		v["SizeRw"] = size
		v["SizeRootFs"] = size
		containerViews = append(containerViews, v)
	}
	volumeViews := []map[string]any{}
	vols, _ := s.allVolumes()
	for _, v := range vols {
		view := s.volumeView(v)
		users, _ := s.volumeUsers(v.Name)
		view["UsageData"] = map[string]any{"Size": dirSize(s.volumeDataDir(v.Name)), "RefCount": len(users)}
		volumeViews = append(volumeViews, view)
	}
	layers := int64(0)
	for _, img := range images {
		layers += img.Size
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"LayersSize": layers, "Images": imageViews, "Containers": containerViews,
		"Volumes": volumeViews, "BuildCache": []any{},
	})
}

func fileSize(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return info.Size()
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// pruneContainers removes every stopped container.
func (s *server) pruneContainers(w http.ResponseWriter) {
	records, err := s.containers.list()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	deleted := []string{}
	var reclaimed int64
	for _, c := range records {
		if c.State.Running || c.State.Restarting {
			continue
		}
		size := fileSize(s.containers.rootfsPath(c.ID))
		if s.containers.remove(c.ID) == nil {
			// Docker's container prune also removes anonymous volumes.
			_ = s.containers.removeAnonymousVolumes(c, s.volumeRoot())
			deleted = append(deleted, c.ID)
			reclaimed += size
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ContainersDeleted": deleted, "SpaceReclaimed": reclaimed})
}
