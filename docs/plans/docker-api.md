# Docker API Engine on `gvx`: Implementation Plan

This plan defines a Docker Engine API service for `gvx` on `darwin/arm64`. The host keeps only the Docker CLI, Compose, and Buildx. The service runs no `dockerd`, no Docker Desktop, and no Linux virtual machine.

## 1. Scope and Decisions (Descriptive Module)

The table records the decisions for the first implementation. Later work can change a decision after a measured reason.

| Topic | Decision |
| :--- | :--- |
| Program | Add `cmd/gvx-docker`. Keep Docker semantics out of `pkg/gvx`. |
| First milestone | Builds. BuildKit already ran under `gvx` and exported an image with a `RUN` step. |
| Builder lifetime | Start one BuildKit sandbox on demand and keep it while the engine runs. Idle stop and pre-start come later. |
| Architecture | Native `linux/arm64` only. Cross-architecture builds and runs are out of scope. |
| Compose scope | `up`, `down`, `ps`, `logs`, `restart`, `build`, `depends_on` order, named volumes, and user-defined networks. `--scale` is out of scope. |
| Restart safety | Required. A container must keep running when the API process exits. A new API process must find it again. |
| Security goal | Ordinary program compatibility for one trusted user. The design does not claim isolation of hostile images. |

## 2. Status (Reference Module)

The table records the state of each milestone on `darwin/arm64` with the host Docker CLI 29.9.1, Buildx 0.33.0, and Compose 5.6.0. Section 7 lists the open gaps.

| Milestone | State | Verified commands |
| :--- | :--- | :--- |
| M0 | Complete | BuildKit in a sandbox builds and exports an image. |
| M1 | Complete | `docker build`, `docker buildx build`, `docker compose build`, and cache reuse after an engine restart. |
| M2 | Complete | `run`, `run -d -p`, `ps`, `logs`, `stop`, `start`, `rm`, `--rm`, `--restart on-failure:N`, and state kept across `stop` and `start`. A container runs on after the API process exits. |
| M3 | Complete | `run -i`, `run -it`, `attach`, `exec`, `exec -i`, `exec -it`, and `cp` in both directions for running and stopped containers, without `tar` in the image. |
| M5 | Complete | `rmi`, `tag`, `push`, `save`, `load`, `history`, `image prune`, `commit`, `export`, `import`, `pause`, `unpause`, `top`, `stats`, `rename`, `update`, `system df`, `container prune`, `logs --since --until`, log rotation, `--add-host`, `--dns`, `--internal`, Compose `service_completed_successfully`, and Buildx SSH forwarding and secrets. |
| M6 | Complete | `--read-only`, `--shm-size`, `--group-add`, `--ulimit`, the image `STOPSIGNAL`, `--stop-signal`, `kill -s`, Ctrl-P Ctrl-Q detach for `run -it`, `attach`, and `exec -it`, window resize, a bind of `/var/run/docker.sock`, Testcontainers for Go with Ryuk, `login`, `pull`, `push`, and `run` with a private registry, a private base image in a build, a registry build cache, and published UDP ports from the Mac and the LAN. |
| M4 | Complete | `network create`, names and aliases on a shared network, TCP and UDP between containers, isolation between networks, named, anonymous, and image `VOLUME` volumes with the image files, health checks, `compose up`, `up -d`, `ps`, `logs`, `exec`, `restart`, `down`, and `down -v`. |

## 3. Architecture (Descriptive Module)

The API process serves the Docker Engine API on `~/.gvx/docker.sock` with mode `0600`. It owns records and routes requests. Each container runs in its own supervisor process, so the death of the API process does not stop a container.

```text
docker CLI, Compose, Buildx
        |
        | Docker Engine API (Unix socket)
        v
gvx-docker serve
        |-- state: containers, networks, volumes, events
        |-- image store: pkg/oci layer cache
        |-- builder: one BuildKit sandbox
        `-- one supervisor for each running container
                `-- gvx.Sandbox, logs, control socket
```

A sandbox locks down its host process, so the process cannot start a second sandbox. Each run of a container therefore has two processes. The shim runs the sandbox and locks itself down. The supervisor starts the shim and stays outside the lockdown. It does these tasks:

- It writes the output of the shim to the log file.
- It records the state and the exit code of each run.
- It applies the restart policy and starts a new shim.
- It runs the health checks.
- It answers name lookups for the shim and opens connections to other containers.
- It outlives the API process, so containers keep running when the API process exits.

