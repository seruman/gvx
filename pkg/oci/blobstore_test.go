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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// TestFetchMissingBlobsKeepsAValidBlobOnAMismatch makes a registry send the
// bytes of one layer for the descriptor of another layer, when the blob
// store already holds the first layer. The fetch must fail and leave the
// stored blob in place: another image can need it, and no registry may be
// able to send it again.
func TestFetchMissingBlobsKeepsAValidBlobOnAMismatch(t *testing.T) {
	reg := newFakeRegistry(t)
	img := twoLayerImage(t, "arm64", "A=1")
	reg.addImage(img)
	reg.tag("latest", img.digest())
	cache := newCache(t)
	mustPull(t, reg, cache, "latest", "arm64")

	// The blob store holds layer 0, as after a commit or a load. Layer 1 is
	// missing, and the registry answers its digest with the bytes of layer 0.
	valid := img.blobs[0]
	validHex, _, err := cache.StoreBlob(bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	reg.tamperBlob[sha256Digest(img.blobs[1])] = valid

	sel, err := cache.Select(reg.srv.Listener.Addr().String()+"/"+testRepo+":latest", "arm64")
	if err != nil || len(sel.Images) == 0 {
		t.Fatalf("Select: %v", err)
	}
	err = cache.FetchMissingBlobs(context.Background(), sel.Images[0])
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("FetchMissingBlobs = %v, want ErrDigestMismatch", err)
	}
	p, ok := cache.BlobPath(validHex)
	if !ok {
		t.Fatal("the fetch removed a valid blob that the store held before")
	}
	got, err := os.ReadFile(p)
	if err != nil || digest.FromBytes(got).Encoded() != validHex {
		t.Fatalf("the stored blob changed: %v", err)
	}
}

// TestEnableBlobStoreDuringAPull turns the blob store on while a pull
// compiles layers. Run it with -race: the compiler reads the setting that
// EnableBlobStore writes. The blob hook turns the store on while a layer
// downloads, so the write and the read overlap on every run.
func TestEnableBlobStoreDuringAPull(t *testing.T) {
	reg := newFakeRegistry(t)
	img := twoLayerImage(t, "arm64", "A=1")
	reg.addImage(img)
	reg.tag("latest", img.digest())
	cache := newCache(t)
	reg.onBlob = func(w http.ResponseWriter, r *http.Request, d string) bool {
		if r.Method != http.MethodGet {
			return false
		}
		go cache.EnableBlobStore()
		time.Sleep(20 * time.Millisecond)
		return false
	}
	mustPull(t, reg, cache, "latest", "arm64")
}

// TestPruneRemovesUnusedBlobsAndCountsThem proves that the blob store takes
// part in the cache accounting. DiskUsage counts the blobs, a prune of an
// image removes the blobs that no image uses, and a prune removes a stale
// temporary blob that a crash left, but not a fresh one that a pull writes.
func TestPruneRemovesUnusedBlobsAndCountsThem(t *testing.T) {
	reg := newFakeRegistry(t)
	img := twoLayerImage(t, "arm64", "A=1")
	reg.addImage(img)
	reg.tag("latest", img.digest())
	cache := newCache(t)
	cache.EnableBlobStore()
	mustPull(t, reg, cache, "latest", "arm64")
	blobHex := digest.FromBytes(img.blobs[0]).Encoded()
	if _, ok := cache.BlobPath(blobHex); !ok {
		t.Fatal("the pull stored no blob")
	}
	// The totals must grow by exactly the size of a new blob.
	beforeFiles, err := cache.cacheFileBytes()
	if err != nil {
		t.Fatal(err)
	}
	beforeUsage, err := cache.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	extra := bytes.Repeat([]byte("x"), 12345)
	if _, _, err := cache.StoreBlob(bytes.NewReader(extra)); err != nil {
		t.Fatal(err)
	}
	var failures []string
	if after, err := cache.cacheFileBytes(); err != nil || after-beforeFiles != int64(len(extra)) {
		failures = append(failures, fmt.Sprintf("cacheFileBytes grew by %d (%v) for a blob of %d bytes", after-beforeFiles, err, len(extra)))
	}
	if after, err := cache.DiskUsage(); err != nil || after.TotalBytes-beforeUsage.TotalBytes != int64(len(extra)) {
		failures = append(failures, fmt.Sprintf("DiskUsage.TotalBytes grew by %d (%v) for a blob of %d bytes", after.TotalBytes-beforeUsage.TotalBytes, err, len(extra)))
	}
	if err := os.MkdirAll(cache.blobDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(cache.blobDir(), ".tmp-stale")
	fresh := filepath.Join(cache.blobDir(), ".tmp-fresh")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTempBlobAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.PruneImages(PruneOptions{All: true, Arch: "arm64"}); err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if _, ok := cache.BlobPath(blobHex); ok {
		failures = append(failures, "the prune kept a blob that no image uses")
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		failures = append(failures, fmt.Sprintf("the prune kept a stale temporary blob: %v", err))
	}
	if _, err := os.Stat(fresh); err != nil {
		failures = append(failures, fmt.Sprintf("the prune removed a fresh temporary blob that a pull can be writing: %v", err))
	}
	for _, f := range failures {
		t.Error(f)
	}
}

// TestRemoveDeletesTheBlobsOfItsUnusedLayers proves that docker rmi, which
// calls Remove, frees the blobs of the layers that no other image uses, as it
// frees their EROFS images.
func TestRemoveDeletesTheBlobsOfItsUnusedLayers(t *testing.T) {
	reg := newFakeRegistry(t)
	img := twoLayerImage(t, "arm64", "A=1")
	reg.addImage(img)
	reg.tag("latest", img.digest())
	cache := newCache(t)
	cache.EnableBlobStore()
	mustPull(t, reg, cache, "latest", "arm64")
	sel, err := cache.Select(reg.srv.Listener.Addr().String()+"/"+testRepo+":latest", "arm64")
	if err != nil || len(sel.Images) == 0 {
		t.Fatalf("Select: %v", err)
	}
	if _, err := cache.Remove(sel.Images[0]); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, b := range img.blobs {
		if _, ok := cache.BlobPath(digest.FromBytes(b).Encoded()); ok {
			t.Fatalf("Remove kept the blob of a layer that no image uses")
		}
	}
}
