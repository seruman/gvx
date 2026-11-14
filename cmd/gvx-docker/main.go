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

// Binary gvx-docker serves a Docker Engine API backed by gvx.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"z.gfire.dev/gvx/pkg/gvx"
)

const (
	defaultBuildKitImage = "moby/buildkit:buildx-stable-1"
	builderName          = "gvx-buildkit"
	contextName          = "gvx"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "serve":
		return serveCommand(ctx, args[1:], stderr)
	case "builder":
		return builderCommand(ctx, args[1:], stderr)
	case "supervise":
		return superviseCommand(ctx, args[1:], stderr)
	case "shim":
		return shimCommand(ctx, args[1:], stderr)
	case "setup":
		return setupCommand(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "gvx-docker: unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage:")
	_, _ = fmt.Fprintln(w, "  gvx-docker serve [-socket PATH] [-state-dir DIR]")
	_, _ = fmt.Fprintln(w, "  gvx-docker builder [-state-dir DIR]")
	_, _ = fmt.Fprintln(w, "  gvx-docker setup [-socket PATH] [-state-dir DIR]")
}

type paths struct {
	stateDir      string
	apiSocket     string
	buildkitSock  string
	backendSock   string
	buildkitImage string
}

func defaultPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, fmt.Errorf("find home directory: %w", err)
	}
	stateDir := filepath.Join(home, ".gvx", "docker")
	return paths{
		stateDir:      stateDir,
		apiSocket:     filepath.Join(stateDir, "docker.sock"),
		buildkitImage: defaultBuildKitImage,
	}, nil
}

func addPathFlags(fs *flag.FlagSet, p *paths) {
	fs.StringVar(&p.stateDir, "state-dir", p.stateDir, "State directory")
	fs.StringVar(&p.apiSocket, "socket", p.apiSocket, "Docker API Unix socket")
	fs.StringVar(&p.buildkitImage, "buildkit-image", p.buildkitImage, "BuildKit image")
}

func (p *paths) normalize() {
	p.stateDir = filepath.Clean(p.stateDir)
	p.buildkitSock = filepath.Join(p.stateDir, "run", "buildkit.sock")
	p.backendSock = filepath.Join(p.stateDir, "run", "buildkitd.sock")
}

func serveCommand(ctx context.Context, args []string, stderr io.Writer) int {
	p, err := defaultPaths()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker serve: %v\n", err)
		return 1
	}
	fs := flag.NewFlagSet("gvx-docker serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addPathFlags(fs, &p)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p.normalize()
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "gvx-docker serve: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	// One serve owns a state directory. A second one would remove the
	// sockets of the first one and start a second builder on the same
	// volume image.
	stateLock, err := lockStateDir(p.stateDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker serve: %v\n", err)
		return 1
	}
	defer stateLock.release()
	exe, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker serve: find executable: %v\n", err)
		return 1
	}
	cache, err := gvx.NewLayerCache("")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker serve: open image cache: %v\n", err)
		return 1
	}
	// Loads and pulls keep the original layer blobs, so save, push, commit,
	// and export work for every image that the engine stored.
	cache.EnableBlobStore()
	builder := &builderProcess{exe: exe, paths: p, log: stderr}
	defer builder.stop()
	containers := newContainerStore(p.stateDir)
	server := newServer(cache, builder, containers, processRuntime{exe: exe, paths: p, store: containers}, p.stateDir)
	server.exe = exe
	errs := make(chan error, 2)
	if err := publishEngineSocket(p); err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker serve: %v\n", err)
		return 1
	}
	go func() { errs <- serveUnix(ctx, p.apiSocket, server) }()
	go func() { errs <- serveBuildKitProxy(ctx, p.buildkitSock, p.backendSock, builder) }()
	if err := <-errs; err != nil && !errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintf(stderr, "gvx-docker serve: %v\n", err)
		return 1
	}
	return 0
}

func builderCommand(ctx context.Context, args []string, stderr io.Writer) int {
	p, err := defaultPaths()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker builder: %v\n", err)
		return 1
	}
	fs := flag.NewFlagSet("gvx-docker builder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addPathFlags(fs, &p)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p.normalize()
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "gvx-docker builder: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	code, err := runBuildKit(ctx, p, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker builder: %v\n", err)
		return 1
	}
	return code
}

func setupCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	p, err := defaultPaths()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker setup: %v\n", err)
		return 1
	}
	fs := flag.NewFlagSet("gvx-docker setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addPathFlags(fs, &p)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p.normalize()
	if err := setupDocker(ctx, p, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "gvx-docker setup: %v\n", err)
		return 1
	}
	return 0
}

func setupDocker(ctx context.Context, p paths, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	contextHost := "host=unix://" + p.apiSocket
	if err := runDocker(ctx, stdout, stderr, "context", "inspect", contextName); err != nil {
		if err := runDocker(ctx, stdout, stderr, "context", "create", contextName, "--docker", contextHost); err != nil {
			return err
		}
	} else if err := runDocker(ctx, stdout, stderr, "context", "update", contextName, "--docker", contextHost); err != nil {
		return err
	}
	builderEndpoint := "unix://" + p.buildkitSock
	_ = runDocker(ctx, io.Discard, io.Discard, "buildx", "rm", builderName)
	if err := runDocker(ctx, stdout, stderr, "buildx", "create", "--name", builderName, "--driver", "remote", "--driver-opt", "default-load=true", builderEndpoint); err != nil {
		return err
	}
	if err := runDocker(ctx, stdout, stderr, "--context", contextName, "buildx", "use", "--default", builderName); err != nil {
		return err
	}
	// docker build uses the builder that has the name of the context, and
	// docker compose build falls back to the builder "default". Buildx
	// refuses to create builders with those names, so setup writes their
	// instance files as copies of the gvx builder. Without them, both start a
	// BuildKit container in the engine.
	for _, name := range []string{contextName, "default"} {
		if err := writeBuilderAlias(name); err != nil {
			return err
		}
	}
	return nil
}

// writeBuilderAlias copies the instance file of builderName to an instance
// file with the given name.
func writeBuilderAlias(alias string) error {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".docker")
	}
	instances := filepath.Join(dir, "buildx", "instances")
	data, err := os.ReadFile(filepath.Join(instances, builderName))
	if err != nil {
		return fmt.Errorf("read the buildx instance %s: %w", builderName, err)
	}
	var inst map[string]any
	if err := json.Unmarshal(data, &inst); err != nil {
		return fmt.Errorf("decode the buildx instance %s: %w", builderName, err)
	}
	inst["Name"] = alias
	if nodes, ok := inst["Nodes"].([]any); ok {
		for i, n := range nodes {
			if node, ok := n.(map[string]any); ok {
				node["Name"] = fmt.Sprintf("%s%d", alias, i)
			}
		}
	}
	out, err := json.Marshal(inst)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(instances, alias), out, 0o600)
}

func runDocker(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// engineSocketLink returns the link that names the API socket of the engine.
// A bind of /var/run/docker.sock reaches that socket.
func engineSocketLink(stateDir string) string {
	return filepath.Join(stateDir, "run", "engine.sock")
}

func publishEngineSocket(p paths) error {
	link := engineSocketLink(p.stateDir)
	abs, err := filepath.Abs(p.apiSocket)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		return err
	}
	_ = os.Remove(link)
	if err := os.Symlink(abs, link); err != nil {
		return fmt.Errorf("publish the engine socket: %w", err)
	}
	return nil
}
