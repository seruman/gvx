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

package gvx

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"z.gfire.dev/gvx/pkg/hostlockdown"
)

const fallbackChildEnv = "GVX_TEST_DNS_FALLBACK_CHILD"

// TestHostFallbackLookupChild is not a test. The test below starts the test
// binary again with fallbackChildEnv set to a host name. The child prepares
// the fallback, applies the lockdown of a NAT sandbox, and resolves the name
// through the fallback, as the DNS relay does after the lockdown.
func TestHostFallbackLookupChild(t *testing.T) {
	host := os.Getenv(fallbackChildEnv)
	if host == "" {
		return
	}
	lookup := hostFallbackLookup()
	if _, err := hostlockdown.Apply(hostlockdown.Options{AllowOutboundNet: true}); err != nil {
		fmt.Println("lockdown:", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		fmt.Printf("lookup %s after lockdown: %v %v\n", host, addrs, err)
		os.Exit(3)
	}
	fmt.Println("resolved", addrs)
	os.Exit(0)
}

// TestHostFallbackLookupWorksAfterLockdown resolves a public name through
// the fallback in a process under the Seatbelt profile of a NAT sandbox. The
// child process never resolved the name before.
func TestHostFallbackLookupWorksAfterLockdown(t *testing.T) {
	const host = "example.com."
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host); err != nil {
		t.Skipf("the host cannot resolve %s: %v", host, err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHostFallbackLookupChild$")
	cmd.Env = append(os.Environ(), fallbackChildEnv+"="+host, "GORACE=atexit_sleep_ms=0")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "resolved") {
		t.Fatalf("fallback lookup under the lockdown failed: %v\n%s", err, out)
	}
}