The shim opens its control socket before `hostlockdown.Apply()`. The API process sends `exec`, `resize`, and `dial` requests to that socket. The supervisor and the shim share one socket pair, because the lockdown blocks new Unix connections. The shim asks for container names on that pair. For each connection to another container, the shim first asks for a session: the supervisor creates a new socket pair, serves one end, and passes the other end with `SCM_RIGHTS`. The dial then runs on the session, so a target that does not answer delays no name lookup. The supervisor passes each new connection to the shim with `SCM_RIGHTS`. It reads the reply of the target shim one byte at a time, so the first bytes of a target that speaks first stay in the socket.

The store chooses the address of a container on a network when it writes the record, under the lock that every process shares. Parallel creates, as Compose sends them, therefore get distinct addresses.

### 3.1 Liveness and Stop (Descriptive Module)

Each supervisor holds an exclusive `flock` on `supervisor.lock` in its container directory for its whole life. The file contains the PID of the holder. The kernel releases the lock when the supervisor dies, also on `SIGKILL`. The engine therefore reads a container as running only while a process holds that lock, and it does not trust a PID: a zombie and a reused PID both answer `kill(pid, 0)`. The lock descriptor has `O_CLOEXEC`, so a shim never inherits it.

The API process never signals a shim. It sends `SIGTERM` (stop) or `SIGUSR1` (kill) to the PID that the lock names, and only while the lock is held. The supervisor sends the stop signal of the container to its shim, waits for the timeout, then sends `SIGUSR2`, and then `SIGKILL`. The shim is an unreaped child of the supervisor, so its PID cannot name another process. `SIGKILL` of the supervisor is the last step of the API process. A shim ends with its supervisor: it reads a pipe from the supervisor and kills its guest when the pipe ends. On Linux, the shim also gets `SIGUSR2` as its parent death signal.

Only the supervisor writes the state of a run. `docker start` writes `StartPending` before it starts the supervisor, and the container reads as running for at most 30 seconds until the supervisor takes its lock. One `serve` holds `serve.lock` in the state directory, so a second `serve` on the same directory exits with an error.

A container gets one address on each of its networks. The guest keeps its NAT address. The DNS relay of the guest answers container names with those addresses. A guest connection to such an address leaves through the NAT forwarder, and the shim routes it to the guest of the target container.

## 4. Stored Data (Reference Module)

| Data | Location | Lifetime |
| :--- | :--- | :--- |
| Image layers and image records | Existing `pkg/oci` cache | Shared by pulls, loads, and builds. `docker rmi` and `docker image prune` remove records. |
| BuildKit state and build cache | Volume `gvx-docker-buildkit` | Survives builder and engine restarts. `docker builder prune` clears it. |
| Container records | `~/.gvx/docker/containers/<id>/config.json` and `state.json` | Removed by `docker rm`. |
| Container writable layer | `~/.gvx/docker/containers/<id>/rootfs.ext4` | Survives stop and start. Removed by `docker rm`. |
| Container logs | `~/.gvx/docker/containers/<id>/log.json` | Rotated at a size limit. Removed by `docker rm`. |
| Supervisor lock | `~/.gvx/docker/containers/<id>/supervisor.lock` | Held by the live supervisor. Removed by `docker rm`. |
| Engine lock | `~/.gvx/docker/serve.lock` | Held by the live `serve`. |
| Shim control socket | `~/.gvx/docker/run/<id12>.sock` | Exists while the container runs. |
| Named volumes | `~/.gvx/docker/volumes/<name>/_data` | Host directories, so several containers share them. Removed by `docker volume rm` or `docker volume prune`. |
| Networks | `~/.gvx/docker/networks/<id>.json` | Removed by `docker network rm`. |
| Registry credentials | `~/.docker/config.json` and credential helpers | Not copied. |

## 5. Milestones (Reference Module)

Each milestone ends with commands that pass from the host Docker CLI.

### M0: BuildKit Feasibility (Complete)

`buildkitd` from `moby/buildkit:buildx-stable-1` ran under `gvx` with the overlay snapshotter on an `ext4lite` volume. It built a Dockerfile with `COPY` and `RUN`, and it exported an OCI archive. `rungv load` imported the archive, and `rungv run` executed it.

### M1: Builds from the Host

Target commands:

```sh
docker context use gvx
docker buildx create --name gvx --driver remote unix://$HOME/.gvx/docker/run/buildkit.sock --use
docker buildx build --load -t app .
docker build -t app .
docker compose build
```

Work items:

1. Serve `/_ping`, `/version`, `/info`, `/images/json`, and `/images/{name}/json`.
2. Start the BuildKit sandbox on the first build request or on `gvx-docker builder start`.
3. Publish the BuildKit socket of the guest as a host Unix socket with mode `0600`.
4. Implement `POST /images/load` with `oci.LoadArchive`, so `--load` reaches the `gvx` image store.
5. Add `gvx-docker setup` to create the Docker context and the Buildx builder.
6. Return a clear error for `POST /build`, `/session`, and `/grpc` until a later milestone adds them.

