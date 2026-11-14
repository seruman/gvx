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
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"z.gfire.dev/gvx/pkg/abi/linux"
	gvcontext "z.gfire.dev/gvx/pkg/context"
	"z.gfire.dev/gvx/pkg/errors/linuxerr"
	"z.gfire.dev/gvx/pkg/sentry/kernel/auth"
	"z.gfire.dev/gvx/pkg/sentry/vfs"
	"z.gfire.dev/gvx/pkg/usermem"
)

// ErrPathNotFound reports a guest path that does not exist.
var ErrPathNotFound = errors.New("gvx: guest path not found")

// PathStat describes one guest path for an archive copy.
type PathStat struct {
	Name       string
	Size       int64
	Mode       uint32 // Linux st_mode
	ModTime    time.Time
	LinkTarget string
}

// guestFS gives access to the root file system of a running guest.
type guestFS struct {
	ctx   gvcontext.Context
	vfs   *vfs.VirtualFilesystem
	creds *auth.Credentials
	root  vfs.VirtualDentry
}

// withGuestFS runs fn with the root file system of the running guest. It
// returns ErrNotRunning when the guest does not run.
func (s *Sandbox) withGuestFS(fn func(g guestFS) error) error {
	s.mu.Lock()
	k, tg := s.activeKernel, s.activeThreadGroup
	s.mu.Unlock()
	if k == nil || tg == nil {
		return ErrNotRunning
	}
	leader := tg.Leader()
	if leader == nil {
		return ErrNotRunning
	}
	mntns := leader.GetMountNamespace()
	if mntns == nil {
		return ErrNotRunning
	}
	sctx := k.SupervisorContext()
	defer mntns.DecRef(sctx)
	root := mntns.Root(sctx)
	defer root.DecRef(sctx)
	return fn(guestFS{
		ctx:   vfs.WithRoot(sctx, root),
		vfs:   k.VFS(),
		creds: auth.NewRootCredentials(k.RootUserNamespace()),
		root:  root,
	})
}

func (g guestFS) pop(p string, follow bool) *vfs.PathOperation {
	return &vfs.PathOperation{Root: g.root, Start: g.root, Path: makeFPath(p), FollowFinalSymlink: follow}
}

func (g guestFS) stat(p string, follow bool) (linux.Statx, error) {
	st, err := g.vfs.StatAt(g.ctx, g.creds, g.pop(p, follow), &vfs.StatOptions{Mask: linux.STATX_ALL})
	if linuxerr.Equals(linuxerr.ENOENT, err) || linuxerr.Equals(linuxerr.ENOTDIR, err) {
		return st, fmt.Errorf("%w: %s", ErrPathNotFound, p)
	}
	return st, err
}

func statxTime(t linux.StatxTimestamp) time.Time {
	return time.Unix(t.Sec, int64(t.Nsec))
}

// StatPath returns the metadata of a guest path, as Docker reports it for a
// copy. A symbolic link reports its own metadata and its target.
func (s *Sandbox) StatPath(p string) (PathStat, error) {
	var out PathStat
	err := s.withGuestFS(func(g guestFS) error {
		st, err := g.stat(p, false)
		if err != nil {
			return err
		}
		out = PathStat{Name: path.Base(path.Clean(p)), Size: int64(st.Size), Mode: uint32(st.Mode), ModTime: statxTime(st.Mtime)}
		if st.Mode&linux.S_IFMT == linux.S_IFLNK {
			out.LinkTarget, _ = g.vfs.ReadlinkAt(g.ctx, g.creds, g.pop(p, false))
		}
		return nil
	})
	return out, err
}

// ArchiveRead writes a tar archive of the guest path p to w. The archive
// holds one entry named after the base name of p, and its whole tree for a
// directory. A path that ends in "/." archives the content of the directory
// without the directory itself, as docker cp does.
func (s *Sandbox) ArchiveRead(p string, w io.Writer) error {
	return s.withGuestFS(func(g guestFS) error {
		clean := path.Clean("/" + p)
		base := path.Base(clean)
		if strings.HasSuffix(p, "/.") {
			base = "."
		}
		tw := tar.NewWriter(w)
		if err := g.archiveTree(tw, clean, base); err != nil {
			return err
		}
		return tw.Close()
	})
}

