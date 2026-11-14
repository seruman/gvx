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
	"io"
	"sync"

	"z.gfire.dev/gvx/pkg/abi/linux"
	gvcontext "z.gfire.dev/gvx/pkg/context"
	"z.gfire.dev/gvx/pkg/fspath"
	"z.gfire.dev/gvx/pkg/sentry/fsimpl/tmpfs"
	"z.gfire.dev/gvx/pkg/sentry/kernel/auth"
	"z.gfire.dev/gvx/pkg/sentry/vfs"
)

// stdioDeviceMajor is a character device major number from the range that
// Linux reserves for local and experimental use. The three stdio streams use
// the minor numbers 0, 1, and 2 for the main process. Each exec process uses
// the next three free minor numbers.
const stdioDeviceMajor = 240

// Open implements vfs.Device.Open. It copies the streams of the device into
// the new file description under devMu, so an exec that ends or reuses the
// device never changes the streams of a description that is open already.
func (d *stdioDevice) Open(ctx gvcontext.Context, mnt *vfs.Mount, vfsd *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	d.devMu.Lock()
	defer d.devMu.Unlock()
	fd := &stdioFD{
		stream:       d.stream,
		sharedOutErr: d.sharedOutErr,
		dev:          d,
		reader:       d.reader,
		writer:       d.writer,
		tty:          d.tty,
		pump:         d.pump,
		mu:           d.mu,
	}
	if err := fd.vfsfd.Init(fd, opts.Flags, auth.CredentialsFromContext(ctx), mnt, vfsd, &vfs.FileDescriptionOptions{
		SpecialFile: d.tty != nil,
	}); err != nil {
		return nil, err
	}
	d.opens.Add(1)
	return &fd.vfsfd, nil
}

// retire drops the streams of the device after its exec process ended. A
// later open of the device node reads EOF and discards writes, so no guest
// process reaches the streams of an exec that ended.
func (d *stdioDevice) retire() {
	d.devMu.Lock()
	defer d.devMu.Unlock()
	d.reader, d.writer, d.tty, d.pump = nil, nil, nil, nil
}

// openStdioFDs registers the stdio devices in vfsObj and returns the file
// descriptions for stdin, stdout, and stderr. The device nodes live on a
// private tmpfs that no mount namespace contains, so the guest sees the
// streams only through its descriptor table.
func openStdioFDs(ctx gvcontext.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, in io.Reader, out, errw io.Writer, tty *ttyState, pump *stdinPump) (fds [3]*vfs.FileDescription, err error) {
	return openStdioFDsAt(ctx, vfsObj, creds, 0, in, out, errw, tty, pump)
}

// openStdioFDsAt is openStdioFDs with the device minor numbers firstMinor to
// firstMinor+2.
func openStdioFDsAt(ctx gvcontext.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, firstMinor uint32, in io.Reader, out, errw io.Writer, tty *ttyState, pump *stdinPump) (fds [3]*vfs.FileDescription, err error) {
	devs := newStdioDevices(in, out, errw, tty, pump)
	if err := registerStdioDevices(vfsObj, firstMinor, devs); err != nil {
		return fds, err
	}
	return openStdioNodes(ctx, vfsObj, creds, firstMinor, tty != nil)
}

var stdioNames = [3]string{"stdin", "stdout", "stderr"}

// newStdioDevices returns the devices of stdin, stdout, and stderr.
func newStdioDevices(in io.Reader, out, errw io.Writer, tty *ttyState, pump *stdinPump) [3]*stdioDevice {
	devs := [3]*stdioDevice{
		{stream: stdioStreamStdin, reader: in, pump: pump},
		{stream: stdioStreamStdout, writer: out},
		{stream: stdioStreamStderr, writer: errw},
	}
	if tty != nil {
		sharedOutErr := errw == out
		errMu := tty.outMu
		if !sharedOutErr {
			errMu = new(sync.Mutex)
		}
		devs = [3]*stdioDevice{
			{stream: stdioStreamStdin, reader: in, writer: out, tty: tty, mu: tty.outMu},
			{stream: stdioStreamStdout, reader: in, writer: out, tty: tty, mu: tty.outMu},
			{stream: stdioStreamStderr, sharedOutErr: sharedOutErr, reader: in, writer: errw, tty: tty, mu: errMu},
		}
	}
	for _, dev := range devs {
		if dev.mu == nil {
			dev.mu = new(sync.Mutex)
		}
	}
	return devs
}

// registerStdioDevices registers devs with the minor numbers firstMinor to
// firstMinor+2.
func registerStdioDevices(vfsObj *vfs.VirtualFilesystem, firstMinor uint32, devs [3]*stdioDevice) error {
	for i, dev := range devs {
		if err := vfsObj.RegisterDevice(vfs.CharDevice, stdioDeviceMajor, firstMinor+uint32(i), dev, &vfs.RegisterDeviceOptions{}); err != nil {
			return fmt.Errorf("gvx: register the %s device: %w", stdioNames[i], err)
		}
	}
	return nil
}