Acceptance:

- A multi-stage Dockerfile with `ARG`, `COPY`, and `RUN` builds and runs with `rungv run`.
- A second build of the same context reports cached steps.
- The cache survives a restart of the engine.
- A private base image pulls with the credentials of `~/.docker/config.json`.
- Build secrets and SSH forwarding work through Buildx.

### M2: Basic Containers

Target commands:

```sh
docker pull alpine
docker run --rm alpine echo hello
docker run -d --name web -p 8080:80 nginx:alpine
docker ps -a
docker logs -f web
docker stop web
docker start web
docker rm -f web
```

Work items:

1. Implement create, start, stop, kill, wait, remove, list, and inspect.
2. Start one detached supervisor process for each running container.
3. Apply image config and Docker overrides: `Env`, `Cmd`, `Entrypoint`, `WorkingDir`, `User`, and labels.
4. Publish ports before lockdown. Allocate a free host port for `-p 80`.
5. Mount bind sources, named volumes, and `tmpfs`.
6. Store stdout and stderr in the Docker JSON log format.
7. Store the exit code and the state after each exit.
8. Implement restart policies `no`, `always`, `unless-stopped`, and `on-failure`.

Acceptance:

- A detached container keeps running after `gvx-docker serve` exits.
- A new API process lists that container as running and reads its logs.
- Files written in the container stay after `docker stop` and `docker start`.
- `docker rm` removes the writable layer and the logs.

### M3: Attach, Exec, and Files

Target commands:

```sh
docker run -it alpine sh
docker exec -it web sh
docker attach web
docker cp ./file web:/tmp/file
docker cp web:/etc/hostname ./hostname
```

Work items:

1. Add a narrow `pkg/gvx` API to start a process in a running sandbox.
2. Add signal delivery and terminal resize for that process.
3. Implement hijacked HTTP streams and the multiplexed stdout and stderr format.
4. Implement archive get and put through the supervisor.

Acceptance:

- An interactive shell gets input, output, Ctrl-C, and window resize.
- `docker exec` returns the exit code of the process.
- `docker cp` keeps file mode, owner, and symbolic links.

### M4: Networks and Compose

Target commands:

```sh
docker network create appnet
docker run -d --network appnet --name db postgres:17
docker run --rm --network appnet alpine nc -z db 5432
docker compose up -d
docker compose ps
docker compose logs -f
docker compose restart api
docker compose down
```

Work items:

1. Implement network create, list, inspect, remove, connect, and disconnect.
2. Connect the network stacks of sandboxes on one user-defined network.
3. Resolve container names, Compose service names, and aliases on shared networks.
4. Implement the filters, labels, and events that Compose reads.
5. Start services in `depends_on` order.

Acceptance:

- Containers on one network reach each other by service name.
- Containers on different networks cannot reach each other.
- A Compose project with a database, an application, a build step, and named volumes starts and stops cleanly.

## 6. `gvx` Changes (Reference Module)

Each change is in a Tier 1 file. The upstream audit stays at 4.16%.

