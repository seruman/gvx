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
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"z.gfire.dev/gvx/pkg/oci"
)

var (
	errNotFound = errors.New("not found")
	errConflict = errors.New("conflict")
)

type containerRuntime interface {
	start(context.Context, containerRecord) error
}

type processRuntime struct {
	exe   string
	paths paths
	store *containerStore
	// afterSupervisorStart runs between the start of the supervisor and the
	// state update of start. Tests use it to order the two writers.
	afterSupervisorStart func()
}

func (r processRuntime) start(_ context.Context, rec containerRecord) error {
	// Mark the start before the supervisor exists. Only the supervisor
	// writes "running" and the exit, so a run that ends at once keeps its
	// exit. The flags make the container read as running until the
	// supervisor writes its first state, so stop and wait see it.
	if err := r.store.updateState(rec.ID, func(st *containerState) {
		st.Running = true
		st.Status = "running"
		st.Pid = 0
		st.ExitCode = 0
		st.Error = ""
		st.Stopping = false
		st.StopTimeout = nil
		st.StartedAt = time.Now().UTC()
		st.FinishedAt = time.Time{}
		st.StartPending = time.Now().UTC()
	}); err != nil {
		return err
	}
	cmd := exec.Command(r.exe, "supervise", "-state-dir", r.paths.stateDir, rec.ID)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		_ = r.store.updateState(rec.ID, func(st *containerState) {
			st.Running = false
			st.StartPending = time.Time{}
			st.Status = "exited"
			st.ExitCode = 125
			st.Error = err.Error()
			st.FinishedAt = time.Now().UTC()
		})
		return fmt.Errorf("start supervisor: %w", err)
	}
	// Reap the supervisor when it exits. Setsid does not change its parent,
	// so without Wait it stays a zombie for the life of this process.
	go func() {
		_ = cmd.Wait()
		// A supervisor that died before it took its lock leaves the start
		// pending. End it, so the container reads as exited at once.
		_ = r.store.updateState(rec.ID, func(st *containerState) { st.StartPending = time.Time{} })
	}()
	if r.afterSupervisorStart != nil {
		r.afterSupervisorStart()
	}
	return nil
}

func (s *server) routeContainer(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/containers/create" && r.Method == http.MethodPost:
		s.createContainer(w, r)
	case path == "/containers/json" && r.Method == http.MethodGet:
		s.listContainers(w, r)
	case path == "/containers/prune" && r.Method == http.MethodPost:
		s.pruneContainers(w)
	case strings.HasPrefix(path, "/containers/"):
		rest := strings.TrimPrefix(path, "/containers/")
		id, action, _ := strings.Cut(rest, "/")
		if s.routeArchive(w, r, id, action) || s.routeContainerExtra(w, r, id, action) {
			return true
		}
		switch {
		case action == "json" && r.Method == http.MethodGet:
			s.inspectContainer(w, id)
		case action == "start" && r.Method == http.MethodPost:
			s.startContainer(w, r, id)
		case action == "stop" && r.Method == http.MethodPost:
			s.stopContainer(w, r, id, true)
		case action == "kill" && r.Method == http.MethodPost:
			s.killContainer(w, r, id)
		case action == "restart" && r.Method == http.MethodPost:
			s.restartContainer(w, r, id)
		case action == "wait" && r.Method == http.MethodPost:
			s.waitContainer(w, r, id)
		case action == "logs" && r.Method == http.MethodGet:
			s.containerLogs(w, r, id)
		case action == "attach" && r.Method == http.MethodPost:
			s.attachContainer(w, r, id)
		case action == "resize" && r.Method == http.MethodPost:
			s.resizeContainer(w, r, id)
		case action == "" && r.Method == http.MethodDelete:
			s.removeContainer(w, r, id)
		default:
			return false
		}
	default:
		return false
	}
	return true
}

