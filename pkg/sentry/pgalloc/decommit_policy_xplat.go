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

package pgalloc

// decommitPolicy selects how a MemoryFile returns released pages to the host.
type decommitPolicy struct {
	// punchAll makes decommitRange punch every range, even one that
	// maxZeroedDecommit lets it zero in place. Tests of the hole punch set it
	// before the file is shared.
	punchAll bool
}
