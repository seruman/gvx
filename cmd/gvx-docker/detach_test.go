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
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func readDetach(t *testing.T, input string, keys []byte) (string, bool) {
	t.Helper()
	d := newDetachReader(iotest.OneByteReader(strings.NewReader(input)), keys)
	var got []byte
	buf := make([]byte, 4)
	for {
		n, err := d.Read(buf)
		got = append(got, buf[:n]...)
		if errors.Is(err, errDetached) {
			return string(got), true
		}
		if errors.Is(err, io.EOF) {
			return string(got), false
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetachKeysEndTheInput(t *testing.T) {
	cases := []struct {
		name, input, want string
		detached          bool
	}{
		{"sequence detaches", "ls\x10\x11rest", "ls", true},
		{"partial sequence passes through", "a\x10b", "a\x10b", false},
		{"repeated first key", "\x10\x10\x11", "\x10", true},
		{"plain input", "echo hi\n", "echo hi\n", false},
	}
	for _, tc := range cases {
		got, detached := readDetach(t, tc.input, defaultDetachKeys)
		if got != tc.want || detached != tc.detached {
			t.Errorf("%s: got %q detached=%v, want %q detached=%v", tc.name, got, detached, tc.want, tc.detached)
		}
	}
}

func TestParseDetachKeys(t *testing.T) {
	keys, err := parseDetachKeys("ctrl-a,x")
	if err != nil || string(keys) != "\x01x" {
		t.Fatalf("parseDetachKeys = %q, %v", keys, err)
	}
	if _, err := parseDetachKeys("ctrl-%"); err == nil {
		t.Fatal("ctrl-% parsed")
	}
}
