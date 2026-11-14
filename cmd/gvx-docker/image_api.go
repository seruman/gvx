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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"z.gfire.dev/gvx/pkg/oci"
)

// routeImage serves the image endpoints beyond list, inspect, pull, and load.
func (s *server) routeImage(w http.ResponseWriter, r *http.Request, path string) bool {
	if s.cache == nil {
		return false
	}
	switch {
	case path == "/images/prune" && r.Method == http.MethodPost:
		s.pruneImages(w, r)
	case path == "/images/get" && r.Method == http.MethodGet:
		s.saveImages(w, r, r.URL.Query()["names"])
	case path == "/commit" && r.Method == http.MethodPost:
		s.commitContainer(w, r)
	case strings.HasPrefix(path, "/images/"):
		rest := strings.TrimPrefix(path, "/images/")
		name, action := rest, ""
		for _, a := range []string{"/tag", "/push", "/history", "/get"} {
			if strings.HasSuffix(rest, a) {
				name, action = strings.TrimSuffix(rest, a), a
				break
			}
		}
		name, err := url.PathUnescape(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid image name")
			return true
		}
		switch {
		case action == "" && r.Method == http.MethodDelete:
			s.removeImage(w, r, name)
		case action == "/tag" && r.Method == http.MethodPost:
			s.tagImage(w, r, name)
		case action == "/push" && r.Method == http.MethodPost:
			s.pushImage(w, r, name)
		case action == "/history" && r.Method == http.MethodGet:
			s.imageHistory(w, name)
		case action == "/get" && r.Method == http.MethodGet:
			s.saveImages(w, r, []string{name})
		default:
			return false
		}
	default:
		return false
	}
	return true
}

func (s *server) selectImage(name string) (oci.Selection, error) {
	sel, err := s.cache.Select(name, s.arch)
	if err != nil || len(sel.Images) == 0 {
		return oci.Selection{}, fmt.Errorf("%w: No such image: %s", errNotFound, name)
	}
	return sel, nil
}

func (s *server) imageInUse(id string) []string {
	records, _ := s.containers.list()
	var users []string
	for _, c := range records {
		if c.ImageID == id {
			users = append(users, c.ID[:12])
		}
	}
	return users
}

