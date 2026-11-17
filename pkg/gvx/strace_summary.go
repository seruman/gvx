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
	"cmp"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"z.gfire.dev/gvx/pkg/log"
	"z.gfire.dev/gvx/pkg/sentry/arch"
	"z.gfire.dev/gvx/pkg/sentry/kernel"
	"z.gfire.dev/gvx/pkg/sentry/strace"
)

// traceMode selects what GVX_STRACE records for the guests of one sandbox.
type traceMode int

const (
	traceOff traceMode = iota
	// traceLines logs the entry and the exit of each system call.
	traceLines
	// traceSummary counts the calls, errors, and time of each system call
	// and logs one table when the guest kernel ends.
	traceSummary
)

// traceModeFromEnv reads GVX_STRACE: "summary" selects traceSummary, any
// other non-empty value selects traceLines.
func traceModeFromEnv() traceMode {
	switch v := os.Getenv("GVX_STRACE"); v {
	case "":
		return traceOff
	case "summary":
		return traceSummary
	default:
		return traceLines
	}
}

// kernelTraces maps each running *kernel.Kernel to its *kernelTrace. The
// system call tables are process-wide, and one process can run several
// sandboxes, so the tracer finds the state of a call through the kernel of
// its task.
var kernelTraces sync.Map

// kernelTrace is the trace state of one guest kernel.
type kernelTrace struct {
	mode   traceMode
	counts syscallCounts
}

// traceInstallOnce installs the gvx tracer in every system call table.
var traceInstallOnce sync.Once

// startKernelTrace registers the trace of k for mode. It installs the tracer
// on first use: after package initialization, because the init functions of
// keyctl.go and reboot.go rebuild the tables and clear the trace flags.
func startKernelTrace(k *kernel.Kernel, mode traceMode) {
	if mode == traceOff {
		return
	}
	traceInstallOnce.Do(installTracer)
	kernelTraces.Store(k, &kernelTrace{mode: mode})
}

// endKernelTrace removes the trace of k. For traceSummary it logs the
// summary table.
func endKernelTrace(k *kernel.Kernel) {
	v, ok := kernelTraces.LoadAndDelete(k)
	if !ok {
		return
	}
	kt := v.(*kernelTrace)
	if kt.mode != traceSummary {
		return
	}
	names := linuxSyscallNames()
	for _, line := range kt.counts.table(names) {
		log.Infof("%s", line)
	}
}

func installTracer() {
	strace.Initialize()
	for _, table := range kernel.SyscallTables() {
		lines, ok := strace.Lookup(table.OS, table.Arch)
		if !ok {
			continue
		}
		table.Stracer = gvxTracer{lines: lines}
	}
	strace.EnableAll(strace.SinkTypeLog)
}

// linuxSyscallNames returns the system call names of the host architecture.
func linuxSyscallNames() func(uintptr) string {
	for _, table := range kernel.SyscallTables() {
		if table.Arch != arch.Host {
			continue
		}
		if m, ok := strace.Lookup(table.OS, table.Arch); ok {
			return m.Name
		}
	}
	return func(sysno uintptr) string { return fmt.Sprintf("sys_%d", sysno) }
}

// gvxTracer implements kernel.Stracer for one system call table. It routes
// each call to the line tracer of upstream or to the counters of the
// kernel of the task.
type gvxTracer struct {
	lines strace.SyscallMap
}

// traceCall is the private data that SyscallEnter passes to SyscallExit.
type traceCall struct {
	kt    *kernelTrace
	start time.Time
	lines any
}

// SyscallEnter implements kernel.Stracer.SyscallEnter.
func (g gvxTracer) SyscallEnter(t *kernel.Task, sysno uintptr, args arch.SyscallArguments, flags uint32) any {
	v, ok := kernelTraces.Load(t.Kernel())
	if !ok {
		return nil
	}
	kt := v.(*kernelTrace)
	c := &traceCall{kt: kt}
	if kt.mode == traceLines {
		c.lines = g.lines.SyscallEnter(t, sysno, args, flags)
	} else {
		c.start = time.Now()
	}
	return c
}

// SyscallExit implements kernel.Stracer.SyscallExit.
func (g gvxTracer) SyscallExit(context any, t *kernel.Task, sysno, rval uintptr, err error) {
	c, ok := context.(*traceCall)
	if !ok {
		return
	}
	if c.kt.mode == traceLines {
		g.lines.SyscallExit(c.lines, t, sysno, rval, err)
		return
	}
	c.kt.counts.record(sysno, time.Since(c.start), err != nil)
}

// maxCountedSysno bounds the system call numbers that a summary counts. The
// Linux tables of arm64 and amd64 end below 512; a larger number goes
// uncounted.
const maxCountedSysno = 511

// syscallCounts holds the counters of each system call number.
type syscallCounts struct {
	calls  [maxCountedSysno + 1]atomic.Uint64
	errors [maxCountedSysno + 1]atomic.Uint64
	nanos  [maxCountedSysno + 1]atomic.Int64
}

func (c *syscallCounts) record(sysno uintptr, d time.Duration, failed bool) {
	if sysno > maxCountedSysno {
		return
	}
	c.calls[sysno].Add(1)
	c.nanos[sysno].Add(int64(d))
	if failed {
		c.errors[sysno].Add(1)
	}
}

// table formats the counters as text lines, the slowest total time first,
// with a total line at the end.
func (c *syscallCounts) table(name func(uintptr) string) []string {
	type row struct {
		name   string
		calls  uint64
		errors uint64
		nanos  int64
	}
	var rows []row
	var total row
	for sysno := range c.calls {
		calls := c.calls[sysno].Load()
		if calls == 0 {
			continue
		}
		r := row{name: name(uintptr(sysno)), calls: calls, errors: c.errors[sysno].Load(), nanos: c.nanos[sysno].Load()}
		rows = append(rows, r)
		total.calls += r.calls
		total.errors += r.errors
		total.nanos += r.nanos
	}
	slices.SortFunc(rows, func(a, b row) int {
		if n := cmp.Compare(b.nanos, a.nanos); n != 0 {
			return n
		}
		return cmp.Compare(a.name, b.name)
	})
	format := func(r row, name string) string {
		return fmt.Sprintf("strace summary: %10d %12.3f %10.2f %8d  %s",
			r.calls, float64(r.nanos)/1e6, float64(r.nanos)/1e3/float64(r.calls), r.errors, name)
	}
	lines := []string{fmt.Sprintf("strace summary: %10s %12s %10s %8s  %s", "calls", "total_ms", "mean_us", "errors", "syscall")}
	for _, r := range rows {
		lines = append(lines, format(r, r.name))
	}
	if total.calls > 0 {
		lines = append(lines, format(total, "total"))
	}
	return lines
}
