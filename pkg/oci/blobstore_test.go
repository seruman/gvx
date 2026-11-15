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
	"os"
	"testing"

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
