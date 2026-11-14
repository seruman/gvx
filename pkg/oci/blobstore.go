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
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The cache serves compiled EROFS layers, which do not hold the original
// compressed layer blobs. A Docker engine needs those blobs to save, push,
// commit, and export images that came from an archive or a local build. With
// EnableBlobStore, each layer that the cache compiles also keeps its original
// stream in blobs/sha256/<hex>. The file is the verified stream: the name is
// its sha256, which the compiler checked against the descriptor when it had
// one.

// EnableBlobStore makes the cache keep the original stream of every layer that
// it compiles from now on.
func (c *LayerCache) EnableBlobStore() {
	c.mu.Lock()
	c.keepBlobs = true
	c.mu.Unlock()
}

func (c *LayerCache) blobDir() string { return filepath.Join(c.dir, "blobs", "sha256") }

// BlobPath returns the path of a stored blob, and false when the cache does
// not hold it.
func (c *LayerCache) BlobPath(hexDigest string) (string, bool) {
	hexKey := normalizeDigestHex(hexDigest)
	if hexKey == "" {
		return "", false
	}
	p := filepath.Join(c.blobDir(), hexKey)
	if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return p, true
}

// OpenBlob opens a stored blob for reading.
func (c *LayerCache) OpenBlob(hexDigest string) (*os.File, int64, error) {
	p, ok := c.BlobPath(hexDigest)
	if !ok {
		return nil, 0, fmt.Errorf("%w: blob %s", ErrNotCached, hexDigest)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func (c *LayerCache) createTempBlob() (*os.File, error) {
	if err := os.MkdirAll(c.blobDir(), 0o755); err != nil {
		return nil, fmt.Errorf("oci: create blob directory: %w", err)
	}
	f, err := os.CreateTemp(c.blobDir(), ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("oci: create blob: %w", err)
	}
	return f, nil
}

// publishBlob gives the temporary blob f its final name and closes it. An
// existing blob with the name stays, because it holds the same bytes.
func (c *LayerCache) publishBlob(f *os.File, hexDigest string) error {
	defer func() { _ = os.Remove(f.Name()) }()
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("oci: sync blob: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("oci: close blob: %w", err)
	}
	target := filepath.Join(c.blobDir(), hexDigest)
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	if err := os.Link(f.Name(), target); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("oci: publish blob %s: %w", hexDigest, err)
	}
	return nil
}

// StoreBlob stores the stream r as a blob and returns its sha256 in hex.
func (c *LayerCache) StoreBlob(r io.Reader) (string, int64, error) {
	f, err := c.createTempBlob()
	if err != nil {
		return "", 0, err
	}
	h := newSHA256()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", 0, fmt.Errorf("oci: write blob: %w", err)
	}
	hexDigest := fmt.Sprintf("%x", h.Sum(nil))
	return hexDigest, n, c.publishBlob(f, hexDigest)
}

func newSHA256() hash.Hash { return sha256.New() }

// needsCompile reports whether a layer must go through the compiler: the cache
// lacks its EROFS image, or the blob store is on and lacks its blob.
func (c *LayerCache) needsCompile(hexKey string) bool {
	if _, ok := c.HasLayer(hexKey); !ok {
		return true
	}
	c.mu.Lock()
	keep := c.keepBlobs
	c.mu.Unlock()
	if !keep {
		return false
	}
	_, ok := c.BlobPath(hexKey)
	return !ok
}

// FetchMissingBlobs downloads the original layer blobs of img that the blob
// store lacks from the registry that the image came from, and verifies each
// one. An image that a commit or an import made has every blob already. An
// image from an archive without blobs fails with ErrNoRegistrySource.
func (c *LayerCache) FetchMissingBlobs(ctx context.Context, img Image) error {
	dec, err := c.verifyRecord(img.record)
	if err != nil {
		return err
	}
	var missing []ocispec.Descriptor
	for _, l := range dec.manifest.Layers {
		if _, ok := c.BlobPath(l.Digest.Encoded()); !ok {
			missing = append(missing, l)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	baseURL, repo, err := img.blobSource()
	if err != nil {
		return err
	}
	rc, err := newRegistryClient(nil, baseURL, repo, c.limits, c.dnsServers)
	if err != nil {
		return err
	}
	defer rc.close()
	for _, l := range missing {
		if err := checkLayerDescriptor(l, c.limits); err != nil {
			return err
		}
		body, err := rc.openBlob(ctx, l.Digest.String())
		if err != nil {
			return err
		}
		hexDigest, size, err := c.StoreBlob(io.LimitReader(body, l.Size+1))
		_ = body.Close()
		if err != nil {
			return err
		}
		if hexDigest != l.Digest.Encoded() || size != l.Size {
			_ = os.Remove(filepath.Join(c.blobDir(), hexDigest))
			return fmt.Errorf("%w: blob %s from %s has sha256 %s and %d bytes", ErrDigestMismatch, l.Digest, repo, hexDigest, size)
		}
	}
	return nil
}
