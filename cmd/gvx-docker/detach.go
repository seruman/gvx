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
	"errors"
	"fmt"
	"io"
	"strings"
)

// errDetached reports that the client typed the detach keys.
var errDetached = errors.New("detached")

// defaultDetachKeys is ctrl-p,ctrl-q, as in Docker.
var defaultDetachKeys = []byte{16, 17}

// parseDetachKeys parses the Docker detach key list, such as "ctrl-p,ctrl-q"
// or "a,ctrl-x". An empty list selects the default.
func parseDetachKeys(spec string) ([]byte, error) {
	if spec == "" {
		return defaultDetachKeys, nil
	}
	var out []byte
	for _, key := range strings.Split(spec, ",") {
		key = strings.TrimSpace(key)
		switch {
		case len(key) == 1:
			out = append(out, key[0])
		case strings.HasPrefix(key, "ctrl-") && len(key) == 6:
			c := key[5]
			switch {
			case c >= 'a' && c <= 'z':
				out = append(out, c-'a'+1)
			case c == '@':
				out = append(out, 0)
			case c >= '[' && c <= '_':
				out = append(out, c-'['+27)
			default:
				return nil, fmt.Errorf("invalid detach key %q", key)
			}
		default:
			return nil, fmt.Errorf("invalid detach key %q", key)
		}
	}
	return out, nil
}

// detachReader passes input through until the input holds the detach
// sequence. It then returns errDetached. Bytes of a partial match that turn
// out not to be the sequence pass through.
type detachReader struct {
	r       io.Reader
	keys    []byte
	matched int
	pending []byte
}

func newDetachReader(r io.Reader, keys []byte) *detachReader {
	return &detachReader{r: r, keys: keys}
}

func (d *detachReader) Read(p []byte) (int, error) {
	if len(d.pending) > 0 {
		n := copy(p, d.pending)
		d.pending = d.pending[n:]
		return n, nil
	}
	buf := make([]byte, len(p))
	n, err := d.r.Read(buf)
	out := p[:0]
	for _, b := range buf[:n] {
		if len(d.keys) > 0 && b == d.keys[d.matched] {
			d.matched++
			if d.matched == len(d.keys) {
				d.pending = append(d.pending, out[len(out):]...)
				if len(out) > 0 {
					return len(out), errDetached
				}
				return 0, errDetached
			}
			continue
		}
		if d.matched > 0 {
			// The partial sequence was input after all.
			for _, k := range d.keys[:d.matched] {
				out = appendBounded(out, p, &d.pending, k)
			}
			d.matched = 0
			if b == d.keys[0] {
				d.matched = 1
				continue
			}
		}
		out = appendBounded(out, p, &d.pending, b)
	}
	return len(out), err
}

// appendBounded appends b to out within the capacity of p, and to pending
// beyond it.
func appendBounded(out, p []byte, pending *[]byte, b byte) []byte {
	if len(out) < len(p) {
		return append(out, b)
	}
	*pending = append(*pending, b)
	return out
}