func (g guestFS) archiveTree(tw *tar.Writer, guestPath, name string) error {
	st, err := g.stat(guestPath, false)
	if err != nil {
		return err
	}
	hdr := &tar.Header{
		Name:    name,
		Mode:    int64(st.Mode & 0o7777),
		Uid:     int(st.UID),
		Gid:     int(st.GID),
		ModTime: statxTime(st.Mtime),
		Format:  tar.FormatPAX,
	}
	switch st.Mode & linux.S_IFMT {
	case linux.S_IFDIR:
		hdr.Typeflag = tar.TypeDir
		if name != "." {
			hdr.Name = name + "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		}
		children, err := g.listDir(guestPath)
		if err != nil {
			return err
		}
		for _, child := range children {
			childName := child
			if name != "." {
				childName = name + "/" + child
			}
			if err := g.archiveTree(tw, path.Join(guestPath, child), childName); err != nil {
				return err
			}
		}
		return nil
	case linux.S_IFLNK:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname, err = g.vfs.ReadlinkAt(g.ctx, g.creds, g.pop(guestPath, false))
		if err != nil {
			return err
		}
		return tw.WriteHeader(hdr)
	case linux.S_IFREG:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = int64(st.Size)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		return g.copyFileOut(tw, guestPath, hdr.Size)
	case linux.S_IFCHR, linux.S_IFBLK:
		hdr.Typeflag = tar.TypeChar
		if st.Mode&linux.S_IFMT == linux.S_IFBLK {
			hdr.Typeflag = tar.TypeBlock
		}
		hdr.Devmajor, hdr.Devminor = int64(st.RdevMajor), int64(st.RdevMinor)
		return tw.WriteHeader(hdr)
	case linux.S_IFIFO:
		hdr.Typeflag = tar.TypeFifo
		return tw.WriteHeader(hdr)
	default:
		// Sockets have no archive form, as in GNU tar.
		return nil
	}
}

func (g guestFS) listDir(p string) ([]string, error) {
	fd, err := g.vfs.OpenAt(g.ctx, g.creds, g.pop(p, false), &vfs.OpenOptions{Flags: linux.O_RDONLY | linux.O_DIRECTORY})
	if err != nil {
		return nil, err
	}
	defer fd.DecRef(g.ctx)
	var names []string
	err = fd.IterDirents(g.ctx, vfs.IterDirentsCallbackFunc(func(d vfs.Dirent) error {
		if d.Name != "." && d.Name != ".." {
			names = append(names, d.Name)
		}
		return nil
	}))
	sort.Strings(names)
	return names, err
}

func (g guestFS) copyFileOut(w io.Writer, p string, size int64) error {
	fd, err := g.vfs.OpenAt(g.ctx, g.creds, g.pop(p, false), &vfs.OpenOptions{Flags: linux.O_RDONLY})
	if err != nil {
		return err
	}
	defer fd.DecRef(g.ctx)
	buf := make([]byte, 128*1024)
	var done int64
	for done < size {
		want := min(int64(len(buf)), size-done)
		n, err := fd.Read(g.ctx, usermem.BytesIOSequence(buf[:want]), vfs.ReadOptions{})
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			done += n
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n == 0 {
			break
		}
	}
	if done < size {
		// The file shrank after the stat. Pad, so the archive stays valid.
		_, err := w.Write(make([]byte, size-done))
		return err
	}
	return nil
}

// ArchiveWrite extracts a tar archive into the guest directory dir. Entries
// keep their mode, owner, times, and links. An entry that leaves dir fails.
func (s *Sandbox) ArchiveWrite(dir string, r io.Reader) error {
	return s.withGuestFS(func(g guestFS) error {
		clean := path.Clean("/" + dir)
		st, err := g.stat(clean, true)
		if err != nil {
			return err
		}
		if st.Mode&linux.S_IFMT != linux.S_IFDIR {
			return fmt.Errorf("gvx: %s is not a directory: %w", clean, linuxerr.ENOTDIR)
		}
		tr := tar.NewReader(r)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := g.extract(clean, hdr, tr); err != nil {
				return fmt.Errorf("gvx: extract %s: %w", hdr.Name, err)
			}
		}
	})
}

