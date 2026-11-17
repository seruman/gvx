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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	// RunBinary runs a static Linux executable from the host in a fresh
	// container and returns its stdout.
	RunBinary(ctx context.Context, path string, argv []string) ([]byte, error)
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

func (e rungvEngine) RunBinary(ctx context.Context, path string, argv []string) ([]byte, error) {
	args := append([]string{"run", "-cache-dir", e.cacheDir, "-net", "none", "-bin", path, "--"}, argv...)
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

func (e dockerEngine) RunBinary(ctx context.Context, path string, argv []string) ([]byte, error) {
	args := append([]string{"run", "--rm", "--network", "none", "--pull", "never", "-v", path + ":/microbench:ro", imagePython, "/microbench"}, argv...)
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
	// MicroBench is the path of the micro-benchmark guest, or empty to skip
	// the micro tier.
	MicroBench string
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
	if opts.MicroBench != "" && (len(opts.Tiers) == 0 || opts.Tiers["micro"]) {
		if err := runMicro(ctx, e, opts, metrics); err != nil {
			return nil, err
		}
	}
	return metrics, nil
}

func (o Options) progress(name string, m Metric) {
	if o.Progress != nil {
		o.Progress(fmt.Sprintf("%-28s %s", name, formatValue(m.Median, m.Unit)))
	}
}

// microLine is one MICROBENCH line of the micro-benchmark guest.
type microLine struct {
	Name    string  `json:"name"`
	NsPerOp float64 `json:"ns_per_op"`
}

// microNames are the micro-benchmarks that the suite records. The guest also
// has shm_sparse_idle, which measures idle CPU and not a latency.
var microNames = []string{"getpid", "open_close", "pipe_pingpong", "futex_pingpong", "fork_wait", "fork_exec_true"}

// runMicro runs the micro-benchmark guest Iterations times. Each run
// measures every micro-benchmark once, and each run gives one sample.
func runMicro(ctx context.Context, e Engine, opts Options, metrics map[string]Metric) error {
	samples := map[string][]float64{}
	for i := range opts.Warmup + opts.Iterations {
		out, err := e.RunBinary(ctx, opts.MicroBench, microNames)
		if err != nil {
			return fmt.Errorf("micro-benchmarks: %w", err)
		}
		got, err := parseMicro(out)
		if err != nil {
			return err
		}
		if i < opts.Warmup {
			continue
		}
		for _, name := range microNames {
			v, ok := got[name]
			if !ok {
				return fmt.Errorf("micro-benchmark %s printed no result", name)
			}
			samples[name] = append(samples[name], v)
		}
	}
	for _, name := range microNames {
		metrics["micro/"+name] = newMetric("ns", samples[name])
		opts.progress("micro/"+name, metrics["micro/"+name])
	}
	return nil
}

// parseMicro reads the MICROBENCH lines of the guest. A MICROBENCH_ERR line
// is an error.
func parseMicro(out []byte) (map[string]float64, error) {
	got := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "MICROBENCH_ERR "); ok {
			return nil, fmt.Errorf("micro-benchmark failed: %s", rest)
		}
		rest, ok := strings.CutPrefix(line, "MICROBENCH ")
		if !ok {
			continue
		}
		var m microLine
		if err := json.Unmarshal([]byte(rest), &m); err != nil {
			return nil, fmt.Errorf("micro-benchmark line %q: %w", line, err)
		}
		got[m.Name] = m.NsPerOp
	}
	return got, sc.Err()
}
