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
	"strconv"
	"time"
)

// containerEvent is one Docker event of a container.
type containerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Status string `json:"status,omitempty"`
	ID     string `json:"id,omitempty"`
	From   string `json:"from,omitempty"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Scope    string `json:"scope"`
	Time     int64  `json:"time"`
	TimeNano int64  `json:"timeNano"`
}

func newContainerEvent(rec containerRecord, action string) containerEvent {
	now := time.Now()
	e := containerEvent{Type: "container", Action: action, Status: action, ID: rec.ID, From: rec.Config.Image, Scope: "local", Time: now.Unix(), TimeNano: now.UnixNano()}
	e.Actor.ID = rec.ID
	e.Actor.Attributes = map[string]string{"name": rec.Name, "image": rec.Config.Image}
	for k, v := range rec.Config.Labels {
		e.Actor.Attributes[k] = v
	}
	if action == "die" {
		e.Actor.Attributes["exitCode"] = strconv.Itoa(rec.State.ExitCode)
	}
	return e
}

// observedState is the part of a container state that events report.
type observedState struct {
	exists     bool
	running    bool
	startedAt  time.Time
	finishedAt time.Time
	health     string
}

func observe(rec containerRecord) observedState {
	o := observedState{exists: true, running: rec.State.Running, startedAt: rec.State.StartedAt, finishedAt: rec.State.FinishedAt}
	if rec.State.Health != nil {
		o.health = rec.State.Health.Status
	}
	return o
}

// diffEvents returns the events between two observations of one container.
func diffEvents(rec containerRecord, before, after observedState) []string {
	var out []string
	if !before.exists {
		out = append(out, "create")
	}
	if after.running && (!before.running || !after.startedAt.Equal(before.startedAt)) {
		out = append(out, "start")
	}
	if !after.running && !after.finishedAt.IsZero() && !after.finishedAt.Equal(before.finishedAt) && (before.running || !before.exists || !before.finishedAt.IsZero()) {
		out = append(out, "die")
	}
	if after.health != before.health && (after.health == "healthy" || after.health == "unhealthy") {
		out = append(out, "health_status: "+after.health)
	}
	return out
}

// streamEvents polls the container records and writes the changes as Docker
// events until the client goes away or the until time passes.
func (s *server) streamEvents(w http.ResponseWriter, r *http.Request) {
	filters, err := parseFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var until time.Time
	if u := r.URL.Query().Get("until"); u != "" {
		if n, err := strconv.ParseInt(u, 10, 64); err == nil {
			until = time.Unix(n, 0)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	seen := map[string]observedState{}
	records := map[string]containerRecord{}
	if list, err := s.containers.list(); err == nil {
		for _, rec := range list {
			seen[rec.ID] = observe(rec)
			records[rec.ID] = rec
		}
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		if !until.IsZero() && time.Now().After(until) {
			return
		}
		list, err := s.containers.list()
		if err != nil {
			continue
		}
		current := map[string]bool{}
		for _, rec := range list {
			current[rec.ID] = true
			after := observe(rec)
			for _, action := range diffEvents(rec, seen[rec.ID], after) {
				if eventMatches(rec, action, filters) {
					_ = enc.Encode(newContainerEvent(rec, action))
				}
			}
			seen[rec.ID] = after
			records[rec.ID] = rec
		}
		for id, rec := range records {
			if current[id] {
				continue
			}
			if eventMatches(rec, "destroy", filters) {
				_ = enc.Encode(newContainerEvent(rec, "destroy"))
			}
			delete(records, id)
			delete(seen, id)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func eventMatches(rec containerRecord, action string, filters map[string][]string) bool {
	for key, values := range filters {
		switch key {
		case "type":
			if !contains(values, "container") {
				return false
			}
		case "event":
			base := action
			if len(action) > 13 && action[:13] == "health_status" {
				base = "health_status"
			}
			if !contains(values, action) && !contains(values, base) {
				return false
			}
		case "container":
			if !contains(values, rec.ID) && !contains(values, rec.Name) {
				return false
			}
		case "label":
			if !matchesFilters(rec, map[string][]string{"label": values}) {
				return false
			}
		}
	}
	return true
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
