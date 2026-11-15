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
	"net"
	"net/netip"
	"time"
)

// hostFallbackLookup returns the fallback of the NAT DNS relay: the macOS
// system resolver, which uses the host DNS cache and every scoped resolver of
// the host. Call it before hostlockdown.Apply. The relay calls the returned
// function after Apply, and Seatbelt then denies the first connection of the
// process to the resolver daemon. A lookup of "localhost" before Apply opens
// that connection, and later lookups reuse it. "localhost" needs no network.
func hostFallbackLookup() func(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fallbackWarmTimeout)
	defer cancel()
	_, _ = net.DefaultResolver.LookupNetIP(ctx, "ip", "localhost")
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
}

// fallbackWarmTimeout bounds the lookup that opens the resolver connection.
const fallbackWarmTimeout = 2 * time.Second
