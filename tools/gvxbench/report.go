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
	"cmp"
	"fmt"
	"math"
	"slices"
)

// Result is the output of one run of the suite. Its JSON form is the file
// that "gvxbench run" writes and "gvxbench compare" reads.
type Result struct {
	// Env names the column, such as "gvx-hvf" or "linux-vm".
	Env string `json:"env"`
	// Commit is the git commit of the measured build, when known.
	Commit  string            `json:"commit,omitempty"`
	Host    string            `json:"host,omitempty"`
	Metrics map[string]Metric `json:"metrics"`
}

// Metric holds the samples of one measurement. Lower is better for every
// metric of the suite.
type Metric struct {
	Unit    string    `json:"unit"`
	Samples []float64 `json:"samples"`
	// Median is the median of Samples, stored for readers of the file.
	Median float64 `json:"median"`
}

func newMetric(unit string, samples []float64) Metric {
	return Metric{Unit: unit, Samples: samples, Median: median(samples)}
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := slices.Clone(v)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// Verdict is the outcome of one metric in a comparison.
type Verdict string

const (
	VerdictSame      Verdict = "same"
	VerdictFaster    Verdict = "faster"
	VerdictSlower    Verdict = "slower"
	VerdictMissing   Verdict = "missing"
	VerdictNew       Verdict = "new"
	VerdictUncertain Verdict = "uncertain"
)

// Row is one metric of a comparison.
type Row struct {
	Name     string
	Unit     string
	Baseline float64
	Current  float64
	// Change is Current/Baseline - 1, so 0.25 is 25% slower.
	Change  float64
	Verdict Verdict
}

// Regression reports whether r fails the gate.
func (r Row) Regression() bool {
	return r.Verdict == VerdictSlower || r.Verdict == VerdictMissing
}

// compareMedians compares the medians of current against baseline. A metric
// whose median moves more than threshold is faster or slower. A metric in
// the baseline but not in current is missing, and one only in current is new.
func compareMedians(baseline, current Result, threshold float64) []Row {
	var rows []Row
	for name, b := range baseline.Metrics {
		c, ok := current.Metrics[name]
		if !ok {
			rows = append(rows, Row{Name: name, Unit: b.Unit, Baseline: b.Median, Verdict: VerdictMissing})
			continue
		}
		rows = append(rows, classify(name, b.Unit, b.Median, c.Median, threshold))
	}
	for name, c := range current.Metrics {
		if _, ok := baseline.Metrics[name]; !ok {
			rows = append(rows, Row{Name: name, Unit: c.Unit, Current: c.Median, Verdict: VerdictNew})
		}
	}
	slices.SortFunc(rows, func(a, b Row) int { return cmp.Compare(a.Name, b.Name) })
	return rows
}

func classify(name, unit string, base, cur, threshold float64) Row {
	r := Row{Name: name, Unit: unit, Baseline: base, Current: cur}
	if base <= 0 {
		r.Verdict = VerdictSame
		return r
	}
	r.Change = cur/base - 1
	switch {
	case r.Change > threshold:
		r.Verdict = VerdictSlower
	case r.Change < -threshold:
		r.Verdict = VerdictFaster
	default:
		r.Verdict = VerdictSame
	}
	return r
}

// comparePaired compares builds that ran in alternating rounds: base[i] and
// head[i] ran back to back. For each metric it takes the ratio of every
// round, and calls the change real only when every round moves the same way
// and the median ratio passes threshold. Host noise moves both builds of a
// round together, so this verdict needs fewer rounds than compareMedians.
func comparePaired(base, head []Result, threshold float64) ([]Row, error) {
	if len(base) == 0 || len(base) != len(head) {
		return nil, fmt.Errorf("paired comparison needs the same number of base and head runs, got %d and %d", len(base), len(head))
	}
	names := map[string]string{}
	for _, r := range append(slices.Clone(base), head...) {
		for name, m := range r.Metrics {
			names[name] = m.Unit
		}
	}
	var rows []Row
	for name, unit := range names {
		var ratios, bases, heads []float64
		for i := range base {
			b, okB := base[i].Metrics[name]
			h, okH := head[i].Metrics[name]
			if !okB || !okH || b.Median <= 0 {
				continue
			}
			ratios = append(ratios, h.Median/b.Median)
			bases = append(bases, b.Median)
			heads = append(heads, h.Median)
		}
		if len(ratios) == 0 {
			rows = append(rows, Row{Name: name, Unit: unit, Verdict: VerdictMissing})
			continue
		}
		r := Row{Name: name, Unit: unit, Baseline: median(bases), Current: median(heads), Change: median(ratios) - 1}
		slower, faster := 0, 0
		for _, x := range ratios {
			if x > 1 {
				slower++
			} else if x < 1 {
				faster++
			}
		}
		switch {
		case math.Abs(r.Change) <= threshold:
			r.Verdict = VerdictSame
		case r.Change > 0 && slower == len(ratios):
			r.Verdict = VerdictSlower
		case r.Change < 0 && faster == len(ratios):
			r.Verdict = VerdictFaster
		default:
			r.Verdict = VerdictUncertain
		}
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b Row) int { return cmp.Compare(a.Name, b.Name) })
	return rows, nil
}

// formatRows renders rows as a fixed-width table.
func formatRows(rows []Row) []string {
	lines := []string{fmt.Sprintf("%-28s %14s %14s %8s  %s", "metric", "baseline", "current", "change", "verdict")}
	for _, r := range rows {
		change := "-"
		if r.Verdict != VerdictMissing && r.Verdict != VerdictNew {
			change = fmt.Sprintf("%+.1f%%", 100*r.Change)
		}
		lines = append(lines, fmt.Sprintf("%-28s %14s %14s %8s  %s", r.Name, formatValue(r.Baseline, r.Unit), formatValue(r.Current, r.Unit), change, r.Verdict))
	}
	return lines
}

func formatValue(v float64, unit string) string {
	if v == 0 || math.IsNaN(v) {
		return "-"
	}
	switch unit {
	case "ns":
		switch {
		case v >= 1e9:
			return fmt.Sprintf("%.2f s", v/1e9)
		case v >= 1e6:
			return fmt.Sprintf("%.2f ms", v/1e6)
		case v >= 1e3:
			return fmt.Sprintf("%.2f us", v/1e3)
		}
		return fmt.Sprintf("%.0f ns", v)
	}
	return fmt.Sprintf("%.3g %s", v, unit)
}
