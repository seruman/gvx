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

package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageBlobs is the content of one cached image: the verbatim manifest and
// config, and the layer descriptors of the manifest.
type ImageBlobs struct {
	Manifest      []byte
	ManifestType  string
	Config        []byte
	ConfigDigest  string
	Layers        []ocispec.Descriptor
	BlobsComplete bool
}

// ImageContent returns the verified manifest, config, and layers of img.
// BlobsComplete reports whether the blob store holds every layer.
func (c *LayerCache) ImageContent(img Image) (ImageBlobs, error) {
	dec, err := c.verifyRecord(img.record)
	if err != nil {
		return ImageBlobs{}, err
	}
	out := ImageBlobs{
		Manifest:      dec.manifestBody,
		ManifestType:  dec.manifest.MediaType,
		Config:        dec.config,
		ConfigDigest:  dec.manifest.Config.Digest.String(),
		Layers:        dec.manifest.Layers,
		BlobsComplete: true,
	}
	if out.ManifestType == "" {
		out.ManifestType = ocispec.MediaTypeImageManifest
	}
	for _, l := range dec.manifest.Layers {
		if _, ok := c.BlobPath(l.Digest.Encoded()); !ok {
			out.BlobsComplete = false
		}
	}
	return out, nil
}

// PushProgress receives one line for each step of a push.
type PushProgress func(id, status string)

// PushImage uploads img to the registry of ref and tags it as ref. The blob
// store must hold every layer, or the image must have a registry source that
// still serves the layers: a blob that the target registry has already
// counts. Credentials come from the docker config.
func (c *LayerCache) PushImage(ctx context.Context, img Image, ref Reference, progress PushProgress) (string, error) {
	if err := c.FetchMissingBlobs(ctx, img); err != nil && !errors.Is(err, ErrNoRegistrySource) {
		return "", err
	}
	content, err := c.ImageContent(img)
	if err != nil {
		return "", err
	}
	rc, err := newRegistryClient(nil, ref.BaseURL(), ref.Repository, c.limits, c.dnsServers)
	if err != nil {
		return "", err
	}
	defer rc.close()
	if progress == nil {
		progress = func(string, string) {}
	}
	for _, l := range content.Layers {
		id := l.Digest.Encoded()[:12]
		exists, err := rc.blobExists(ctx, l.Digest)
		if err != nil {
			return "", err
		}
		if exists {
			progress(id, "Layer already exists")
			continue
		}
		f, size, err := c.OpenBlob(l.Digest.Encoded())
		if err != nil {
			return "", fmt.Errorf("oci: push %s: the cache lacks layer %s; pull or build the image again with the blob store on: %w", ref.Familiar(), l.Digest, err)
		}
		progress(id, "Pushing")
		err = rc.uploadBlob(ctx, l.Digest, f, size)
		_ = f.Close()
		if err != nil {
			return "", err
		}
		progress(id, "Pushed")
	}
	cfgDigest := digest.Digest(content.ConfigDigest)
	if exists, err := rc.blobExists(ctx, cfgDigest); err != nil {
		return "", err
	} else if !exists {
		if err := rc.uploadBlob(ctx, cfgDigest, bytes.NewReader(content.Config), int64(len(content.Config))); err != nil {
			return "", err
		}
	}
	tag := ref.Tag
	if tag == "" {
		tag = "latest"
	}
	if err := rc.putManifest(ctx, tag, content.ManifestType, content.Manifest); err != nil {
		return "", err
	}
	return digest.FromBytes(content.Manifest).String(), nil
}

// send sends a request with a body and answers one authentication challenge.
// body returns a fresh reader for each attempt.
func (rc *registryClient) send(ctx context.Context, method, rawURL, contentType string, size int64, body func() (io.Reader, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var r io.Reader = http.NoBody
		if body != nil {
			b, err := body()
			if err != nil {
				return nil, err
			}
			r = b
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, r)
		if err != nil {
			return nil, fmt.Errorf("oci: build request %s: %w", rawURL, err)
		}
		if body != nil {
			req.ContentLength = size
		}
		req.Header.Set("User-Agent", userAgent)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if rc.authz != "" {
			req.Header.Set("Authorization", rc.authz)
		}
		resp, err := rc.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("oci: request %s: %w", rawURL, err)
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		headers := resp.Header.Values("WWW-Authenticate")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if err := rc.authorize(ctx, headers); err != nil {
			return nil, err
		}
	}
}