func (s *server) createContainer(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode container config: "+err.Error())
		return
	}
	name := strings.TrimPrefix(r.URL.Query().Get("name"), "/")
	if name != "" && !containerNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid container name "+strconv.Quote(name))
		return
	}
	if req.Image == "" {
		writeError(w, http.StatusBadRequest, "config has no image")
		return
	}
	if err := unsupportedOptions(req.HostConfig); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.HostConfig.NetworkMode == "host" {
		// Each container has its own network stack in its sandbox, so it
		// cannot share the ports and loopback of the Mac. Publish ports
		// with -p, and reach the Mac at host.docker.internal.
		writeError(w, http.StatusBadRequest, "gvx does not support --network host (Compose network_mode: host); publish ports with -p and reach the Mac at host.docker.internal")
		return
	}
	if strings.HasPrefix(req.HostConfig.NetworkMode, "container:") {
		writeError(w, http.StatusNotImplemented, "gvx does not support --network container: (Compose network_mode: service:); each container has its own network stack")
		return
	}
	sel, err := s.images.Select(req.Image, s.arch)
	if err != nil || len(sel.Images) == 0 {
		writeError(w, http.StatusNotFound, "No such image: "+req.Image)
		return
	}
	img := sel.Images[0]
	id, err := newContainerID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if name == "" {
		name = generatedName(id)
	}
	anonymous := anonymousVolumes(img.Config.Volumes, req.Volumes, req.HostConfig)
	binds, seeds, err := s.resolveBinds(append(req.HostConfig.Binds, anonymous...), req.HostConfig.Mounts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.HostConfig.Binds = binds
	req.HostConfig.Mounts = withoutVolumeMounts(req.HostConfig.Mounts)
	var consoleRows, consoleCols uint16
	if len(req.HostConfig.ConsoleSize) == 2 {
		consoleRows, consoleCols = req.HostConfig.ConsoleSize[0], req.HostConfig.ConsoleSize[1]
	}
	ports, err := allocatePorts(req.HostConfig.PortBindings)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec := containerRecord{
		ID: id, Name: name, Created: time.Now().UTC(), ImageID: img.ID,
		Config: req.containerConfig, HostConfig: req.HostConfig,
		State:       containerState{Status: "created", Ports: ports},
		SeedVolumes: seeds,
		ConsoleRows: consoleRows,
		ConsoleCols: consoleCols,
	}
	applyImageDefaults(&rec.Config, img.Config.Entrypoint, img.Config.Cmd, img.Config.Env, img.Config.WorkingDir, img.Config.User)
	if rec.Config.StopSignal == "" {
		rec.Config.StopSignal = img.Config.StopSignal
	}
	if rec.Config.StopSignal != "" {
		if _, err := parseSignal(rec.Config.StopSignal); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if rec.Config.Labels == nil {
		rec.Config.Labels = map[string]string{}
	}
	if err := s.containerNetworks(&rec, req.HostConfig.NetworkMode, req.NetworkingConfig); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.containers.create(rec); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"Id": id, "Warnings": []string{}})
}

// unsupportedOptions refuses the options that gvx cannot honor, so a
// container never runs with less isolation or fewer limits than it asked for
// without a word.
func unsupportedOptions(hc hostConfig) error {
	var bad []string
	if hc.Privileged {
		bad = append(bad, "--privileged")
	}
	if len(hc.CapAdd) > 0 {
		bad = append(bad, "--cap-add")
	}
	if len(hc.CapDrop) > 0 {
		bad = append(bad, "--cap-drop")
	}
	if len(hc.SecurityOpt) > 0 {
		bad = append(bad, "--security-opt")
	}
	if len(hc.Devices) > 0 || len(hc.DeviceRequests) > 0 {
		bad = append(bad, "--device/--gpus")
	}
	if len(hc.Sysctls) > 0 {
		bad = append(bad, "--sysctl")
	}
	if hc.PidsLimit != nil && *hc.PidsLimit > 0 {
		bad = append(bad, "--pids-limit")
	}
	if hc.Init != nil && *hc.Init {
		bad = append(bad, "--init")
	}
	if hc.LogConfig.Type != "" && hc.LogConfig.Type != "json-file" && hc.LogConfig.Type != "local" {
		bad = append(bad, "--log-driver "+hc.LogConfig.Type)
	}
	if len(bad) > 0 {
		return fmt.Errorf("gvx does not support %s", strings.Join(bad, ", "))
	}
	return nil
}

// applyImageDefaults records the image values that the container uses, as
// Docker does. An explicit entrypoint drops the image Cmd.
func applyImageDefaults(cfg *containerConfig, entrypoint, cmd, env []string, workingDir, user string) {
	if cfg.Entrypoint == nil {
		cfg.Entrypoint = entrypoint
		if cfg.Cmd == nil {
			cfg.Cmd = cmd
		}
	}
	cfg.Env = mergeEnv(env, cfg.Env)
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = workingDir
	}
	if cfg.User == "" {
		cfg.User = user
	}
}