func (g guestFS) extract(dir string, hdr *tar.Header, r io.Reader) error {
	rel := path.Clean("/" + hdr.Name)
	if rel == "/" {
		return nil
	}
	target := path.Join(dir, rel)
	parent := path.Dir(target)
	if err := vfsEnsureDirAll(g.ctx, g.vfs, g.creds, g.root, parent, 0o755); err != nil {
		return err
	}
	mode := linux.FileMode(hdr.Mode & 0o7777)
	switch hdr.Typeflag {
	case tar.TypeDir:
		if st, err := g.stat(target, false); err == nil {
			if st.Mode&linux.S_IFMT != linux.S_IFDIR {
				return linuxerr.ENOTDIR
			}
		} else if err := g.vfs.MkdirAt(g.ctx, g.creds, g.pop(target, false), &vfs.MkdirOptions{Mode: mode}); err != nil {
			return err
		}
	case tar.TypeReg, tar.TypeRegA:
		g.removeNonDir(target)
		fd, err := g.vfs.OpenAt(g.ctx, g.creds, g.pop(target, false), &vfs.OpenOptions{
			Flags: linux.O_WRONLY | linux.O_CREAT | linux.O_TRUNC | linux.O_NOFOLLOW,
			Mode:  mode,
		})
		if err != nil {
			return err
		}
		buf := make([]byte, 128*1024)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				if _, werr := fd.Write(g.ctx, usermem.BytesIOSequence(buf[:n]), vfs.WriteOptions{}); werr != nil {
					fd.DecRef(g.ctx)
					return werr
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				fd.DecRef(g.ctx)
				return rerr
			}
		}
		fd.DecRef(g.ctx)
	case tar.TypeSymlink:
		g.removeNonDir(target)
		if err := g.vfs.SymlinkAt(g.ctx, g.creds, g.pop(target, false), hdr.Linkname); err != nil {
			return err
		}
	case tar.TypeLink:
		g.removeNonDir(target)
		old := path.Join(dir, path.Clean("/"+hdr.Linkname))
		return g.vfs.LinkAt(g.ctx, g.creds, g.pop(old, false), g.pop(target, false))
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		kind := linux.FileMode(linux.S_IFIFO)
		if hdr.Typeflag == tar.TypeChar {
			kind = linux.S_IFCHR
		} else if hdr.Typeflag == tar.TypeBlock {
			kind = linux.S_IFBLK
		}
		g.removeNonDir(target)
		if err := g.vfs.MknodAt(g.ctx, g.creds, g.pop(target, false), &vfs.MknodOptions{
			Mode: kind | mode, DevMajor: uint32(hdr.Devmajor), DevMinor: uint32(hdr.Devminor),
		}); err != nil {
			return err
		}
	default:
		return nil
	}
	return g.applyMetadata(target, hdr)
}

func (g guestFS) removeNonDir(p string) {
	if st, err := g.stat(p, false); err == nil && st.Mode&linux.S_IFMT != linux.S_IFDIR {
		_ = g.vfs.UnlinkAt(g.ctx, g.creds, g.pop(p, false))
	}
}

