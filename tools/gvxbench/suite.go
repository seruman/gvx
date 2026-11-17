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

// Workload is one timed command of the suite. The suite runs each one in a
// fresh container, so a sample includes the start of the sandbox: that is
// the cost that a user of docker run sees.
type Workload struct {
	// Name is the metric name, unique in the suite.
	Name string
	// Tier groups workloads: "micro" runs the micro-benchmark guest, "tool"
	// runs a short command in a small image, "app" runs a real workload.
	Tier string
	// Image is a digest-pinned image reference. The digest fixes the bytes
	// that the workload runs, so a baseline stays valid when a tag moves.
	Image string
	// Argv is the command in the container.
	Argv []string
	// Env holds KEY=VALUE pairs for the container.
	Env []string
}

// Pinned images (linux/arm64 manifests of 2026-11-17).
const (
	imagePython = "python@sha256:9ef1a599f863231c3d91e941389bfdc4a2d6070557243d54df1a4e57015b960e"
	imageNode   = "node@sha256:1314fb30ec37592d5845e5a160d522c0e680972aa534549940b8d73bde88481a"
	imageGolang = "golang@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191"
)

// goBuildScript builds a program that imports net/http with an empty build
// cache, so the compiler and the linker do the full work every sample.
const goBuildScript = `set -e
mkdir -p /tmp/h && cd /tmp/h
printf 'package main\nimport ("fmt";"net/http")\nfunc main(){fmt.Println(http.StatusOK)}\n' > m.go
go mod init h >/dev/null 2>&1
go build -o h .
./h >/dev/null`

// shellLoopScript forks and executes small programs: the cost of a shell
// script that calls many tools.
const shellLoopScript = `i=0; while [ $i -lt 200 ]; do echo $i | grep -q 5; cat /etc/os-release >/dev/null; i=$((i+1)); done`

// pythonImportScript imports modules whose import maps and unmaps memory
// thousands of times.
const pythonImportScript = `import json, ssl, asyncio, sqlite3, decimal, email, http.client, xml.etree.ElementTree`

// nodeScript computes, hashes, and waits on a timer: the startup and the
// clock reads of a node program.
const nodeScript = `let s=0; for (let i=0;i<1e7;i++) s+=i; require("crypto").createHash("sha256").update(String(s)).digest("hex"); setTimeout(()=>{},20)`

// defaultSuite is the fixed set of workloads. Change it only with a new
// baseline: a changed workload makes old results incomparable.
func defaultSuite() []Workload {
	return []Workload{
		{Name: "tool/true", Tier: "tool", Image: imagePython, Argv: []string{"true"}},
		{Name: "tool/python_pass", Tier: "tool", Image: imagePython, Argv: []string{"python3", "-c", "pass"}},
		{Name: "tool/shell_loop", Tier: "tool", Image: imagePython, Argv: []string{"sh", "-c", shellLoopScript}},
		{Name: "app/python_import", Tier: "app", Image: imagePython, Argv: []string{"python3", "-c", pythonImportScript}},
		{Name: "app/node_compute", Tier: "app", Image: imageNode, Argv: []string{"node", "-e", nodeScript}},
		{Name: "app/go_build", Tier: "app", Image: imageGolang, Argv: []string{"sh", "-c", goBuildScript}, Env: []string{"GOCACHE=/tmp/gocache", "CGO_ENABLED=0", "GOFLAGS=-mod=mod"}},
	}
}

// selectWorkloads returns the workloads whose tier is in tiers, or every
// workload when tiers is empty.
func selectWorkloads(all []Workload, tiers map[string]bool) []Workload {
	if len(tiers) == 0 {
		return all
	}
	var out []Workload
	for _, w := range all {
		if tiers[w.Tier] {
			out = append(out, w)
		}
	}
	return out
}