// openStdioNodes creates the device nodes of the minor numbers firstMinor to
// firstMinor+2 on a private tmpfs and opens them.
func openStdioNodes(ctx gvcontext.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, firstMinor uint32, terminal bool) (fds [3]*vfs.FileDescription, err error) {
	flags := [3]uint32{linux.O_RDONLY, linux.O_WRONLY, linux.O_WRONLY}
	if terminal {
		flags = [3]uint32{linux.O_RDWR, linux.O_RDWR, linux.O_RDWR}
	}
	fs, rootDentry, err := tmpfs.FilesystemType{}.GetFilesystem(ctx, vfsObj, creds, "", vfs.GetFilesystemOptions{})
	if err != nil {
		return fds, fmt.Errorf("gvx: create the stdio device filesystem: %w", err)
	}
	mnt := vfsObj.NewDisconnectedMount(fs, rootDentry, &vfs.MountOptions{})
	fs.DecRef(ctx)
	root := vfs.MakeVirtualDentry(mnt, rootDentry)
	defer root.DecRef(ctx)

	defer func() {
		if err == nil {
			return
		}
		for i, fd := range fds {
			if fd != nil {
				fd.DecRef(ctx)
				fds[i] = nil
			}
		}
	}()
	for i, name := range stdioNames {
		pop := &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(name)}
		if err := vfsObj.MknodAt(ctx, creds, pop, &vfs.MknodOptions{
			Mode:     linux.S_IFCHR | 0o666,
			DevMajor: stdioDeviceMajor,
			DevMinor: firstMinor + uint32(i),
		}); err != nil {
			return fds, fmt.Errorf("gvx: create the %s device node: %w", name, err)
		}
		fd, err := vfsObj.OpenAt(ctx, creds, pop, &vfs.OpenOptions{Flags: flags[i]})
		if err != nil {
			return fds, fmt.Errorf("gvx: open the %s device node: %w", name, err)
		}
		fds[i] = fd
	}
	return fds, nil
}

// stdioSet is the three stdio devices of one exec process.
type stdioSet struct {
	firstMinor uint32
	devs       [3]*stdioDevice
}

// adopt points the devices of set at the streams of fresh when no guest file
// description has any of them open. It reports whether it did. It holds the
// lock of the three devices, so an Open runs either before the check, and
// then the set stays retired, or after the new streams are in place.
func (set stdioSet) adopt(fresh [3]*stdioDevice) bool {
	in, out, errd := set.devs[0], set.devs[1], set.devs[2]
	in.devMu.Lock()
	defer in.devMu.Unlock()
	out.devMu.Lock()
	defer out.devMu.Unlock()
	errd.devMu.Lock()
	defer errd.devMu.Unlock()
	if in.opens.Load() != 0 || out.opens.Load() != 0 || errd.opens.Load() != 0 {
		return false
	}
	in.adoptLocked(fresh[0])
	out.adoptLocked(fresh[1])
	errd.adoptLocked(fresh[2])
	return true
}

// adoptLocked copies the streams of f into d. The caller holds d.devMu.
func (d *stdioDevice) adoptLocked(f *stdioDevice) {
	d.stream, d.sharedOutErr = f.stream, f.sharedOutErr
	d.reader, d.writer, d.tty, d.pump, d.mu = f.reader, f.writer, f.tty, f.pump, f.mu
}

// openExecStdio opens the stdio streams of an exec process. It reuses the
// devices of an exec that ended when no guest file description holds them,
// and registers three new devices otherwise, so the number of devices stays
// bounded for any number of sequential execs. release retires the devices.
func (s *Sandbox) openExecStdio(ctx gvcontext.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, in io.Reader, out, errw io.Writer, tty *ttyState, pump *stdinPump) (fds [3]*vfs.FileDescription, release func(), err error) {
	fresh := newStdioDevices(in, out, errw, tty, pump)
	var set stdioSet
	reused := false
	s.mu.Lock()
	for i, free := range s.freeStdio {
		if free.adopt(fresh) {
			set, reused = free, true
			s.freeStdio = append(s.freeStdio[:i], s.freeStdio[i+1:]...)
			break
		}
	}
	if !reused {
		if s.execStdioMinor < 3 {
			s.execStdioMinor = 3
		}
		set = stdioSet{firstMinor: s.execStdioMinor, devs: fresh}
		s.execStdioMinor += 3
	}
	s.mu.Unlock()
	if !reused {
		if err := registerStdioDevices(vfsObj, set.firstMinor, set.devs); err != nil {
			return fds, nil, err
		}
	}
	release = func() {
		for _, d := range set.devs {
			d.retire()
		}
		s.mu.Lock()
		s.freeStdio = append(s.freeStdio, set)
		s.mu.Unlock()
	}
	fds, err = openStdioNodes(ctx, vfsObj, creds, set.firstMinor, tty != nil)
	if err != nil {
		release()
		return fds, nil, err
	}
	return fds, release, nil
}
