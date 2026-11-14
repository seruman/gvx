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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

type containerConfig struct {
	Image        string              `json:"Image"`
	Hostname     string              `json:"Hostname"`
	Domainname   string              `json:"Domainname"`
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	Env          []string            `json:"Env"`
	WorkingDir   string              `json:"WorkingDir"`
	User         string              `json:"User"`
	Labels       map[string]string   `json:"Labels"`
	Tty          bool                `json:"Tty"`
	OpenStdin    bool                `json:"OpenStdin"`
	StdinOnce    bool                `json:"StdinOnce"`
	AttachStdin  bool                `json:"AttachStdin"`
	ExposedPorts map[string]any      `json:"ExposedPorts"`
	Volumes      map[string]struct{} `json:"Volumes"`
	Healthcheck  *healthConfig       `json:"Healthcheck,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	StopTimeout  *int                `json:"StopTimeout,omitempty"`
}

// healthConfig is the Docker health check. The durations are nanoseconds.
type healthConfig struct {
	Test          []string `json:"Test"`
	Interval      int64    `json:"Interval"`
	Timeout       int64    `json:"Timeout"`
	StartPeriod   int64    `json:"StartPeriod"`
	StartInterval int64    `json:"StartInterval"`
	Retries       int      `json:"Retries"`
}

type healthLog struct {
	Start    time.Time `json:"Start"`
	End      time.Time `json:"End"`
	ExitCode int       `json:"ExitCode"`
	Output   string    `json:"Output"`
}

type healthState struct {
	Status        string      `json:"Status"`
	FailingStreak int         `json:"FailingStreak"`
	Log           []healthLog `json:"Log"`
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type restartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type mountSpec struct {
	Type          string `json:"Type"`
	Source        string `json:"Source"`
	Target        string `json:"Target"`
	ReadOnly      bool   `json:"ReadOnly"`
	VolumeOptions *struct {
		NoCopy bool `json:"NoCopy"`
	} `json:"VolumeOptions,omitempty"`
}

type hostConfig struct {
	Binds          []string                 `json:"Binds"`
	Mounts         []mountSpec              `json:"Mounts"`
	Tmpfs          map[string]string        `json:"Tmpfs"`
	ExtraHosts     []string                 `json:"ExtraHosts"`
	DNS            []string                 `json:"Dns"`
	DNSSearch      []string                 `json:"DnsSearch"`
	DNSOptions     []string                 `json:"DnsOptions"`
	LogConfig      logConfig                `json:"LogConfig"`
	ReadonlyRootfs bool                     `json:"ReadonlyRootfs"`
	ShmSize        int64                    `json:"ShmSize"`
	GroupAdd       []string                 `json:"GroupAdd"`
	Ulimits        []dockerUlimit           `json:"Ulimits"`
	Init           *bool                    `json:"Init"`
	Privileged     bool                     `json:"Privileged"`
	CapAdd         []string                 `json:"CapAdd"`
	CapDrop        []string                 `json:"CapDrop"`
	SecurityOpt    []string                 `json:"SecurityOpt"`
	Devices        []any                    `json:"Devices"`
	DeviceRequests []any                    `json:"DeviceRequests"`
	Sysctls        map[string]string        `json:"Sysctls"`
	PidsLimit      *int64                   `json:"PidsLimit"`
	ConsoleSize    []uint16                 `json:"ConsoleSize"`
	PortBindings   map[string][]portBinding `json:"PortBindings"`
	RestartPolicy  restartPolicy            `json:"RestartPolicy"`
	AutoRemove     bool                     `json:"AutoRemove"`
	NetworkMode    string                   `json:"NetworkMode"`
	Memory         int64                    `json:"Memory"`
	NanoCPUs       int64                    `json:"NanoCpus"`
}

type dockerUlimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

type logConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

type createRequest struct {
	containerConfig
	HostConfig       hostConfig       `json:"HostConfig"`
	NetworkingConfig networkingConfig `json:"NetworkingConfig"`
}

type containerState struct {
	Status     string    `json:"Status"`
	Running    bool      `json:"Running"`
	Pid        int       `json:"Pid"`
	ExitCode   int       `json:"ExitCode"`
	Error      string    `json:"Error"`
	StartedAt  time.Time `json:"StartedAt"`
	FinishedAt time.Time `json:"FinishedAt"`
	Restarting bool      `json:"Restarting"`
	Paused     bool      `json:"Paused,omitempty"`
	Stopping   bool      `json:"Stopping,omitempty"`
	// StopTimeout is the time in seconds that the supervisor waits after the
	// stop signal before it kills the guest. The API process sets it with
	// Stopping.
	StopTimeout   *int                     `json:"StopTimeout,omitempty"`
	SupervisorPid int                      `json:"SupervisorPid,omitempty"`
	Ports         map[string][]portBinding `json:"Ports,omitempty"`
	Health        *healthState             `json:"Health,omitempty"`
}

type containerRecord struct {
	ID         string          `json:"Id"`
	Name       string          `json:"Name"`
	Created    time.Time       `json:"Created"`
	ImageID    string          `json:"ImageID"`
	Config     containerConfig `json:"Config"`
	HostConfig hostConfig      `json:"HostConfig"`
	State      containerState  `json:"State"`
	// RestartCount counts the restarts by the restart policy.
	RestartCount int `json:"RestartCount"`
	// ConsoleRows and ConsoleCols are the terminal size that the client gave
	// at create time.
	ConsoleRows uint16 `json:"ConsoleRows,omitempty"`
	ConsoleCols uint16 `json:"ConsoleCols,omitempty"`
	// SeedVolumes lists the named volumes that start with the files of the
	// image at their guest path, as "host directory:guest path". The first
	// start copies the files when the volume is empty.
	SeedVolumes []string `json:"SeedVolumes,omitempty"`
	// Networks maps a network name to the endpoint of the container on it.
	Networks map[string]endpointSettings `json:"Networks,omitempty"`
}

type containerStore struct {
	root string
	mu   sync.Mutex
}

func newContainerStore(stateDir string) *containerStore {
	return &containerStore{root: filepath.Join(stateDir, "containers")}
}

func newContainerID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate container ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *containerStore) dir(id string) string { return filepath.Join(s.root, id) }

func (s *containerStore) configPath(id string) string {
	return filepath.Join(s.dir(id), "config.json")
}

func (s *containerStore) statePath(id string) string {
	return filepath.Join(s.dir(id), "state.json")
}

func (s *containerStore) logPath(id string) string { return filepath.Join(s.dir(id), "log.json") }

func (s *containerStore) rootfsPath(id string) string {
	return filepath.Join(s.dir(id), "rootfs.ext4")
}

func (s *containerStore) create(rec containerRecord) error {
	unlock, err := s.lockAll()
	if err != nil {
		return err
	}
	defer unlock()
	if rec.Name != "" {
		if _, err := s.findLocked(rec.Name); err == nil {
			return fmt.Errorf("%w: container name %q is already in use", errConflict, rec.Name)
		}
	}
	if err := os.MkdirAll(s.dir(rec.ID), 0o700); err != nil {
		return fmt.Errorf("create container directory: %w", err)
	}
	if err := writeJSONFile(s.configPath(rec.ID), rec); err != nil {
		return err
	}
	logFile, err := os.OpenFile(s.logPath(rec.ID), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create container log: %w", err)
	}
	if err := logFile.Close(); err != nil {
		return fmt.Errorf("close container log: %w", err)
	}
	return writeJSONFile(s.statePath(rec.ID), rec.State)
}

func (s *containerStore) exitPath(id string) string {
	return filepath.Join(filepath.Dir(s.root), "exits", id+".json")
}

// removeWithExit removes a container and keeps its exit code for a client that
// waits for the removal.
func (s *containerStore) removeWithExit(id string, code int) error {
	if err := os.MkdirAll(filepath.Dir(s.exitPath(id)), 0o700); err != nil {
		return fmt.Errorf("create exit directory: %w", err)
	}
	if err := writeJSONFile(s.exitPath(id), map[string]int{"StatusCode": code}); err != nil {
		return err
	}
	return s.remove(id)
}

func (s *containerStore) removedExitCode(id string) (int, bool) {
	var v struct{ StatusCode int }
	if err := readJSONFile(s.exitPath(id), &v); err != nil {
		return 0, false
	}
	return v.StatusCode, true
}

// lockAll takes the store lock of this process and the file lock that the API
// process and every supervisor share. It returns the unlock function.
func (s *containerStore) lockAll() (func(), error) {
	s.mu.Lock()
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("create container directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(s.root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("open container lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		s.mu.Unlock()
		return nil, fmt.Errorf("lock containers: %w", err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
		s.mu.Unlock() // +checklocksforce: lockAll took s.mu, and this closure is its only release.
	}, nil
}

// claimSupervisor takes the supervisor lock of container id under the store
// lock, so that two supervisors of one container cannot start together.
func (s *containerStore) claimSupervisor(id string) (supervisorLock, error) {
	unlock, err := s.lockAll()
	if err != nil {
		return supervisorLock{}, err
	}
	defer unlock()
	return acquireSupervisorLock(s.dir(id))
}

// update reads the record of id, applies fn, and writes the record and the
// state back, under the lock of every process.
func (s *containerStore) update(id string, fn func(*containerRecord) error) (containerRecord, error) {
	unlock, err := s.lockAll()
	if err != nil {
		return containerRecord{}, err
	}
	defer unlock()
	rec, err := s.readLocked(id)
	if err != nil {
		return containerRecord{}, fmt.Errorf("%w: No such container: %s", errNotFound, id)
	}
	if err := fn(&rec); err != nil {
		return rec, err
	}
	if err := writeJSONFile(s.configPath(rec.ID), rec); err != nil {
		return rec, err
	}
	return rec, writeJSONFile(s.statePath(rec.ID), rec.State)
}

// updateState applies fn to the state of id under the lock of every process.
func (s *containerStore) updateState(id string, fn func(*containerState)) error {
	_, err := s.update(id, func(rec *containerRecord) error {
		fn(&rec.State)
		return nil
	})
	return err
}

func (s *containerStore) get(ref string) (containerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findLocked(ref)
}

func (s *containerStore) findLocked(ref string) (containerRecord, error) {
	ref = strings.TrimPrefix(ref, "/")
	records, err := s.listLocked()
	if err != nil {
		return containerRecord{}, err
	}
	var matches []containerRecord
	for _, rec := range records {
		if rec.ID == ref || rec.Name == ref {
			return rec, nil
		}
		if len(ref) >= 3 && strings.HasPrefix(rec.ID, ref) {
			matches = append(matches, rec)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return containerRecord{}, fmt.Errorf("%w: container ID prefix %q is ambiguous", errConflict, ref)
	}
	return containerRecord{}, fmt.Errorf("%w: No such container: %s", errNotFound, ref)
}

func (s *containerStore) list() ([]containerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *containerStore) listLocked() ([]containerRecord, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var records []containerRecord
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := s.readLocked(e.Name())
		if err == nil {
			records = append(records, rec)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Created.After(records[j].Created) })
	return records, nil
}

func (s *containerStore) readLocked(id string) (containerRecord, error) {
	var rec containerRecord
	if err := readJSONFile(s.configPath(id), &rec); err != nil {
		return containerRecord{}, err
	}
	if err := readJSONFile(s.statePath(id), &rec.State); err != nil {
		return containerRecord{}, err
	}
	refreshProcessStateIn(s.dir(id), &rec.State)
	return rec, nil
}

func (s *containerStore) remove(id string) error {
	unlock, err := s.lockAll()
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.RemoveAll(s.dir(id)); err != nil {
		return fmt.Errorf("remove container %s: %w", id, err)
	}
	return nil
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restrict %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func readJSONFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