| Change | Reason | Milestone |
| :--- | :--- | :--- |
| `Config.RootfsUpperPath` | An `ext4lite` image holds the root writes, so `docker stop` and `docker start` keep them. | M2 |
| `Sandbox.Signal` | `docker stop` and `docker kill` send a signal to the main process. | M2 |
| `Sandbox.Exec`, `ExecProcess` | `docker exec` starts a process in a running guest. | M3 |
| `Sandbox.Resize`, `ExecProcess.Resize` | A terminal client changes the window size. | M3 |
| `Config.ControlListener`, `Sandbox.AcceptControlConn` | The shim accepts control requests after the lockdown. | M3 |
| `Config.LocalNames`, `NATConfig.LocalNames` | The DNS relay answers container names. | M4 |
| `Config.NATDialContext` | The NAT forwarder routes container addresses to other sandboxes. | M4 |
| `Sandbox.DialGuestTCP`, `Sandbox.DialGuestUDP` | A shim connects a peer to its own guest. | M4 |
| `Config.TerminalLineDiscipline`, `TerminalRows`, `TerminalCols` | `docker run -it` sends raw terminal input. | M3 |
| `Sandbox.StatPath`, `ArchiveRead`, `ArchiveWrite` | `docker cp` reads and writes guest files without `tar` in the image. | M3 |
| `Config.Idle` | `docker cp` on a stopped container boots its file systems without its command. | M3 |
| `Mount.SeedFromImage` | A new volume starts with the image files at its path, as in Docker. | M4 |
| `oci.RuntimeConfig.Volumes`, `Labels` | Image `VOLUME` lines get anonymous volumes. | M4 |
| `Config.GuestAddresses`, `Sandbox.DialGuestFrom`, `NATGuestSource` | A container has its network address on `eth0` and sees the address of a peer as the source, so Kafka and other clusters advertise reachable addresses. | M4 |
| `Config.GuestFiles` | `/etc/hosts` and `/etc/resolv.conf` follow Docker, with `--add-host` and the resolver of the sandbox. | M4 |
| `Sandbox.Pause`, `Resume`, `Processes`, `Usage` | `docker pause`, `unpause`, `top`, and `stats`. | M5 |
| `Sandbox.ArchiveUpper` | `docker commit` reads the changes of the root as an OCI layer. | M5 |
| `oci.LayerCache.EnableBlobStore`, `PushImage`, `StoreImage`, `FetchMissingBlobs` | `docker save`, `push`, `commit`, and `import` need the original layer blobs. | M5 |
| `/dev/pts/N` for the guest terminal | `tty` and `ttyname(3)` name the terminal of `run -it` and `exec -it`. | M3 |
| `hostfs` journal lock and merge | Containers that share a named volume keep the owner and mode records of each other. | M4 |
| `Config.ReadOnlyRoot`, `ShmSizeBytes`, `ExtraGroups`, `Ulimits` | `--read-only`, `--shm-size`, `--group-add`, and `--ulimit`. | M6 |
| `Config.UnixBridges` | A bind of the Docker socket gives a guest a socket whose connections the supervisor relays to the engine. | M6 |
| `ErrExecutableNotFound` | `docker exec` of a missing command exits with 127, which Testcontainers reads. | M6 |
| `Exec` waits for the guest | `docker exec` right after `docker run -d` finds the guest. | M6 |
| `oci.WithCredentials`, `VerifyLogin` | The engine uses the `X-Registry-Auth` credentials of each request and checks `docker login` without a store. | M6 |
| `Config.LoopbackToHost`, `NATConfig.LoopbackToHost` | A build reaches a registry on `localhost` of the Mac. Containers keep their own loopback. | M6 |

The supervisor writes the logs, so the logs need no pre-opened writer in the sandbox.

Each change gets its own pull request. A change in Tier 3 files obeys the one-behavior rule in `AGENTS.md`.

## 7. Known Gaps and Risks (Descriptive Module)

| Risk | Effect | Mitigation |
| :--- | :--- | :--- |
| Ports cannot change after lockdown | `docker run -p` cannot add a port to a running sandbox. | Read all ports at create time, as Docker does. |
| Builder aliases | `gvx-docker setup` writes the Buildx instances `gvx` and `default` as copies of `gvx-buildkit`. Buildx refuses those names in `buildx create`. | Serve `/grpc` with a proxy to BuildKit, so the builder of the engine works. |
| A copy into a stopped container | The copy boots a short sandbox, which takes about one second. | Mount the root upper image in the API process. |
| Windows host | The engine needs descriptor passing and Unix signals. | Port the supervisor protocol to named pipes. |
| Two processes per container | An idle container costs about 20 MB for its shim and 11 MB for its supervisor, about 31 MB in all, measured with `footprint` on ten `alpine` containers that sleep. `docker run --rm alpine true` takes 0.12 s, and `nginx:alpine` answers HTTP after 0.15 s. | Serve several containers from one supervisor. |
| `--network host` | A container cannot share the ports and loopback of the Mac, so the engine refuses the mode with an error. `-p` and `host.docker.internal` cover the usual needs. | None planned. |
| `--network container:` | Each container has its own network stack, so the engine refuses the mode with an error. | Run the processes of both containers in one sandbox. |
| Refused options | The engine refuses `--cap-add`, `--cap-drop`, `--privileged`, `--security-opt`, `--device`, `--gpus`, `--sysctl`, `--pids-limit`, and `--init` with an error, because the Sentry does not apply them. | Map an option to a `gvx.Config` field when a workload needs it. |
| Signal to a PID 1 without a handler | Linux ignores SIGTERM for a PID 1 without a handler, and Docker then kills it with code 137. The Sentry delivers SIGTERM, so `docker stop` ends such a container at once with code 143. | Apply the PID 1 rule of Linux in the Sentry. |
| Docker socket bind | The bridge relays bytes only, without peer credentials or descriptors. `/var/run/docker.sock` and `/run/docker.sock` always name this engine. | None needed for the Docker API. |
| Events come from polling | `docker events` sees a change after up to 100 ms. | Send events from the supervisor. |

## 8. Verification Procedure (Procedural Module)

1. Run the Go tests of each changed package with `scripts/codesign-exec.sh`.
2. Run the milestone acceptance commands from the host Docker CLI.
3. Restart `gvx-docker serve` and run the restart-safety commands again.
4. Record each failure, its cause, and its fix in the pull request.
