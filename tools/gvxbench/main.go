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

// Command gvxbench measures gvx on a fixed suite and compares the results
// with a baseline.
//
//	gvxbench run [-engine rungv|docker] [-env NAME] [-o FILE] [-tiers tool,app]
//	gvxbench compare -baseline FILE -results FILE [-threshold 0.20] [-report-only]
//	gvxbench compare -paired -base FILE... -head FILE... [-threshold 0.05]
//
// "run" times each workload in a fresh container, so a sample includes the
// start of the sandbox. "compare" exits 1 when a metric is slower than the
// baseline by more than the threshold, or missing. The syscall
// micro-benchmarks are TestE2EMicroBench in e2e.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var gate errGate
		if errors.As(err, &gate) {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "gvxbench:", err)
		os.Exit(2)
	}
}

// errGate reports that a comparison found a regression.
type errGate struct{ count int }

func (e errGate) Error() string { return fmt.Sprintf("%d metrics regressed", e.count) }

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: gvxbench run|compare [flags]")
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "compare":
		return cmdCompare(args[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdRun(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	engine := fs.String("engine", "rungv", "engine: rungv or docker")
	rungv := fs.String("rungv", "./rungv", "path of the rungv binary")
	cacheDir := fs.String("cache-dir", "", "image cache of rungv (default: a gvxbench directory in the user cache)")
	dockerContext := fs.String("docker-context", "", "Docker context of the docker engine")
	env := fs.String("env", "", "column name in the result (default: the engine name)")
	out := fs.String("o", "", "write the JSON result to this file (default: stdout)")
	iterations := fs.Int("iterations", 5, "timed samples for each workload")
	warmup := fs.Int("warmup", 1, "untimed samples before the timed ones")
	tiers := fs.String("tiers", "", "comma-separated tiers to run: tool, app (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *iterations < 1 || *warmup < 0 {
		return errors.New("-iterations must be at least 1 and -warmup at least 0")
	}

	var e Engine
	switch *engine {
	case "rungv":
		dir := *cacheDir
		if dir == "" {
			base, err := os.UserCacheDir()
			if err != nil {
				return err
			}
			dir = base + "/gvxbench"
		}
		e = rungvEngine{bin: *rungv, cacheDir: dir}
	case "docker":
		e = dockerEngine{context: *dockerContext}
	default:
		return fmt.Errorf("unknown engine %q", *engine)
	}
	selected, err := parseTiers(*tiers)
	if err != nil {
		return err
	}
	opts := Options{Iterations: *iterations, Warmup: *warmup, Tiers: selected,
		Progress: func(line string) { fmt.Fprintln(stderr, line) }}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	metrics, err := runSuite(ctx, e, selectWorkloads(defaultSuite(), opts.Tiers), opts)
	if err != nil {
		return err
	}
	res := Result{Env: *env, Commit: gitCommit(), Metrics: metrics}
	if res.Env == "" {
		res.Env = *engine
	}
	if h, err := os.Hostname(); err == nil {
		res.Host = h
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if *out == "" {
		_, err = stdout.Write(data)
		return err
	}
	return os.WriteFile(*out, data, 0o644)
}

// parseTiers returns the tiers that s names, or nil for all of them.
func parseTiers(s string) (map[string]bool, error) {
	if s == "" {
		return nil, nil
	}
	m := map[string]bool{}
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t != "tool" && t != "app" {
			return nil, fmt.Errorf("unknown tier %q: want tool or app", t)
		}
		m[t] = true
	}
	return m, nil
}

func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func cmdCompare(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	baseline := fs.String("baseline", "", "baseline result file")
	results := fs.String("results", "", "current result file")
	threshold := fs.Float64("threshold", -1, "fractional change that counts (default 0.20, or 0.05 with -paired)")
	reportOnly := fs.Bool("report-only", false, "print regressions and exit 0")
	paired := fs.Bool("paired", false, "compare alternating rounds: -base and -head name files in round order")
	var base, head multiFlag
	fs.Var(&base, "base", "result file of the base build (repeat, -paired)")
	fs.Var(&head, "head", "result file of the head build (repeat, -paired)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var rows []Row
	if *paired {
		if *threshold < 0 {
			*threshold = 0.05
		}
		bs, err := readResults(base)
		if err != nil {
			return err
		}
		hs, err := readResults(head)
		if err != nil {
			return err
		}
		if rows, err = comparePaired(bs, hs, *threshold); err != nil {
			return err
		}
	} else {
		if *baseline == "" || *results == "" {
			return errors.New("compare needs -baseline and -results, or -paired with -base and -head")
		}
		if *threshold < 0 {
			*threshold = 0.20
		}
		rs, err := readResults([]string{*baseline, *results})
		if err != nil {
			return err
		}
		rows = compareMedians(rs[0], rs[1], *threshold)
	}
	for _, line := range formatRows(rows) {
		fmt.Fprintln(stdout, line)
	}
	regressions := 0
	for _, r := range rows {
		if r.Regression() {
			regressions++
		}
	}
	if regressions > 0 && !*reportOnly {
		return errGate{count: regressions}
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func readResults(paths []string) ([]Result, error) {
	var out []Result
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var r Result
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, r)
	}
	return out, nil
}
