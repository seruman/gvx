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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"z.gfire.dev/gvx/pkg/gvx"
	"z.gfire.dev/gvx/pkg/oci"
)

const (
	apiVersion    = "1.47"
	minAPIVersion = "1.24"
	engineVersion = "0.1.0-gvx"
)

var versionPrefix = regexp.MustCompile(`^/v[0-9]+(?:\.[0-9]+)?(/.*)$`)

type imageStore interface {
	ListImages(arch string) ([]oci.Image, error)
	Select(query, arch string) (oci.Selection, error)
	LoadArchive(r io.Reader, arch string) ([]oci.LoadedImage, error)
	Pull(ctx context.Context, ref, arch string) (string, error)
}

type layerCacheStore struct{ cache *oci.LayerCache }

func (s layerCacheStore) ListImages(arch string) ([]oci.Image, error) {
	return s.cache.ListImages(arch)
}

func (s layerCacheStore) Select(query, arch string) (oci.Selection, error) {
	return s.cache.Select(query, arch)
}

func (s layerCacheStore) LoadArchive(r io.Reader, arch string) ([]oci.LoadedImage, error) {
	return oci.LoadArchive(r, s.cache, arch)
}

func (s layerCacheStore) Pull(ctx context.Context, ref, arch string) (string, error) {
	s.cache.EnableBlobStore()
	resolved, err := gvx.ResolveImageWithPolicy(ctx, s.cache, "", ref, arch, gvx.PullAlways, nil)
	if err != nil {
		return "", err
	}
	digest := resolved.ManifestDigest
	return digest, resolved.Close()
}

type builderStarter interface {
	ensure(context.Context) error
}

type server struct {
	images     imageStore
	cache      *oci.LayerCache
	builder    builderStarter
	containers *containerStore
	networks   *networkStore
	networkMu  sync.Mutex
	runtime    containerRuntime
	execs      execRegistry
	stateDir   string
	exe        string
	arch       string
	// stopGrace is the time that stop waits for the supervisor after the
	// timeout of the container, before it kills the supervisor.
	stopGrace time.Duration
}

func newServer(cache *oci.LayerCache, builder builderStarter, containers *containerStore, runtime containerRuntime, stateDir string) *server {
	return &server{images: layerCacheStore{cache: cache}, cache: cache, builder: builder, containers: containers, networks: newNetworkStore(stateDir), runtime: runtime, stateDir: stateDir, arch: goarch()}
}

func goarch() string { return runtime.GOARCH }

func serveUnix(ctx context.Context, path string, handler http.Handler) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", path, err)
	}
	defer func() { _ = os.Remove(path) }()
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("restrict %s: %w", path, err)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	err = httpServer.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return ctx.Err()
	}
	return err
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := normalizeAPIPath(r.URL.Path)
	w.Header().Set("Api-Version", apiVersion)
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("OSType", "linux")
	switch {
	case path == "/auth" && r.Method == http.MethodPost:
		s.checkAuth(w, r)
	case path == "/_ping" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.ping(w, r)
	case path == "/version" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.version())
	case path == "/info" && r.Method == http.MethodGet:
		s.info(w)
	case path == "/images/json" && r.Method == http.MethodGet:
		s.listImages(w)
	case path == "/images/create" && r.Method == http.MethodPost && r.URL.Query().Get("fromSrc") == "":
		s.pullImage(w, r)
	case path == "/images/load" && r.Method == http.MethodPost:
		s.loadImages(w, r)
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json") && r.Method == http.MethodGet:
		name, err := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid image name")
			return
		}
		s.inspectImage(w, name)
	case path == "/events" && r.Method == http.MethodGet && s.containers != nil:
		s.streamEvents(w, r)
	case path == "/system/df" && r.Method == http.MethodGet && s.containers != nil:
		s.systemDF(w)
	case path == "/images/create" && r.Method == http.MethodPost && r.URL.Query().Get("fromSrc") != "" && s.cache != nil:
		s.importImage(w, r)
	case s.containers != nil && s.routeImage(w, r, path):
	case s.containers != nil && s.routeVolume(w, r, path):
	case s.containers != nil && s.routeNetwork(w, r, path):
	case s.containers != nil && s.routeExec(w, r, path):
	case s.containers != nil && s.routeContainer(w, r, path):
	case path == "/build" || path == "/session" || path == "/grpc":
		writeError(w, http.StatusNotImplemented, "build through the gvx Buildx builder: docker buildx build --builder gvx-buildkit --load")
	default:
		writeError(w, http.StatusNotFound, "page not found: "+path)
	}
}