func (rc *registryClient) repoURL(suffix string) string {
	u := *rc.baseURL
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v2/" + rc.repo + suffix
	return u.String()
}

func (rc *registryClient) blobExists(ctx context.Context, d digest.Digest) (bool, error) {
	resp, err := rc.send(ctx, http.MethodHead, rc.repoURL("/blobs/"+d.String()), "", 0, nil)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, &StatusError{URL: rc.repoURL("/blobs/" + d.String()), StatusCode: resp.StatusCode}
	}
}

// uploadBlob uploads r, which has size bytes and the digest d, with a
// monolithic PUT after a POST that opens the upload session.
func (rc *registryClient) uploadBlob(ctx context.Context, d digest.Digest, r io.ReadSeeker, size int64) error {
	resp, err := rc.send(ctx, http.MethodPost, rc.repoURL("/blobs/uploads/"), "", 0, nil)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return &StatusError{URL: rc.repoURL("/blobs/uploads/"), StatusCode: resp.StatusCode}
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.Header.Get("Location") == "" {
		return fmt.Errorf("oci: upload of %s has no valid Location", d)
	}
	target := rc.baseURL.ResolveReference(loc)
	q := target.Query()
	q.Set("digest", d.String())
	target.RawQuery = q.Encode()
	put, err := rc.send(ctx, http.MethodPut, target.String(), "application/octet-stream", size, func() (io.Reader, error) {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return r, nil
	})
	if err != nil {
		return err
	}
	_ = put.Body.Close()
	if put.StatusCode != http.StatusCreated && put.StatusCode != http.StatusAccepted {
		return &StatusError{URL: target.Redacted(), StatusCode: put.StatusCode}
	}
	return nil
}

func (rc *registryClient) putManifest(ctx context.Context, tag, mediaType string, body []byte) error {
	resp, err := rc.send(ctx, http.MethodPut, rc.repoURL("/manifests/"+tag), mediaType, int64(len(body)), func() (io.Reader, error) {
		return bytes.NewReader(body), nil
	})
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return &StatusError{URL: rc.repoURL("/manifests/" + tag), StatusCode: resp.StatusCode}
	}
	return nil
}

// CommitLayer stores a new image that adds the layer blob with hexDigest on top
// of base. config is the new image config, which must list the diff ID of the
// layer as its last diff ID. The image gets the names in names.
func (c *LayerCache) CommitLayer(base Image, layerHex string, layerSize int64, config []byte, names []Reference) (LoadedImage, error) {
	content, err := c.ImageContent(base)
	if err != nil {
		return LoadedImage{}, err
	}
	layers := append([]ocispec.Descriptor(nil), content.Layers...)
	layers = append(layers, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.NewDigestFromEncoded(digest.SHA256, layerHex), Size: layerSize})
	return c.StoreImage(config, layers, names)
}

// StoreImage writes a new image record with the config and the layers, and
// compiles each layer from the blob store when the cache lacks it.
func (c *LayerCache) StoreImage(config []byte, layers []ocispec.Descriptor, names []Reference) (LoadedImage, error) {
	cfgDigest := digest.FromBytes(config)
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: cfgDigest, Size: int64(len(config))},
		Layers:    layers,
	}
	manifest.SchemaVersion = 2
	body, err := json.Marshal(manifest)
	if err != nil {
		return LoadedImage{}, fmt.Errorf("oci: encode manifest: %w", err)
	}
	var cfg ocispec.Image
	if err := json.Unmarshal(config, &cfg); err != nil {
		return LoadedImage{}, fmt.Errorf("oci: decode image config: %w", err)
	}
	arch := normalizeArch(cfg.Architecture)
	for _, l := range layers {
		hexKey := l.Digest.Encoded()
		if _, ok := c.HasLayer(hexKey); ok {
			continue
		}
		f, _, err := c.OpenBlob(hexKey)
		if err != nil {
			return LoadedImage{}, err
		}
		_, _, err = c.compileAndStore(layerSpec{key: hexKey, blobHex: hexKey, blobSize: l.Size}, f)
		_ = f.Close()
		if err != nil {
			return LoadedImage{}, fmt.Errorf("oci: compile layer %s: %w", l.Digest, err)
		}
	}
	rec := imageRecord{Digest: digest.FromBytes(body).String(), Architecture: arch, Manifests: [][]byte{body}, Config: config}
	if err := c.registerLoaded(rec, names); err != nil {
		return LoadedImage{}, err
	}
	return LoadedImage{ID: cfgDigest.String(), Digest: rec.Digest, Names: names}, nil
}
