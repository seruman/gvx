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
	"errors"
	"sort"
	"strings"
	"time"

	"z.gfire.dev/gvx/pkg/sentry/kernel"
	"z.gfire.dev/gvx/pkg/sentry/mm"
	"z.gfire.dev/gvx/pkg/usermem"
)

// Pause stops every task of the guest until Resume. A paused guest runs no
// code, as the freezer of a Docker container. A Pause of a paused guest does
// nothing, so one Resume ends any number of Pause calls.
func (s *Sandbox) Pause() error {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	s.mu.Lock()
	k := s.activeKernel
	s.mu.Unlock()
	if k == nil {
		return ErrNotRunning
	}
	if s.userPaused {
		return nil
	}
	k.Pause()
	s.userPaused = true
	return nil
}

// ErrNotPaused reports a Resume of a guest that Pause did not stop.
var ErrNotPaused = errors.New("gvx: the guest is not paused")

// Resume ends the effect of a Pause. It returns ErrNotPaused when Pause did
// not stop the guest: an unmatched unpause makes the kernel panic, which
// would end the host process.
func (s *Sandbox) Resume() error {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	s.mu.Lock()
	k := s.activeKernel
	s.mu.Unlock()
	if k == nil {
		return ErrNotRunning
	}
	if !s.userPaused {
		return ErrNotPaused
	}
	k.Unpause()
	s.userPaused = false
	return nil
}

// endPause ends a Pause of the kernel k, if one is in effect.
func (s *Sandbox) endPause(k *kernel.Kernel) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	if s.userPaused {
		k.Unpause()
		s.userPaused = false
	}
}

// Process describes one process of the guest.
type Process struct {
	PID     int32
	PPID    int32
	UID     uint32
	Command string
	Args    []string
	RSS     uint64
	CPU     time.Duration
}

// Processes lists the processes of the guest, ordered by PID.
func (s *Sandbox) Processes() ([]Process, error) {
	s.mu.Lock()
	k := s.activeKernel
	s.mu.Unlock()
	if k == nil {
		return nil, ErrNotRunning
	}
	ns := k.RootPIDNamespace()
	var out []Process
	for _, tg := range ns.ThreadGroups() {
		// A thread group that exited and was reaped has no ID in the
		// namespace, as control.Processes skips it.
		pid := ns.IDOfThreadGroup(tg)
		leader := tg.Leader()
		if pid == 0 || leader == nil {
			continue
		}
		p := Process{PID: int32(pid), Command: leader.Name()}
		if parent := leader.Parent(); parent != nil {
			p.PPID = int32(ns.IDOfThreadGroup(parent.ThreadGroup()))
		}
		p.UID = uint32(leader.Credentials().RealKUID)
		cpu := tg.CPUStats()
		p.CPU = cpu.UserTime + cpu.SysTime
		if mm := taskMM(leader); mm != nil {
			p.RSS = mm.ResidentSetSize()
			p.Args = readArgs(leader, mm)
			mm.DecUsers(k.SupervisorContext())
		}
		if len(p.Args) == 0 {
			p.Args = []string{p.Command}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// taskMM returns the memory manager of t with one more user, or nil when t
// has none. The caller drops the user with DecUsers. A task changes its
// memory manager on execve and exit, so the read holds the task lock, and the
// user keeps the memory manager alive after it, as /proc does.
func taskMM(t *kernel.Task) *mm.MemoryManager {
	var m *mm.MemoryManager
	t.WithMuLocked(func(t *kernel.Task) { m = t.MemoryManager() })
	if m == nil || !m.IncUsers() {
		return nil
	}
	return m
}

// readArgs reads the argument vector of t from the memory manager m, which
// the caller holds with taskMM.
func readArgs(t *kernel.Task, mm *mm.MemoryManager) []string {
	start, end := mm.ArgvStart(), mm.ArgvEnd()
	if end <= start || end-start > 64*1024 {
		return nil
	}
	buf := make([]byte, end-start)
	n, err := mm.CopyIn(t, start, buf, usermem.IOOpts{})
	if err != nil && n == 0 {
		return nil
	}
	buf = buf[:n]
	return strings.Split(strings.TrimRight(string(buf), "\x00"), "\x00")
}

// Usage is a sample of the resource use of the guest.
type Usage struct {
	MemoryBytes uint64
	MemoryLimit uint64
	CPU         time.Duration
	PIDs        int
	RxBytes     uint64
	TxBytes     uint64
	RxPackets   uint64
	TxPackets   uint64
}

// Usage returns the current resource use of the guest.
func (s *Sandbox) Usage() (Usage, error) {
	s.mu.Lock()
	k := s.activeKernel
	st := s.netStack
	s.mu.Unlock()
	if k == nil {
		return Usage{}, ErrNotRunning
	}
	var u Usage
	// The memory accounting of the memory file counts pages after a scan,
	// and a scan runs at most once per clock tick. The resident sets of the
	// processes give a current figure, and the larger value wins.
	mf := k.MemoryFile()
	_ = mf.UpdateUsage(nil)
	_, accounted := mf.Accounting().Copy()
	var rss uint64
	u.MemoryLimit = uint64(s.cfg.Resources.MemoryBytes)
	if u.MemoryLimit == 0 {
		// The host lockdown denies the sysctl, so New read it before.
		u.MemoryLimit = s.hostRAM
	}
	ns := k.RootPIDNamespace()
	for _, tg := range ns.ThreadGroups() {
		cpu := tg.CPUStats()
		u.CPU += cpu.UserTime + cpu.SysTime
		u.PIDs++
		if leader := tg.Leader(); leader != nil {
			if mm := taskMM(leader); mm != nil {
				rss += mm.ResidentSetSize()
				mm.DecUsers(k.SupervisorContext())
			}
		}
	}
	u.MemoryBytes = max(accounted, rss)
	if st != nil {
		for _, info := range st.NICInfo() {
			if info.Name != "eth0" {
				continue
			}
			u.RxBytes += info.Stats.Rx.Bytes.Value()
			u.TxBytes += info.Stats.Tx.Bytes.Value()
			u.RxPackets += info.Stats.Rx.Packets.Value()
			u.TxPackets += info.Stats.Tx.Packets.Value()
		}
	}
	return u, nil
}