func mergeEnv(base, overrides []string) []string {
	out := append([]string{}, base...)
	index := map[string]int{}
	for i, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		index[k] = i
	}
	for _, kv := range overrides {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := index[k]; ok {
			out[i] = kv
			continue
		}
		index[k] = len(out)
		out = append(out, kv)
	}
	return out
}

// anonymousVolumes returns a bind for each VOLUME of the image and of the
// config that no bind or mount covers. Each gets a new named volume.
func anonymousVolumes(imageVolumes, configVolumes map[string]struct{}, hc hostConfig) []string {
	covered := map[string]bool{}
	for _, b := range hc.Binds {
		parts := strings.Split(b, ":")
		if len(parts) >= 2 {
			covered[parts[1]] = true
		}
	}
	for _, m := range hc.Mounts {
		covered[m.Target] = true
	}
	for target := range hc.Tmpfs {
		covered[target] = true
	}
	var out []string
	for _, set := range []map[string]struct{}{imageVolumes, configVolumes} {
		for target := range set {
			if covered[target] {
				continue
			}
			covered[target] = true
			id, err := newContainerID()
			if err != nil {
				continue
			}
			out = append(out, id+":"+target)
		}
	}
	return out
}

// withoutVolumeMounts drops the bind and volume mounts, which resolveBinds
// turned into binds.
func withoutVolumeMounts(mounts []mountSpec) []mountSpec {
	var out []mountSpec
	for _, m := range mounts {
		if m.Type == "tmpfs" {
			out = append(out, m)
		}
	}
	return out
}

func generatedName(id string) string { return "gvx_" + id[:12] }

func allocatePorts(bindings map[string][]portBinding) (map[string][]portBinding, error) {
	out := map[string][]portBinding{}
	for containerPort, hostBindings := range bindings {
		for _, hb := range hostBindings {
			if hb.HostIP == "" {
				hb.HostIP = "0.0.0.0"
			}
			if hb.HostPort == "" || hb.HostPort == "0" {
				port, err := freePort(containerPort)
				if err != nil {
					return nil, err
				}
				hb.HostPort = strconv.Itoa(port)
			}
			out[containerPort] = append(out[containerPort], hb)
		}
	}
	return out, nil
}

func freePort(containerPort string) (int, error) {
	if strings.HasSuffix(containerPort, "/udp") {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return 0, fmt.Errorf("allocate host port: %w", err)
		}
		defer func() { _ = c.Close() }()
		return c.LocalAddr().(*net.UDPAddr).Port, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate host port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (s *server) listContainers(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	filters, err := parseFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	records, err := s.containers.list()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]any{}
	for _, rec := range records {
		if !all && !rec.State.Running {
			continue
		}
		if !matchesFilters(rec, filters) {
			continue
		}
		out = append(out, s.containerSummary(rec))
	}
	writeJSON(w, http.StatusOK, out)
}

func parseFilters(raw string) (map[string][]string, error) {
	if raw == "" {
		return nil, nil
	}
	var asList map[string][]string
	if err := json.Unmarshal([]byte(raw), &asList); err == nil {
		return asList, nil
	}
	var asMap map[string]map[string]bool
	if err := json.Unmarshal([]byte(raw), &asMap); err != nil {
		return nil, fmt.Errorf("invalid filters: %w", err)
	}
	out := map[string][]string{}
	for key, values := range asMap {
		for value, ok := range values {
			if ok {
				out[key] = append(out[key], value)
			}
		}
	}
	return out, nil
}