func (s *server) removeImage(w http.ResponseWriter, r *http.Request, name string) {
	sel, err := s.selectImage(name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	img := sel.Images[0]
	var out []map[string]string
	if sel.Ref != nil && len(img.Tags) > 0 && !(len(img.Tags) == 1 && !force && len(sel.Images) == 1 && false) {
		tag := sel.Ref.Tag
		if sel.Ref.Digest != "" {
			tag = sel.Ref.Digest
		}
		// A name removes that name. The image goes when nothing else names it.
		all, _ := s.cache.ListImages(s.arch)
		names := 0
		for _, other := range all {
			if other.ID == img.ID {
				names += len(other.Tags)
			}
		}
		if names <= 1 && !force {
			if users := s.imageInUse(img.ID); len(users) > 0 {
				writeError(w, http.StatusConflict, fmt.Sprintf("conflict: unable to remove repository reference %q (must force) - container %s is using its referenced image %s", name, users[0], shortID(img.ID)))
				return
			}
		}
		report, err := s.cache.Untag(img, tag)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = appendRemoveReport(out, report)
		writeJSON(w, http.StatusOK, out)
		return
	}
	if users := s.imageInUse(img.ID); len(users) > 0 && !force {
		writeError(w, http.StatusConflict, fmt.Sprintf("conflict: unable to delete %s (cannot be forced) - image is being used by running container %s", shortID(img.ID), users[0]))
		return
	}
	if len(sel.Images) > 1 && !force {
		writeError(w, http.StatusConflict, fmt.Sprintf("conflict: unable to delete %s (must be forced) - image is referenced in multiple repositories", shortID(img.ID)))
		return
	}
	for _, each := range sel.Images {
		report, err := s.cache.Remove(each)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = appendRemoveReport(out, report)
	}
	writeJSON(w, http.StatusOK, out)
}

func appendRemoveReport(out []map[string]string, report oci.RemoveReport) []map[string]string {
	for _, u := range report.Untagged {
		out = append(out, map[string]string{"Untagged": u})
	}
	for _, d := range report.Deleted {
		out = append(out, map[string]string{"Deleted": d})
	}
	return out
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (s *server) tagImage(w http.ResponseWriter, r *http.Request, name string) {
	sel, err := s.selectImage(name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	repo := r.URL.Query().Get("repo")
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		tag = "latest"
	}
	ref, err := oci.ParseReference(repo + ":" + tag)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.cache.Tag(sel.Images[0], ref); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *server) pushImage(w http.ResponseWriter, r *http.Request, name string) {
	tag := r.URL.Query().Get("tag")
	full := name
	if tag != "" {
		full = name + ":" + tag
	}
	ref, err := oci.ParseReference(full)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sel, err := s.selectImage(full)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	_ = enc.Encode(map[string]string{"status": "The push refers to repository [" + ref.Name() + "]"})
	flush()
	dgst, err := s.cache.PushImage(withRegistryAuth(r.Context(), r, ref), sel.Images[0], ref, func(id, status string) {
		_ = enc.Encode(map[string]string{"id": id, "status": status})
		flush()
	})
	if err != nil {
		_ = enc.Encode(map[string]any{"error": err.Error(), "errorDetail": map[string]string{"message": err.Error()}})
		return
	}
	content, _ := s.cache.ImageContent(sel.Images[0])
	_ = enc.Encode(map[string]any{"status": fmt.Sprintf("%s: digest: %s size: %d", ref.Tag, dgst, len(content.Manifest))})
}

func (s *server) imageHistory(w http.ResponseWriter, name string) {
	sel, err := s.selectImage(name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	img := sel.Images[0]
	content, err := s.cache.ImageContent(img)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var cfg ocispec.Image
	_ = json.Unmarshal(content.Config, &cfg)
	out := []map[string]any{}
	layer := len(content.Layers) - 1
	for i := len(cfg.History) - 1; i >= 0; i-- {
		h := cfg.History[i]
		var size int64
		id := "<missing>"
		if !h.EmptyLayer && layer >= 0 {
			size = content.Layers[layer].Size
			layer--
		}
		if i == len(cfg.History)-1 {
			id = img.ID
		}
		var created int64
		if h.Created != nil {
			created = h.Created.Unix()
		}
		out = append(out, map[string]any{"Id": id, "Created": created, "CreatedBy": h.CreatedBy, "Tags": nil, "Size": size, "Comment": h.Comment})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) pruneImages(w http.ResponseWriter, r *http.Request) {
	filters, _ := parseFilters(r.URL.Query().Get("filters"))
	all := false
	for _, v := range filters["dangling"] {
		if v == "false" || v == "0" {
			all = true
		}
	}
	used := map[string]bool{}
	records, _ := s.containers.list()
	for _, c := range records {
		used[c.ImageID] = true
	}
	images, err := s.cache.ListImages(s.arch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out []map[string]string
	var reclaimed int64
	for _, img := range images {
		if used[img.ID] || (!all && img.Referenced()) {
			continue
		}
		report, err := s.cache.Remove(img)
		if err != nil {
			continue
		}
		reclaimed += img.Size
		out = appendRemoveReport(out, report)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ImagesDeleted": out, "SpaceReclaimed": reclaimed})
}

// saveImages writes a docker save archive of the images. A layer comes from
// the blob store, or from the registry of the image when the store lacks it.
func (s *server) saveImages(w http.ResponseWriter, r *http.Request, names []string) {
	var targets []oci.SaveTarget
	var local []oci.Image
	var localNames [][]oci.Reference
	for _, name := range names {
		sel, err := s.selectImage(name)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		var refs []oci.Reference
		if sel.Ref != nil && sel.Ref.Tag != "" {
			refs = append(refs, *sel.Ref)
		}
		img := sel.Images[0]
		_ = s.cache.FetchMissingBlobs(r.Context(), img)
		content, err := s.cache.ImageContent(img)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if content.BlobsComplete {
			local = append(local, img)
			localNames = append(localNames, refs)
			continue
		}
		targets = append(targets, oci.SaveTarget{Image: img, Names: refs})
	}
	if len(local) > 0 && len(targets) > 0 {
		writeError(w, http.StatusNotImplemented, "save images from the local store and from registries in separate commands")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if len(targets) > 0 {
		if err := s.cache.SaveImages(r.Context(), w, nil, targets); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if err := s.writeLocalArchive(w, local, localNames); err != nil {
		panic(http.ErrAbortHandler)
	}
}

type dockerSaveManifest struct {
	Config   string
	RepoTags []string
	Layers   []string
}

// writeLocalArchive writes an OCI layout with a docker manifest.json from the
// blob store.
func (s *server) writeLocalArchive(w io.Writer, images []oci.Image, names [][]oci.Reference) error {
	tw := tar.NewWriter(w)
	written := map[string]bool{}
	writeFile := func(name string, data []byte) error {
		if written[name] {
			return nil
		}
		written[name] = true
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := writeFile("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	index := ocispec.Index{MediaType: ocispec.MediaTypeImageIndex}
	index.SchemaVersion = 2
	var manifests []dockerSaveManifest
	for i, img := range images {
		content, err := s.cache.ImageContent(img)
		if err != nil {
			return err
		}
		cfgName := "blobs/sha256/" + strings.TrimPrefix(content.ConfigDigest, "sha256:")
		if err := writeFile(cfgName, content.Config); err != nil {
			return err
		}
		entry := dockerSaveManifest{Config: cfgName}
		for _, l := range content.Layers {
			name := "blobs/sha256/" + l.Digest.Encoded()
			entry.Layers = append(entry.Layers, name)
			if written[name] {
				continue
			}
			f, size, err := s.cache.OpenBlob(l.Digest.Encoded())
			if err != nil {
				return err
			}
			written[name] = true
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
				_ = f.Close()
				return err
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			if err != nil {
				return err
			}
		}
		mdigest := digest.FromBytes(content.Manifest)
		if err := writeFile("blobs/sha256/"+mdigest.Encoded(), content.Manifest); err != nil {
			return err
		}
		desc := ocispec.Descriptor{MediaType: content.ManifestType, Digest: mdigest, Size: int64(len(content.Manifest))}
		for _, n := range names[i] {
			entry.RepoTags = append(entry.RepoTags, n.Familiar())
			d := desc
			d.Annotations = map[string]string{"io.containerd.image.name": n.String(), ocispec.AnnotationRefName: n.Tag}
			index.Manifests = append(index.Manifests, d)
		}
		if len(names[i]) == 0 {
			index.Manifests = append(index.Manifests, desc)
		}
		manifests = append(manifests, entry)
	}
	indexJSON, _ := json.Marshal(index)
	if err := writeFile("index.json", indexJSON); err != nil {
		return err
	}
	manifestJSON, _ := json.Marshal(manifests)
	if err := writeFile("manifest.json", manifestJSON); err != nil {
		return err
	}
	return tw.Close()
}

// commitContainer makes an image of the changes of a container. The changes
// are the files of the root upper image, which a short sandbox archives.
func (s *server) commitContainer(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rec, err := s.containers.get(q.Get("container"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	sel, err := s.cache.Select(rec.ImageID, s.arch)
	if err != nil || len(sel.Images) == 0 {
		writeError(w, http.StatusNotFound, "the image of the container is gone")
		return
	}
	base := sel.Images[0]
	// The new image has no registry source, so its base layers must be in
	// the blob store for a later save or push.
	if err := s.cache.FetchMissingBlobs(r.Context(), base); err != nil {
		writeError(w, http.StatusInternalServerError, "fetch the layers of the base image: "+err.Error())
		return
	}
	baseContent, err := s.cache.ImageContent(base)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var overrides containerConfig
	if r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&overrides)
	}
	layer, diffID, err := s.containerDiff(r.Context(), rec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hexDigest, size, err := s.cache.StoreBlob(bytes.NewReader(layer))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var cfg map[string]any
	if err := json.Unmarshal(baseContent.Config, &cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().UTC()
	cfg["created"] = now.Format(time.RFC3339Nano)
	runtimeCfg, _ := cfg["config"].(map[string]any)
	if runtimeCfg == nil {
		runtimeCfg = map[string]any{}
	}
	if overrides.Cmd != nil {
		runtimeCfg["Cmd"] = overrides.Cmd
	}
	if overrides.Entrypoint != nil {
		runtimeCfg["Entrypoint"] = overrides.Entrypoint
	}
	if overrides.Env != nil {
		runtimeCfg["Env"] = overrides.Env
	}
	if overrides.WorkingDir != "" {
		runtimeCfg["WorkingDir"] = overrides.WorkingDir
	}
	if overrides.User != "" {
		runtimeCfg["User"] = overrides.User
	}
	if overrides.ExposedPorts != nil {
		runtimeCfg["ExposedPorts"] = overrides.ExposedPorts
	}
	if overrides.Labels != nil {
		runtimeCfg["Labels"] = overrides.Labels
	}
	for _, change := range q["changes"] {
		applyChange(runtimeCfg, change)
	}
	cfg["config"] = runtimeCfg
	rootfs, _ := cfg["rootfs"].(map[string]any)
	diffIDs, _ := rootfs["diff_ids"].([]any)
	rootfs["diff_ids"] = append(diffIDs, diffID)
	cfg["rootfs"] = rootfs
	history, _ := cfg["history"].([]any)
	cfg["history"] = append(history, map[string]any{"created": now.Format(time.RFC3339Nano), "created_by": "docker commit", "comment": q.Get("comment")})
	config, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	names, err := targetNames(q.Get("repo"), q.Get("tag"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	img, err := s.cache.CommitLayer(base, hexDigest, size, config, names)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"Id": img.ID})
}

// containerDiff returns the changes of rec as a gzip layer and the digest of
// the uncompressed tar. A shim reads the upper image of the root.
func (s *server) containerDiff(ctx context.Context, rec containerRecord) ([]byte, string, error) {
	sock, release, err := s.archiveControl(ctx, rec.ID)
	if err != nil {
		return nil, "", err
	}
	defer release()
	conn, reader, err := controlCall(ctx, sock, controlRequest{Op: "diff"})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = conn.Close() }()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(gz, h), reader); err != nil {
		return nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, "", err
	}
	return compressed.Bytes(), fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

// exportContainer writes the whole file system of a container as a tar.
func (s *server) exportContainer(w http.ResponseWriter, r *http.Request, ref string) {
	sock, release, err := s.archiveControl(r.Context(), ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer release()
	conn, reader, err := controlCall(r.Context(), sock, controlRequest{Op: "archive-get", Path: "/."})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

// importImage makes a one-layer image from a tar, as docker import does.
func (s *server) importImage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var src io.Reader = r.Body
	if from := q.Get("fromSrc"); from != "" && from != "-" {
		resp, err := http.Get(from)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		defer func() { _ = resp.Body.Close() }()
		src = resp.Body
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(gz, h), decompressMaybe(src)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = gz.Close()
	hexDigest, size, err := s.cache.StoreBlob(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().UTC()
	runtimeCfg := map[string]any{}
	for _, change := range q["changes"] {
		applyChange(runtimeCfg, change)
	}
	cfg := map[string]any{
		"architecture": s.arch, "os": "linux", "created": now.Format(time.RFC3339Nano),
		"config":  runtimeCfg,
		"rootfs":  map[string]any{"type": "layers", "diff_ids": []string{fmt.Sprintf("sha256:%x", h.Sum(nil))}},
		"history": []map[string]any{{"created": now.Format(time.RFC3339Nano), "created_by": "Imported from " + q.Get("fromSrc"), "comment": q.Get("message")}},
	}
	config, _ := json.Marshal(cfg)
	names, err := targetNames(q.Get("repo"), q.Get("tag"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	layers := []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.NewDigestFromEncoded(digest.SHA256, hexDigest), Size: size}}
	img, err := s.cache.StoreImage(config, layers, names)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": img.ID})
}

// targetNames parses the name of a new image. repo can hold the tag, as
// docker import sends it.
func targetNames(repo, tag string) ([]oci.Reference, error) {
	if repo == "" {
		return nil, nil
	}
	full := repo
	if tag != "" {
		full = repo + ":" + tag
	}
	ref, err := oci.ParseReference(full)
	if err != nil {
		return nil, err
	}
	return []oci.Reference{ref}, nil
}

// decompressMaybe returns a reader of the tar in r, which may be gzip
// compressed.
func decompressMaybe(r io.Reader) io.Reader {
	buf := make([]byte, 2)
	n, _ := io.ReadFull(r, buf)
	full := io.MultiReader(bytes.NewReader(buf[:n]), r)
	if n == 2 && buf[0] == 0x1f && buf[1] == 0x8b {
		if gz, err := gzip.NewReader(full); err == nil {
			return gz
		}
	}
	return full
}

// applyChange applies one Dockerfile instruction of docker import --change.
func applyChange(cfg map[string]any, change string) {
	instr, arg, _ := strings.Cut(strings.TrimSpace(change), " ")
	arg = strings.TrimSpace(arg)
	parseList := func() []string {
		var list []string
		if json.Unmarshal([]byte(arg), &list) == nil {
			return list
		}
		return []string{"/bin/sh", "-c", arg}
	}
	switch strings.ToUpper(instr) {
	case "CMD":
		cfg["Cmd"] = parseList()
	case "ENTRYPOINT":
		cfg["Entrypoint"] = parseList()
	case "WORKDIR":
		cfg["WorkingDir"] = arg
	case "USER":
		cfg["User"] = arg
	case "ENV":
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			k, v, _ = strings.Cut(arg, " ")
		}
		env, _ := cfg["Env"].([]string)
		cfg["Env"] = append(env, strings.TrimSpace(k)+"="+strings.TrimSpace(v))
	case "EXPOSE":
		ports, _ := cfg["ExposedPorts"].(map[string]any)
		if ports == nil {
			ports = map[string]any{}
		}
		for _, p := range strings.Fields(arg) {
			if !strings.Contains(p, "/") {
				p += "/tcp"
			}
			ports[p] = map[string]any{}
		}
		cfg["ExposedPorts"] = ports
	}
}

var _ = errors.New
var _ = os.ErrNotExist
