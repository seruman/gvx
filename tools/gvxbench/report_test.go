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

import "testing"

func result(metrics map[string]float64) Result {
	r := Result{Metrics: map[string]Metric{}}
	for name, v := range metrics {
		r.Metrics[name] = newMetric("ns", []float64{v})
	}
	return r
}

func verdicts(rows []Row) map[string]Verdict {
	m := map[string]Verdict{}
	for _, r := range rows {
		m[r.Name] = r.Verdict
	}
	return m
}

func TestCompareMediansAppliesTheThreshold(t *testing.T) {
	base := result(map[string]float64{"a": 100, "b": 100, "c": 100, "gone": 100})
	cur := result(map[string]float64{"a": 125, "b": 110, "c": 70, "added": 5})
	got := verdicts(compareMedians(base, cur, 0.20))
	want := map[string]Verdict{"a": VerdictSlower, "b": VerdictSame, "c": VerdictFaster, "gone": VerdictMissing, "added": VerdictNew}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("verdict of %s = %s, want %s", name, got[name], v)
		}
	}
	regressions := 0
	for _, r := range compareMedians(base, cur, 0.20) {
		if r.Regression() {
			regressions++
		}
	}
	if regressions != 2 {
		t.Errorf("regressions = %d, want 2 (slower and missing)", regressions)
	}
}

func TestComparePairedNeedsEveryRoundToAgree(t *testing.T) {
	rounds := func(vals ...float64) []Result {
		var rs []Result
		for _, v := range vals {
			rs = append(rs, result(map[string]float64{"m": v}))
		}
		return rs
	}
	for _, tc := range []struct {
		name       string
		base, head []float64
		want       Verdict
	}{
		{"every round slower", []float64{100, 200, 150}, []float64{110, 220, 165}, VerdictSlower},
		{"every round faster", []float64{100, 200, 150}, []float64{90, 180, 135}, VerdictFaster},
		{"rounds disagree", []float64{100, 100, 100}, []float64{120, 80, 130}, VerdictUncertain},
		{"below threshold", []float64{100, 100, 100}, []float64{101, 102, 101}, VerdictSame},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := comparePaired(rounds(tc.base...), rounds(tc.head...), 0.05)
			if err != nil {
				t.Fatal(err)
			}
			if got := verdicts(rows)["m"]; got != tc.want {
				t.Errorf("verdict = %s, want %s", got, tc.want)
			}
		})
	}
	if _, err := comparePaired(rounds(1, 2), rounds(1), 0.05); err == nil {
		t.Error("comparePaired accepted runs of different counts")
	}
}

func TestMedian(t *testing.T) {
	for _, tc := range []struct {
		in   []float64
		want float64
	}{{[]float64{3, 1, 2}, 2}, {[]float64{4, 1, 3, 2}, 2.5}, {[]float64{7}, 7}} {
		if got := median(tc.in); got != tc.want {
			t.Errorf("median(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseMicroReadsResultsAndRejectsErrors(t *testing.T) {
	out := []byte("MICROBENCH_INFO {}\nMICROBENCH {\"name\":\"getpid\",\"iters\":10,\"total_ns\":13000,\"ns_per_op\":1300}\nnoise\n")
	got, err := parseMicro(out)
	if err != nil || got["getpid"] != 1300 {
		t.Fatalf("parseMicro = %v, %v, want getpid 1300", got, err)
	}
	if _, err := parseMicro([]byte("MICROBENCH_ERR fork_wait: EPERM\n")); err == nil {
		t.Fatal("parseMicro accepted a MICROBENCH_ERR line")
	}
}
