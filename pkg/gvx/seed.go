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
	"path"

	"z.gfire.dev/gvx/pkg/abi/linux"
	gvcontext "z.gfire.dev/gvx/pkg/context"
	"z.gfire.dev/gvx/pkg/errors/linuxerr"
	"z.gfire.dev/gvx/pkg/sentry/kernel/auth"
	"z.gfire.dev/gvx/pkg/sentry/vfs"
)

// seedSource holds the directory that the root file system has at a mount
// point before the mount hides it.
type seedSource struct {
	dir vfs.VirtualDentry
}

// openSeedSource returns the image directory at guestPath, or nil when the
// root has no directory there.
func openSeedSource(sctx gvcontext.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, guestPath string) (*seedSource, error) {
	pop := &vfs.PathOperation{Root: root, Start: root, Path: makeFPath(guestPath), FollowFinalSymlink: true}
	vd, err := vfsObj.GetDentryAt(sctx, creds, pop, &vfs.GetDentryOptions{CheckSearchable: true})
	if linuxerr.Equals(linuxerr.ENOENT, err) || linuxerr.Equals(linuxerr.ENOTDIR, err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gvx: open the image directory %s: %w", guestPath, err)
	}
	return &seedSource{dir: vd}, nil
}

func (s *seedSource) release(sctx gvcontext.Context) {
	if s != nil {
		s.dir.DecRef(sctx)
	}
}

// copyInto copies the image directory into the mount at guestPath when the
// mount has no entry. It keeps modes, owners, times, and symbolic links.
func (s *seedSource) copyInto(sctx gvcontext.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, guestPath string) error {
	if s == nil {
		return nil
	}
	src := guestFS{ctx: sctx, vfs: vfsObj, creds: creds, root: s.dir}
	dst := guestFS{ctx: sctx, vfs: vfsObj, creds: creds, root: root}
	existing, err := dst.listDir(guestPath)
	if err != nil {
		return fmt.Errorf("gvx: list the volume at %s: %w", guestPath, err)
	}
	if len(existing) > 0 {
		return nil
	}
	if err := copyTree(src, "/", dst, guestPath); err != nil {
		return fmt.Errorf("gvx: copy the image files into the volume at %s: %w", guestPath, err)
	}
	st, err := src.stat("/", false)
	if err != nil {
		return err
	}
	return dst.setMetadata(guestPath, st)
}

func copyTree(src guestFS, srcPath string, dst guestFS, dstPath string) error {
	names, err := src.listDir(srcPath)
	if err != nil {
		return err
	}
	for _, name := range names {
		from, to := path.Join(srcPath, name), path.Join(dstPath, name)
		st, err := src.stat(from, false)
		if err != nil {
			return err
		}
		switch st.Mode & linux.S_IFMT {
		case linux.S_IFDIR:
			if err := dst.vfs.MkdirAt(dst.ctx, dst.creds, dst.pop(to, false), &vfs.MkdirOptions{Mode: linux.FileMode(st.Mode & 0o7777)}); err != nil {
				return err
			}
			if err := copyTree(src, from, dst, to); err != nil {
				return err
			}
		case linux.S_IFREG:
			fd, err := dst.vfs.OpenAt(dst.ctx, dst.creds, dst.pop(to, false), &vfs.OpenOptions{
				Flags: linux.O_WRONLY | linux.O_CREAT | linux.O_EXCL, Mode: linux.FileMode(st.Mode & 0o7777),
			})
			if err != nil {
				return err
			}
			err = src.copyFileOut(fdWriter{ctx: dst.ctx, fd: fd}, from, int64(st.Size))
			fd.DecRef(dst.ctx)
			if err != nil {
				return err
			}
		case linux.S_IFLNK:
			target, err := src.vfs.ReadlinkAt(src.ctx, src.creds, src.pop(from, false))
			if err != nil {
				return err
			}
			if err := dst.vfs.SymlinkAt(dst.ctx, dst.creds, dst.pop(to, false), target); err != nil {
				return err
			}
		default:
			continue
		}
		if err := dst.setMetadata(to, st); err != nil {
			return err
		}
	}
	return nil
}

func (g guestFS) setMetadata(p string, st linux.Statx) error {
	owner := linux.Statx{Mask: linux.STATX_UID | linux.STATX_GID, UID: st.UID, GID: st.GID}
	if err := g.vfs.SetStatAt(g.ctx, g.creds, g.pop(p, false), &vfs.SetStatOptions{Stat: owner}); err != nil {
		return err
	}
	if st.Mode&linux.S_IFMT == linux.S_IFLNK {
		return nil
	}
	rest := linux.Statx{Mask: linux.STATX_MODE | linux.STATX_ATIME | linux.STATX_MTIME, Mode: st.Mode & 0o7777, Atime: st.Atime, Mtime: st.Mtime}
	return g.vfs.SetStatAt(g.ctx, g.creds, g.pop(p, false), &vfs.SetStatOptions{Stat: rest})
}
