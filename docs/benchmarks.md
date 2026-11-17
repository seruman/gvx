# Benchmarks

`tools/gvxbench` measures `gvx` on a fixed suite and compares a run with a baseline. Use it to prove that a change makes `gvx` faster, or that it does not make `gvx` slower. The design follows the benchmark suite of `elfuse`.

## 1. Suite (Descriptive Module)

Each workload runs in a fresh container, so a sample includes the start of the sandbox. That is the cost that a user of `docker run` sees. The images are pinned by `linux/arm64` digest, so a baseline stays valid when a tag moves.

| Tier | Metric | Workload |
| :--- | :--- | :--- |
| `micro` | `micro/getpid`, `open_close`, `pipe_pingpong`, `futex_pingpong`, `fork_wait`, `fork_exec_true` | The guest `internal/acceptprog/microbench`. One run gives one sample of each metric. |
| `tool` | `tool/true` | Start and stop of a container. |
| `tool` | `tool/python_pass` | Start of the Python interpreter. |
| `tool` | `tool/shell_loop` | A shell loop of 200 `grep` and `cat` calls: fork, exec, and wait. |
| `app` | `app/python_import` | Python imports modules that map and unmap memory thousands of times. |
| `app` | `app/node_compute` | `node` computes, hashes, and waits on a timer. |
| `app` | `app/go_build` | `go build` of a program that imports `net/http`, with an empty build cache. |

Each metric records its samples and their median. Lower is better for every metric. A change to a workload makes old results incomparable, so it needs a new baseline.

## 2. Comparison Modes (Descriptive Module)

The tool has two comparison modes. Use the paired mode to judge one change on one host.

| Mode | Command | Verdict |
| :--- | :--- | :--- |
| Baseline | `compare -baseline FILE -results FILE` | A metric is `slower` when its median grows by more than the threshold (default 20%). A metric of the baseline that the run lacks is `missing`. Both fail the gate with exit status 1. |
| Paired | `compare -paired -base FILE... -head FILE...` | The base and head builds run in alternating rounds. A metric is `slower` or `faster` only when every round moves the same way and the median ratio passes the threshold (default 5%). Otherwise it is `same` or `uncertain`. |

Host noise moves both builds of a round together, so the paired mode needs fewer rounds than the baseline mode. `-report-only` prints the regressions and exits 0.

## 3. Procedure (Procedural Module)

> [!IMPORTANT]
> Close other heavy programs before a run. A busy host makes every sample slower and hides small changes.

1. Build and sign `rungv` for each build that you compare.
2. Run the suite: `go run ./tools/gvxbench run -rungv ./rungv -o head.json`.
3. Compare with the baseline: `go run ./tools/gvxbench compare -baseline tools/gvxbench/baseline/darwin-arm64-hvf.json -results head.json`.
4. For one change, run the base and head builds in turns, three rounds or more.
5. Compare the rounds: `go run ./tools/gvxbench compare -paired -base base-1.json -base base-2.json -base base-3.json -head head-1.json -head head-2.json -head head-3.json`.
6. Replace the baseline file only when a change makes the suite faster on purpose.

`-engine docker -docker-context NAME` runs the same suite in a Docker context, for a reference column such as a Linux virtual machine. `-tiers micro,tool` runs a subset. The first run pulls the pinned images into the image cache of `gvxbench`.

## 4. Reference Results (Reference Module)

The baseline file holds the result of commit `24ac289a6c6b` on an Apple M5 Pro, with 5 samples for each metric.

| Metric | Median |
| :--- | ---: |
| `tool/true` | 34.82 ms |
| `tool/python_pass` | 56.83 ms |
| `tool/shell_loop` | 242.11 ms |
| `app/python_import` | 580.35 ms |
| `app/node_compute` | 151.29 ms |
| `app/go_build` | 4.00 s |
| `micro/getpid` | 1.25 us |
| `micro/open_close` | 3.75 us |
| `micro/pipe_pingpong` | 8.75 us |
| `micro/futex_pingpong` | 9.08 us |
| `micro/fork_wait` | 659.12 us |
| `micro/fork_exec_true` | 207.98 us |

A paired run of three rounds compared commit `b34194af` with `24ac289a`, which makes the vDSO clock ready at boot. It found `app/go_build` 10.8% faster and every other metric the same.
