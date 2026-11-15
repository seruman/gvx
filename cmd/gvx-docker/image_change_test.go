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
	"reflect"
	"testing"
)

// TestEnvChangeKeepsTheEnvOfTheBaseImage applies --change 'ENV …' to a config
// that came from JSON, as commit decodes the config of the base image. The
// change adds a variable, replaces a variable with the same name, and keeps
// every other variable, as Docker does.
func TestEnvChangeKeepsTheEnvOfTheBaseImage(t *testing.T) {
	var cfg map[string]any
	if err := json.Unmarshal([]byte(`{"Env":["PATH=/usr/bin:/bin","LANG=C"]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	applyChange(cfg, "ENV FOO=bar")
	applyChange(cfg, "ENV LANG C.UTF-8")
	raw, _ := json.Marshal(cfg["Env"])
	var got []string
	_ = json.Unmarshal(raw, &got)
	want := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "FOO=bar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Env = %q, want %q", got, want)
	}
}