func matchesFilters(rec containerRecord, filters map[string][]string) bool {
	for key, values := range filters {
		if key == "label" {
			// Every label filter must match, as in Docker.
			for _, value := range values {
				k, v, hasValue := strings.Cut(value, "=")
				got, exists := rec.Config.Labels[k]
				if !exists || (hasValue && got != v) {
					return false
				}
			}
			continue
		}
		ok := false
		for _, value := range values {
			switch key {
			case "name":
				ok = strings.Contains(rec.Name, strings.TrimPrefix(value, "/"))
			case "id":
				ok = strings.HasPrefix(rec.ID, value)
			case "status":
				ok = rec.State.Status == value
			case "network":
				_, ok = rec.Networks[value]
			case "volume":
				for _, b := range rec.HostConfig.Binds {
					if strings.Contains(b, "/"+value+"/") {
						ok = true
					}
				}
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

func (s *server) containerSummary(rec containerRecord) map[string]any {
	return map[string]any{
		"Id":      rec.ID,
		"Names":   []string{"/" + rec.Name},
		"Image":   rec.Config.Image,
		"ImageID": rec.ImageID,
		"Command": strings.Join(append(append([]string{}, rec.Config.Entrypoint...), rec.Config.Cmd...), " "),
		"Created": rec.Created.Unix(),
		"Ports":   summaryPorts(rec.State.Ports),
		"Labels":  rec.Config.Labels,
		"State":   rec.State.Status,
		"Status":  statusText(rec.State),
		"HostConfig": map[string]string{
			"NetworkMode": defaultString(rec.HostConfig.NetworkMode, "default"),
		},
		"NetworkSettings": networkSettings(rec),
		"Mounts":          s.containerMounts(rec),
	}
}

func summaryPorts(ports map[string][]portBinding) []map[string]any {
	out := []map[string]any{}
	for containerPort, bindings := range ports {
		port, proto, _ := strings.Cut(containerPort, "/")
		private, _ := strconv.Atoi(port)
		for _, b := range bindings {
			public, _ := strconv.Atoi(b.HostPort)
			out = append(out, map[string]any{"IP": b.HostIP, "PrivatePort": private, "PublicPort": public, "Type": defaultString(proto, "tcp")})
		}
	}
	return out
}

func statusText(state containerState) string {
	switch state.Status {
	case "running":
		up := "Up " + humanDuration(time.Since(state.StartedAt))
		if state.Health != nil {
			up += " (" + state.Health.Status + ")"
		}
		return up
	case "exited":
		return fmt.Sprintf("Exited (%d) %s ago", state.ExitCode, humanDuration(time.Since(state.FinishedAt)))
	case "restarting":
		return fmt.Sprintf("Restarting (%d)", state.ExitCode)
	default:
		return "Created"
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
}

func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (s *server) inspectContainer(w http.ResponseWriter, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.containerInspect(rec))
}

// containerMounts returns the Docker mount list: a named volume for each bind
// below the volume root, and a bind for every other host path.
func (s *server) containerMounts(rec containerRecord) []map[string]any {
	out := []map[string]any{}
	root := s.volumeRoot() + string(os.PathSeparator)
	for _, b := range rec.HostConfig.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 {
			continue
		}
		rw := !(len(parts) > 2 && strings.Contains(parts[2], "ro"))
		m := map[string]any{"Source": parts[0], "Destination": parts[1], "RW": rw, "Mode": "", "Propagation": "rprivate"}
		if rest, ok := strings.CutPrefix(parts[0], root); ok {
			name, _, _ := strings.Cut(rest, string(os.PathSeparator))
			m["Type"], m["Name"], m["Driver"] = "volume", name, "local"
			m["Mode"] = "z"
		} else {
			m["Type"] = "bind"
		}
		out = append(out, m)
	}
	for target := range rec.HostConfig.Tmpfs {
		out = append(out, map[string]any{"Type": "tmpfs", "Destination": target, "RW": true, "Source": ""})
	}
	return out
}

func (s *server) containerInspect(rec containerRecord) map[string]any {
	state := map[string]any{
		"Status": rec.State.Status, "Running": rec.State.Running, "Paused": rec.State.Paused,
		"Restarting": rec.State.Restarting, "OOMKilled": false, "Dead": false,
		"Pid": rec.State.Pid, "ExitCode": rec.State.ExitCode, "Error": rec.State.Error,
		"StartedAt": formatTime(rec.State.StartedAt), "FinishedAt": formatTime(rec.State.FinishedAt),
	}
	if rec.State.Health != nil {
		state["Health"] = rec.State.Health
	} else if healthCommand(rec.Config.Healthcheck) != nil {
		state["Health"] = healthState{Status: "starting", Log: []healthLog{}}
	}
	return map[string]any{
		"Id": rec.ID, "Created": rec.Created.Format(time.RFC3339Nano),
		"Path": firstArg(rec), "Args": restArgs(rec), "State": state,
		"Image": rec.ImageID, "Name": "/" + rec.Name, "RestartCount": rec.RestartCount,
		"Driver": "gvx", "Platform": "linux",
		"HostConfig": rec.HostConfig, "Config": rec.Config,
		"NetworkSettings": networkSettings(rec),
		"Mounts":          s.containerMounts(rec),
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "0001-01-01T00:00:00Z"
	}
	return t.Format(time.RFC3339Nano)
}

func firstArg(rec containerRecord) string {
	args := append(append([]string{}, rec.Config.Entrypoint...), rec.Config.Cmd...)
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func restArgs(rec containerRecord) []string {
	args := append(append([]string{}, rec.Config.Entrypoint...), rec.Config.Cmd...)
	if len(args) < 2 {
		return []string{}
	}
	return args[1:]
}

func (s *server) startContainer(w http.ResponseWriter, r *http.Request, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if rec.State.Running || rec.State.Restarting {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if err := s.runtime.start(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.waitControlReady(r.Context(), rec.ID, 30*time.Second)
	w.WriteHeader(http.StatusNoContent)
}

// waitControlReady waits until the shim of a started container serves its
// control socket, or the container stops. dockerd returns from a start after
// the process runs, so a client may exec at once.
func (s *server) waitControlReady(ctx context.Context, id string, timeout time.Duration) {
	path := controlSocketPath(s.stateDir, id)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return
		}
		rec, err := s.containers.get(id)
		if err != nil || (!rec.State.Running && !rec.State.Restarting) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *server) stopContainer(w http.ResponseWriter, r *http.Request, ref string, respond bool) bool {
	rec, err := s.containers.get(ref)
	if err != nil {
		if respond {
			writeStoreError(w, err)
		}
		return false
	}
	if !rec.State.Running && !rec.State.Restarting {
		if respond {
			w.WriteHeader(http.StatusNotModified)
		}
		return true
	}
	timeout := 10 * time.Second
	if rec.Config.StopTimeout != nil && *rec.Config.StopTimeout >= 0 {
		timeout = time.Duration(*rec.Config.StopTimeout) * time.Second
	}
	if t := r.URL.Query().Get("t"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n >= 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	seconds := int(timeout / time.Second)
	if err := s.containers.updateState(rec.ID, func(st *containerState) {
		st.Stopping = true
		st.StopTimeout = &seconds
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	// The supervisor sends the stop signal of the container to its shim,
	// waits for the timeout, and then kills the guest. It signals only its
	// own child, which it has not reaped, so the PID cannot belong to another
	// process. The API process signals only the PID that the supervisor lock
	// names, and only while the lock is held.
	s.signalSupervisor(rec.ID, syscall.SIGTERM)
	if !s.waitStopped(r.Context(), rec.ID, timeout+shimKillGrace+s.stopGraceOrDefault()) {
		// The supervisor did not end the run. Kill it as the last step; the
		// shim sees the end of its supervisor pipe and kills the guest.
		s.signalSupervisor(rec.ID, syscall.SIGKILL)
		s.waitStopped(r.Context(), rec.ID, 5*time.Second)
	}
	if respond {
		w.WriteHeader(http.StatusNoContent)
	}
	return true
}

// shimKillGrace is the time that a supervisor gives a shim to close its
// sandbox after SIGUSR2, before it sends SIGKILL.
const shimKillGrace = 10 * time.Second

func (s *server) stopGraceOrDefault() time.Duration {
	if s.stopGrace > 0 {
		return s.stopGrace
	}
	return 5 * time.Second
}

// signalSupervisor sends sig to the live supervisor of container id. It
// sends nothing when no supervisor holds the lock.
func (s *server) signalSupervisor(id string, sig syscall.Signal) bool {
	pid, ok := supervisorPID(s.containers.dir(id))
	if !ok {
		return false
	}
	return signalProcess(pid, sig) == nil
}

func (s *server) waitStopped(ctx context.Context, id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		rec, err := s.containers.get(id)
		if err != nil || (!rec.State.Running && !rec.State.Restarting) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *server) killContainer(w http.ResponseWriter, r *http.Request, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !rec.State.Running {
		writeError(w, http.StatusConflict, "Container "+ref+" is not running")
		return
	}
	sig, err := parseSignal(r.URL.Query().Get("signal"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if sig == linuxSIGKILL || sig == linuxSIGTERM {
		_ = s.containers.updateState(rec.ID, func(st *containerState) { st.Stopping = true })
	}
	if sig == linuxSIGKILL {
		// SIGUSR1 asks the supervisor to kill the guest at once. The shim
		// closes the sandbox cleanly, so volumes keep their state.
		if !s.signalSupervisor(rec.ID, syscall.SIGUSR1) {
			err = fmt.Errorf("container %s has no live supervisor", rec.ID[:12])
		}
	} else {
		err = s.signalGuest(r.Context(), rec.ID, sig)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linuxSignals maps the names of Linux signals to their Linux numbers. The
// guest uses these numbers, which differ from the numbers of the Mac host.
var linuxSignals = map[string]int{
	"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "IOT": 6, "BUS": 7,
	"FPE": 8, "KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12, "PIPE": 13, "ALRM": 14,
	"TERM": 15, "STKFLT": 16, "CHLD": 17, "CONT": 18, "STOP": 19, "TSTP": 20, "TTIN": 21,
	"TTOU": 22, "URG": 23, "XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27, "WINCH": 28,
	"IO": 29, "POLL": 29, "PWR": 30, "SYS": 31,
}

const (
	linuxSIGKILL = 9
	linuxSIGTERM = 15
)

// parseSignal returns the Linux number of a signal name or number, and
// SIGKILL for an empty value, as docker kill does.
func parseSignal(raw string) (int, error) {
	if raw == "" {
		return linuxSIGKILL, nil
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n <= 0 || n > 64 {
			return 0, fmt.Errorf("invalid signal: %s", raw)
		}
		return n, nil
	}
	name := strings.TrimPrefix(strings.ToUpper(raw), "SIG")
	if sig, ok := linuxSignals[name]; ok {
		return sig, nil
	}
	if strings.HasPrefix(name, "RTMIN") || strings.HasPrefix(name, "RTMAX") {
		base, off := 34, strings.TrimPrefix(name, "RTMIN")
		sign := 1
		if strings.HasPrefix(name, "RTMAX") {
			base, off, sign = 64, strings.TrimPrefix(name, "RTMAX"), -1
		}
		n := 0
		if off != "" {
			v, err := strconv.Atoi(strings.TrimPrefix(strings.TrimPrefix(off, "+"), "-"))
			if err != nil {
				return 0, fmt.Errorf("invalid signal: %s", raw)
			}
			n = v
		}
		return base + sign*n, nil
	}
	return 0, fmt.Errorf("invalid signal: %s", raw)
}

// signalGuest sends a Linux signal to the init process of a running
// container through its shim.
func (s *server) signalGuest(ctx context.Context, id string, signo int) error {
	conn, _, err := controlCall(ctx, controlSocketPath(s.stateDir, id), controlRequest{Op: "signal", Signal: signo})
	if err != nil {
		return err
	}
	return conn.Close()
}

func (s *server) restartContainer(w http.ResponseWriter, r *http.Request, ref string) {
	if !s.stopContainer(w, r, ref, false) {
		writeStoreError(w, fmt.Errorf("%w: No such container: %s", errNotFound, ref))
		return
	}
	s.startContainer(w, r, ref)
}

func (s *server) waitContainer(w http.ResponseWriter, r *http.Request, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	condition := r.URL.Query().Get("condition")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	for {
		current, err := s.containers.get(rec.ID)
		if err != nil {
			code, _ := s.containers.removedExitCode(rec.ID)
			_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": code})
			return
		}
		if condition == "removed" {
			// keep waiting until the record goes away.
		} else if !current.State.Running && !current.State.Restarting && (current.State.Status == "exited" || condition == "not-running") {
			_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": current.State.ExitCode})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *server) removeContainer(w http.ResponseWriter, r *http.Request, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if rec.State.Running || rec.State.Restarting {
		if !force {
			writeError(w, http.StatusConflict, "cannot remove a running container; stop it first or use force")
			return
		}
		q := r.URL.Query()
		q.Set("t", "0")
		r.URL.RawQuery = q.Encode()
		s.stopContainer(w, r, rec.ID, false)
	}
	if err := s.containers.remove(rec.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type logEntry struct {
	Log    string    `json:"log"`
	Stream string    `json:"stream"`
	Time   time.Time `json:"time"`
}

func (s *server) containerLogs(w http.ResponseWriter, r *http.Request, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	q := r.URL.Query()
	stdout := q.Get("stdout") == "1" || q.Get("stdout") == "true"
	stderr := q.Get("stderr") == "1" || q.Get("stderr") == "true"
	if !stdout && !stderr {
		stdout, stderr = true, true
	}
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"
	timestamps := q.Get("timestamps") == "1" || q.Get("timestamps") == "true"
	tail := -1
	if t := q.Get("tail"); t != "" && t != "all" {
		tail, _ = strconv.Atoi(t)
	}
	since, err := parseLogTime(q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	until, err := parseLogTime(q.Get("until"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	w.WriteHeader(http.StatusOK)
	s.streamLogs(r.Context(), w, rec, logOptions{stdout: stdout, stderr: stderr, follow: follow, timestamps: timestamps, tail: tail, raw: rec.Config.Tty, since: since, until: until})
}

type logOptions struct {
	stdout, stderr, follow, timestamps, raw bool
	tail                                    int
	// startAtEnd skips the existing log, as an attach to a running container.
	startAtEnd bool
	// since and until bound the entries by time. Zero means no bound.
	since, until time.Time
}

// parseLogTime parses the since and until values of the Docker CLI: Unix
// seconds with an optional fraction.
func parseLogTime(v string) (time.Time, error) {
	if v == "" || v == "0" {
		return time.Time{}, nil
	}
	secText, fracText, _ := strings.Cut(v, ".")
	sec, err := strconv.ParseInt(secText, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid log time %q", v)
	}
	var nsec int64
	if fracText != "" {
		fracText = (fracText + "000000000")[:9]
		nsec, err = strconv.ParseInt(fracText, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid log time %q", v)
		}
	}
	return time.Unix(sec, nsec), nil
}

func (s *server) streamLogs(ctx context.Context, w io.Writer, rec containerRecord, opts logOptions) {
	if !opts.startAtEnd {
		// Rotated files hold the older entries, from the oldest.
		s.writeRotatedLogs(w, rec, opts)
	}
	f, err := os.Open(s.containers.logPath(rec.ID))
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	if opts.startAtEnd {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return
		}
	}
	reader := bufio.NewReader(f)
	var pending []logEntry
	if opts.tail >= 0 && !opts.startAtEnd {
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				var e logEntry
				if json.Unmarshal(line, &e) == nil {
					pending = append(pending, e)
					if len(pending) > opts.tail {
						pending = pending[1:]
					}
				}
			}
			if err != nil {
				break
			}
		}
		for _, e := range pending {
			writeLogEntry(w, e, opts)
		}
	}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var e logEntry
			if json.Unmarshal(line, &e) == nil {
				writeLogEntry(w, e, opts)
			}
			continue
		}
		if !opts.follow {
			return
		}
		if len(line) > 0 {
			_, _ = f.Seek(-int64(len(line)), io.SeekCurrent)
			reader.Reset(f)
		}
		if errors.Is(err, io.EOF) {
			// A rotation renamed the file. Drain the old file, then follow
			// the new one from its start.
			if cur, serr := os.Stat(s.containers.logPath(rec.ID)); serr == nil {
				if open, ferr := f.Stat(); ferr == nil && !os.SameFile(cur, open) && len(line) == 0 {
					nf, oerr := os.Open(s.containers.logPath(rec.ID))
					if oerr == nil {
						_ = f.Close()
						f = nf
						reader.Reset(f)
						continue
					}
				}
			}
			current, getErr := s.containers.get(rec.ID)
			if getErr != nil || current.State.Status == "exited" {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		return
	}
}

// writeRotatedLogs writes the entries of the rotated log files. With a tail,
// the current file serves the tail, so rotated files add nothing.
func (s *server) writeRotatedLogs(w io.Writer, rec containerRecord, opts logOptions) {
	if opts.tail >= 0 {
		return
	}
	base := s.containers.logPath(rec.ID)
	var files []string
	for i := 1; ; i++ {
		p := fmt.Sprintf("%s.%d", base, i)
		if _, err := os.Stat(p); err != nil {
			break
		}
		files = append([]string{p}, files...)
	}
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 16<<20)
		for scanner.Scan() {
			var e logEntry
			if json.Unmarshal(scanner.Bytes(), &e) == nil {
				writeLogEntry(w, e, opts)
			}
		}
		_ = f.Close()
	}
}

func writeLogEntry(w io.Writer, e logEntry, opts logOptions) {
	if e.Stream == "stdout" && !opts.stdout || e.Stream == "stderr" && !opts.stderr {
		return
	}
	if !opts.since.IsZero() && e.Time.Before(opts.since) || !opts.until.IsZero() && e.Time.After(opts.until) {
		return
	}
	payload := e.Log
	if opts.timestamps {
		payload = e.Time.Format(time.RFC3339Nano) + " " + payload
	}
	if opts.raw {
		_, _ = io.WriteString(w, payload)
	} else {
		stream := byte(1)
		if e.Stream == "stderr" {
			stream = 2
		}
		writeFrame(w, stream, []byte(payload))
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeFrame(w io.Writer, stream byte, payload []byte) {
	var header [8]byte
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	_, _ = w.Write(header[:])
	_, _ = w.Write(payload)
}

// attachContainer streams the container output and, with stdin=1, sends the
// client input to the main process. The output starts at the current end of
// the log for a running container, and at the start for one that did not run
// yet, so docker run sees all output.
func (s *server) attachContainer(w http.ResponseWriter, r *http.Request, ref string) {
	rec, err := s.containers.get(ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "the connection cannot be hijacked")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	contentType := "application/vnd.docker.multiplexed-stream"
	if rec.Config.Tty {
		contentType = "application/vnd.docker.raw-stream"
	}
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 101 UPGRADED\r\nContent-Type: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", contentType)
	_ = buf.Flush()
	q := r.URL.Query()
	wantStdin := (q.Get("stdin") == "1" || q.Get("stdin") == "true") && rec.Config.OpenStdin
	detached := make(chan struct{})
	if wantStdin {
		var input io.Reader = buf.Reader
		if rec.Config.Tty {
			// Docker watches for the detach keys on a terminal only.
			keys, err := parseDetachKeys(q.Get("detachKeys"))
			if err != nil {
				keys = defaultDetachKeys
			}
			input = newDetachReader(buf.Reader, keys)
		}
		go func() {
			if s.forwardAttachInput(rec, input) {
				close(detached)
			}
		}()
	} else {
		go func() {
			// A client without stdin closes its write side at once. Only the
			// end of the container ends the output stream.
			_, _ = io.Copy(io.Discard, buf.Reader)
		}()
	}
	if q.Get("stream") != "1" && q.Get("stream") != "true" {
		return
	}
	stdout := q.Get("stdout") == "1" || q.Get("stdout") == "true"
	stderr := q.Get("stderr") == "1" || q.Get("stderr") == "true"
	tail := -1
	if rec.State.Running || rec.State.Status == "exited" {
		tail = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-detached:
			// The client detached: end its stream, and leave the container.
			cancel()
			_ = conn.Close()
		case <-ctx.Done():
		}
	}()
	s.streamLogs(ctx, conn, rec, logOptions{stdout: stdout, stderr: stderr, follow: true, tail: tail, raw: rec.Config.Tty, startAtEnd: tail == 0})
}

// forwardAttachInput sends the client input to the stdin of the main process.
// It waits for the shim, because docker run attaches before it starts the
// container.
// It returns true when the client typed the detach keys.
func (s *server) forwardAttachInput(rec containerRecord, input io.Reader) bool {
	var ctrl net.Conn
	deadline := time.Now().Add(60 * time.Second)
	for {
		current, err := s.containers.get(rec.ID)
		if err != nil || current.State.Status == "exited" {
			_, err := io.Copy(io.Discard, input)
			return errors.Is(err, errDetached)
		}
		if current.State.Running {
			// As in dockerd, the end of the input closes the stdin of a
			// container without a terminal only. The Docker CLI handles the
			// detach keys itself and then closes the connection, so the
			// end of the input of a terminal can be a detach.
			closeAtEnd := 0
			if rec.Config.StdinOnce && !rec.Config.Tty {
				closeAtEnd = 1
			}
			c, _, err := controlCall(context.Background(), controlSocketPath(s.stateDir, rec.ID), controlRequest{Op: "stdin", Signal: closeAtEnd})
			if err == nil {
				ctrl = c
				break
			}
		}
		if time.Now().After(deadline) {
			_, err := io.Copy(io.Discard, input)
			return errors.Is(err, errDetached)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer func() { _ = ctrl.Close() }()
	_, err := io.Copy(ctrl, input)
	if errors.Is(err, errDetached) {
		// Detach leaves the stdin of the container open, as in Docker.
		return true
	}
	closeWrite(ctrl)
	// Wait until the shim has read the input, so the stdin of the guest sees
	// every byte before its end.
	_, _ = io.Copy(io.Discard, ctrl)
	return false
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, strings.TrimPrefix(err.Error(), errNotFound.Error()+": "))
	case errors.Is(err, errConflict):
		writeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), errConflict.Error()+": "))
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

var _ = oci.Image{}
