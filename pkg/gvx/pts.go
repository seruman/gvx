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
	"fmt"
	"strconv"

	"z.gfire.dev/gvx/pkg/abi/linux"
	gvcontext "z.gfire.dev/gvx/pkg/context"
	"z.gfire.dev/gvx/pkg/fspath"
	"z.gfire.dev/gvx/pkg/sentry/fsimpl/devpts"
	"z.gfire.dev/gvx/pkg/sentry/kernel"
	"z.gfire.dev/gvx/pkg/sentry/kernel/auth"
	"z.gfire.dev/gvx/pkg/sentry/vfs"
)

// exposeTerminal gives the terminal tty a node /dev/pts/N in the guest root
// and opens stdin, stdout, and stderr of fdTable through it, so that
// ttyname(3) and tty(1) name the terminal. It returns a function that removes
// the node. A guest without /dev/pts keeps its stdio as it is.
func exposeTerminal(sctx gvcontext.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, fdTable *kernel.FDTable, tty *ttyState) (func(), error) {
	ptsVD, err := vfsObj.GetDentryAt(sctx, creds, &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse("/dev/pts"), FollowFinalSymlink: true}, &vfs.GetDentryOptions{})
	if err != nil {
		return func() {}, nil
	}
	defer ptsVD.DecRef(sctx)
	node, err := devpts.AddExternalReplica(sctx, ptsVD, creds, tty.ktty)
	if err != nil {
		return func() {}, nil
	}
	remove := func() { node.Remove(sctx) }
	open := func(flags uint32) (*vfs.FileDescription, error) {
		return vfsObj.OpenAt(sctx, creds, &vfs.PathOperation{Root: root, Start: ptsVD, Path: fspath.Parse(strconv.FormatUint(uint64(node.Index()), 10))}, &vfs.OpenOptions{Flags: flags | linux.O_NOCTTY})
	}
	for fd := int32(0); fd < 3; fd++ {
		file, err := open(linux.O_RDWR)
		if err != nil {
			remove()
			return func() {}, fmt.Errorf("gvx: open /dev/pts/%d: %w", node.Index(), err)
		}
		displaced, err := fdTable.NewFDAt(sctx, fd, file, kernel.FDFlags{})
		file.DecRef(sctx)
		if displaced != nil {
			displaced.DecRef(sctx)
		}
		if err != nil {
			remove()
			return func() {}, fmt.Errorf("gvx: bind /dev/pts/%d as FD %d: %w", node.Index(), fd, err)
		}
	}
	return remove, nil
}
