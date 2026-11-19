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

package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Engine starts one container for each call and returns after it exits.
type Engine interface {
	// Pull makes image available offline, so no sample includes a download.
	Pull(ctx context.Context, image string) error
	// Run runs argv in a fresh container of image and returns its stdout.
	Run(ctx context.Context, image string, env, argv []string) ([]byte, error)
}

// rungvEngine runs workloads with "rungv run".
type rungvEngine struct {
	bin      string
	cacheDir string
}

func (e rungvEngine) Pull(ctx context.Context, image string) error {
	_, err := runCommand(ctx, e.bin, "image", "pull", "-cache-dir", e.cacheDir, image)
	return err
}

func (e rungvEngine) Run(ctx context.Context, image string, env, argv []string) ([]byte, error) {
	args := []string{"run", "-cache-dir", e.cacheDir, "-net", "none", "-pull", "never", "-image", image}
	for _, kv := range env {
		args = append(args, "-env", kv)
	}
	args = append(append(args, "--"), argv...)
	return runCommand(ctx, e.bin, args...)
}

// dockerEngine runs workloads with "docker run" in a Docker context, for a
// reference column such as a Linux virtual machine.
type dockerEngine struct {
	context string
}

func (e dockerEngine) docker(ctx context.Context, args ...string) ([]byte, error) {
	if e.context != "" {
		args = append([]string{"--context", e.context}, args...)
	}
	return runCommand(ctx, "docker", args...)
}

func (e dockerEngine) Pull(ctx context.Context, image string) error {
	_, err := e.docker(ctx, "pull", "--platform", "linux/arm64", "-q", image)
	return err
}

func (e dockerEngine) Run(ctx context.Context, image string, env, argv []string) ([]byte, error) {
	args := []string{"run", "--rm", "--network", "none", "--pull", "never"}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	args = append(append(args, image), argv...)
	return e.docker(ctx, args...)
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLines(stderr.String(), 5))
	}
	return stdout.Bytes(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// Options configures one run of the suite.
type Options struct {
	Iterations int
	Warmup     int
	Tiers      map[string]bool
	// Progress receives one line for each finished metric.
	Progress func(string)
}

// runSuite measures every workload and returns the metrics. A workload that
// fails returns an error: a benchmark that does not run is a failure, not a
// missing row.
func runSuite(ctx context.Context, e Engine, workloads []Workload, opts Options) (map[string]Metric, error) {
	metrics := map[string]Metric{}
	pulled := map[string]bool{}
	for _, w := range workloads {
		if !pulled[w.Image] {
			if err := e.Pull(ctx, w.Image); err != nil {
				return nil, fmt.Errorf("pull %s: %w", w.Image, err)
			}
			pulled[w.Image] = true
		}
		var samples []float64
		for i := range opts.Warmup + opts.Iterations {
			start := time.Now()
			if _, err := e.Run(ctx, w.Image, w.Env, w.Argv); err != nil {
				return nil, fmt.Errorf("%s: %w", w.Name, err)
			}
			if i >= opts.Warmup {
				samples = append(samples, float64(time.Since(start).Nanoseconds()))
			}
		}
		metrics[w.Name] = newMetric("ns", samples)
		opts.progress(w.Name, metrics[w.Name])
	}
	return metrics, nil
}

func (o Options) progress(name string, m Metric) {
	if o.Progress != nil {
		o.Progress(fmt.Sprintf("%-28s %s", name, formatValue(m.Median, m.Unit)))
	}
}