func normalizeAPIPath(path string) string {
	if m := versionPrefix.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return path
}

func (s *server) ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Builder-Version", "2")
	w.Header().Set("Swarm", "inactive")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "OK")
	}
}

func (s *server) version() map[string]any {
	return map[string]any{
		"Platform":      map[string]string{"Name": "gvx"},
		"Version":       engineVersion,
		"ApiVersion":    apiVersion,
		"MinAPIVersion": minAPIVersion,
		"Os":            "linux",
		"Arch":          s.arch,
		"KernelVersion": "gvx",
		"Experimental":  false,
		"Components": []map[string]any{{
			"Name":    "Engine",
			"Version": engineVersion,
			"Details": map[string]string{"ApiVersion": apiVersion, "MinAPIVersion": minAPIVersion, "Os": "linux", "Arch": s.arch},
		}},
	}
}

func (s *server) info(w http.ResponseWriter) {
	images, err := s.images.ListImages(s.arch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var total, running int
	if s.containers != nil {
		records, err := s.containers.list()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		total = len(records)
		for _, rec := range records {
			if rec.State.Running {
				running++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ID":                "GVX",
		"Containers":        total,
		"ContainersRunning": running,
		"ContainersPaused":  0,
		"ContainersStopped": total - running,
		"Images":            len(images),
		"Driver":            "gvx-erofs",
		"DockerRootDir":     "",
		"OSType":            "linux",
		"OperatingSystem":   "gvx",
		"Architecture":      dockerArchitecture(s.arch),
		"NCPU":              runtime.NumCPU(),
		"ServerVersion":     engineVersion,
		"DefaultRuntime":    "gvx",
		"Name":              "gvx",
	})
}

func dockerArchitecture(goarch string) string {
	if goarch == "arm64" {
		return "aarch64"
	}
	if goarch == "amd64" {
		return "x86_64"
	}
	return goarch
}

func (s *server) listImages(w http.ResponseWriter) {
	images, err := s.images.ListImages(s.arch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := map[string]map[string]any{}
	var order []string
	for _, img := range images {
		item, ok := byID[img.ID]
		if !ok {
			item = map[string]any{
				"Id":          img.ID,
				"ParentId":    "",
				"RepoTags":    []string{},
				"RepoDigests": []string{},
				"Created":     img.Created.Unix(),
				"Size":        img.Size,
				"SharedSize":  -1,
				"VirtualSize": img.Size,
				"Labels":      map[string]string{},
				"Containers":  -1,
			}
			byID[img.ID] = item
			order = append(order, img.ID)
		}
		item["RepoTags"] = append(item["RepoTags"].([]string), img.RepoTags()...)
		item["RepoDigests"] = append(item["RepoDigests"].([]string), img.RepoDigests()...)
	}
	out := make([]map[string]any, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) inspectImage(w http.ResponseWriter, name string) {
	sel, err := s.images.Select(name, s.arch)
	if err != nil || len(sel.Images) == 0 {
		writeError(w, http.StatusNotFound, "No such image: "+name)
		return
	}
	img := sel.Images[0]
	var repoTags, repoDigests []string
	for _, candidate := range sel.Images {
		repoTags = append(repoTags, candidate.RepoTags()...)
		repoDigests = append(repoDigests, candidate.RepoDigests()...)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":           img.ID,
		"RepoTags":     repoTags,
		"RepoDigests":  repoDigests,
		"Created":      img.Created.Format(time.RFC3339Nano),
		"Architecture": img.Architecture,
		"Os":           img.OS,
		"Size":         img.Size,
		"Config": map[string]any{
			"Env":          img.Config.Env,
			"Cmd":          img.Config.Cmd,
			"Entrypoint":   img.Config.Entrypoint,
			"WorkingDir":   img.Config.WorkingDir,
			"User":         img.Config.User,
			"ExposedPorts": img.Config.ExposedPorts,
		},
		"RootFS": map[string]any{"Type": "layers", "Layers": img.DiffIDs},
	})
}

func (s *server) pullImage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ref := q.Get("fromImage")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "fromImage is required")
		return
	}
	if tag := q.Get("tag"); tag != "" {
		if strings.HasPrefix(tag, "sha256:") {
			ref += "@" + tag
		} else {
			ref += ":" + tag
		}
	}
	if platform := q.Get("platform"); platform != "" && platform != "linux/"+s.arch {
		writeError(w, http.StatusBadRequest, "gvx runs only linux/"+s.arch+" images")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	_ = enc.Encode(map[string]string{"status": "Pulling from " + ref})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	ctx := r.Context()
	if parsed, err := oci.ParseReference(ref); err == nil {
		ctx = withRegistryAuth(ctx, r, parsed)
	}
	digest, err := s.images.Pull(ctx, ref, s.arch)
	if err != nil {
		_ = enc.Encode(map[string]any{"errorDetail": map[string]string{"message": err.Error()}, "error": err.Error()})
		return
	}
	_ = enc.Encode(map[string]string{"status": "Digest: " + digest})
	_ = enc.Encode(map[string]string{"status": "Status: Image is up to date for " + ref})
}

func (s *server) loadImages(w http.ResponseWriter, r *http.Request) {
	loaded, err := s.images.LoadArchive(r.Body, s.arch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, img := range loaded {
		if len(img.Names) == 0 {
			_ = enc.Encode(map[string]string{"stream": "Loaded image ID: " + img.ID + "\n"})
			continue
		}
		for _, name := range img.Names {
			_ = enc.Encode(map[string]string{"stream": "Loaded image: " + name.Familiar() + "\n"})
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// registryAuth is the JSON form of the X-Registry-Auth header and of the body
// of POST /auth.
type registryAuth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
	IdentityToken string `json:"identitytoken"`
}

func (a registryAuth) credentials() oci.Credentials {
	return oci.Credentials{Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken}
}

// withRegistryAuth returns a context whose registry requests for ref use the
// X-Registry-Auth credentials of the request. Docker clients read the
// credentials from their own config and helpers and send them with each pull
// and push, so the engine needs no access to them.
func withRegistryAuth(ctx context.Context, r *http.Request, ref oci.Reference) context.Context {
	raw := r.Header.Get("X-Registry-Auth")
	if raw == "" {
		return ctx
	}
	data, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		data, err = base64.RawURLEncoding.DecodeString(raw)
	}
	if err != nil {
		return ctx
	}
	var auth registryAuth
	if json.Unmarshal(data, &auth) != nil {
		return ctx
	}
	cred := auth.credentials()
	if cred == (oci.Credentials{}) {
		return ctx
	}
	// The credentials belong to the registry of the image. A client sends
	// the Docker Hub index address for Docker Hub images.
	authed, err := oci.WithCredentials(ctx, ref.BaseURL(), cred)
	if err != nil {
		return ctx
	}
	return authed
}

// checkAuth serves docker login: it proves the credentials to the registry
// and keeps nothing. The client stores them.
func (s *server) checkAuth(w http.ResponseWriter, r *http.Request) {
	var auth registryAuth
	if err := json.NewDecoder(r.Body).Decode(&auth); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	server := auth.ServerAddress
	if server == "" || strings.Contains(server, "index.docker.io") {
		server = oci.DockerHubRegistry
	}
	if err := oci.VerifyLogin(r.Context(), nil, server, auth.credentials()); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, oci.ErrLoginRejected) {
			status = http.StatusUnauthorized
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"Status": "Login Succeeded"})
}