func (g guestFS) applyMetadata(p string, hdr *tar.Header) error {
	stat := linux.Statx{Mask: linux.STATX_UID | linux.STATX_GID, UID: uint32(hdr.Uid), GID: uint32(hdr.Gid)}
	if err := g.vfs.SetStatAt(g.ctx, g.creds, g.pop(p, false), &vfs.SetStatOptions{Stat: stat}); err != nil {
		return err
	}
	if hdr.Typeflag == tar.TypeSymlink {
		return nil
	}
	// A chown clears set-user-ID bits, so the mode goes after the owner.
	stat = linux.Statx{
		Mask:  linux.STATX_MODE | linux.STATX_ATIME | linux.STATX_MTIME,
		Mode:  uint16(hdr.Mode & 0o7777),
		Atime: linux.NsecToStatxTimestamp(hdr.ModTime.UnixNano()),
		Mtime: linux.NsecToStatxTimestamp(hdr.ModTime.UnixNano()),
	}
	return g.vfs.SetStatAt(g.ctx, g.creds, g.pop(p, false), &vfs.SetStatOptions{Stat: stat})
}

// fdWriter writes to a guest file description.
type fdWriter struct {
	ctx gvcontext.Context
	fd  *vfs.FileDescription
}

func (w fdWriter) Write(p []byte) (int, error) {
	n, err := w.fd.Write(w.ctx, usermem.BytesIOSequence(p), vfs.WriteOptions{})
	return int(n), err
}

// setRootUpperDentry keeps a reference on the upper layer of the guest root,
// and drops the previous one.
func (s *Sandbox) setRootUpperDentry(ctx gvcontext.Context, vd vfs.VirtualDentry) {
	s.mu.Lock()
	old, oldCtx := s.upperRoot, s.upperRootCtx
	s.upperRoot, s.upperRootCtx = vd, ctx
	s.mu.Unlock()
	if old.Ok() {
		old.DecRef(oldCtx)
	}
}

// ArchiveUpper writes the changes of the guest to its root file system as an
// OCI layer tar: the files of the upper layer, a ".wh.NAME" entry for each
// deleted name, and a ".wh..wh..opq" entry for each opaque directory. It needs
// a running guest with an image.
func (s *Sandbox) ArchiveUpper(w io.Writer) error {
	s.mu.Lock()
	upper := s.upperRoot
	if upper.Ok() {
		upper.IncRef()
	}
	ctx := s.upperRootCtx
	s.mu.Unlock()
	if !upper.Ok() {
		return ErrNotRunning
	}
	// Drop the reference on every path. withGuestFS can return
	// ErrNotRunning without the callback, before the guest starts.
	defer upper.DecRef(ctx)
	return s.withGuestFS(func(g guestFS) error {
		ug := guestFS{ctx: g.ctx, vfs: g.vfs, creds: g.creds, root: upper}
		tw := tar.NewWriter(w)
		if err := ug.archiveUpperDir(tw, "/", ""); err != nil {
			return err
		}
		return tw.Close()
	})
}

func (g guestFS) archiveUpperDir(tw *tar.Writer, dir, prefix string) error {
	names, err := g.listDir(dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		p := path.Join(dir, name)
		st, err := g.stat(p, false)
		if err != nil {
			return err
		}
		entry := prefix + name
		if st.Mode&linux.S_IFMT == linux.S_IFCHR && st.RdevMajor == 0 && st.RdevMinor == 0 {
			if err := tw.WriteHeader(&tar.Header{Name: prefix + ".wh." + name, Typeflag: tar.TypeReg, Mode: 0o644, Format: tar.FormatPAX}); err != nil {
				return err
			}
			continue
		}
		if st.Mode&linux.S_IFMT != linux.S_IFDIR {
			if err := g.archiveTree(tw, p, entry); err != nil {
				return err
			}
			continue
		}
		hdr := &tar.Header{Name: entry + "/", Typeflag: tar.TypeDir, Mode: int64(st.Mode & 0o7777), Uid: int(st.UID), Gid: int(st.GID), ModTime: statxTime(st.Mtime), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if v, err := g.vfs.GetXattrAt(g.ctx, g.creds, g.pop(p, false), &vfs.GetXattrOptions{Name: "trusted.overlay.opaque", Size: 16}); err == nil && v == "y" {
			if err := tw.WriteHeader(&tar.Header{Name: entry + "/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o644, Format: tar.FormatPAX}); err != nil {
				return err
			}
		}
		if err := g.archiveUpperDir(tw, p, entry+"/"); err != nil {
			return err
		}
	}
	return nil
}
