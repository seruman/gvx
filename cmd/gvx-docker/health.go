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
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"time"
)

const (
	defaultHealthInterval = 30 * time.Second
	defaultHealthTimeout  = 30 * time.Second
	defaultHealthRetries  = 3
	maxHealthLog          = 5
)

// healthCommand returns the argv of a health check, or nil when the check is
// off.
func healthCommand(h *healthConfig) []string {
	if h == nil || len(h.Test) == 0 {
		return nil
	}
	switch h.Test[0] {
	case "NONE":
		return nil
	case "CMD":
		return h.Test[1:]
	case "CMD-SHELL":
		if len(h.Test) < 2 {
			return nil
		}
		return []string{"/bin/sh", "-c", h.Test[1]}
	default:
		return h.Test
	}
}

func durationOr(ns int64, fallback time.Duration) time.Duration {
	if ns <= 0 {
		return fallback
	}
	return time.Duration(ns)
}

// runHealthChecks runs the health check of rec until ctx ends. It records the
// result in the state of the container.
func runHealthChecks(ctx context.Context, stateDir string, store *containerStore, rec containerRecord) {
	argv := healthCommand(rec.Config.Healthcheck)
	if argv == nil {
		return
	}
	h := rec.Config.Healthcheck
	interval := durationOr(h.Interval, defaultHealthInterval)
	timeout := durationOr(h.Timeout, defaultHealthTimeout)
	startInterval := durationOr(h.StartInterval, 5*time.Second)
	startPeriod := time.Duration(h.StartPeriod)
	retries := h.Retries
	if retries <= 0 {
		retries = defaultHealthRetries
	}
	started := time.Now()
	state := healthState{Status: "starting"}
	wait := interval
	if startPeriod > 0 {
		wait = startInterval
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		entry := runHealthProbe(ctx, stateDir, rec.ID, argv, timeout)
		inStart := time.Since(started) < startPeriod
		if entry.ExitCode == 0 {
			state.Status = "healthy"
			state.FailingStreak = 0
		} else if !inStart {
			state.FailingStreak++
			if state.FailingStreak >= retries {
				state.Status = "unhealthy"
			}
		}
		state.Log = append(state.Log, entry)
		if len(state.Log) > maxHealthLog {
			state.Log = state.Log[len(state.Log)-maxHealthLog:]
		}
		saveHealth(store, rec.ID, state)
		wait = interval
		if inStart && state.Status != "healthy" {
			wait = startInterval
		}
	}
}

func runHealthProbe(ctx context.Context, stateDir, id string, argv []string, timeout time.Duration) healthLog {
	entry := healthLog{Start: time.Now().UTC(), ExitCode: -1}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, reader, err := controlCall(ctx, controlSocketPath(stateDir, id), controlRequest{Op: "exec", Exec: &execSpec{Cmd: argv}})
	if err != nil {
		entry.Output = err.Error()
		entry.End = time.Now().UTC()
		return entry
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var mu sync.Mutex
	_ = writeControlFrame(&mu, conn, frameEOF, nil)
	var out bytes.Buffer
	for {
		kind, payload, err := readControlFrame(reader)
		if err != nil {
			entry.Output = out.String() + "health check timed out"
			break
		}
		if kind == frameStdout || kind == frameStderr {
			if out.Len() < 4096 {
				out.Write(payload)
			}
			continue
		}
		if kind == frameExit {
			entry.ExitCode = int(binary.BigEndian.Uint32(payload))
			entry.Output = out.String()
			break
		}
	}
	entry.End = time.Now().UTC()
	return entry
}

func saveHealth(store *containerStore, id string, state healthState) {
	copied := state
	copied.Log = append([]healthLog(nil), state.Log...)
	_ = store.updateState(id, func(st *containerState) {
		if st.Running {
			st.Health = &copied
		}
	})
}
