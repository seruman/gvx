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
	"debug/elf"
	"encoding/binary"
	"fmt"

	"z.gfire.dev/gvx/pkg/abi/linux"
	gvcontext "z.gfire.dev/gvx/pkg/context"
	"z.gfire.dev/gvx/pkg/fspath"
	"z.gfire.dev/gvx/pkg/sentry/fsimpl/tmpfs"
	"z.gfire.dev/gvx/pkg/sentry/kernel/auth"
	"z.gfire.dev/gvx/pkg/sentry/vfs"
	"z.gfire.dev/gvx/pkg/usermem"
)

// idleELF returns a static Linux ELF64 executable for arch that waits for a
// signal forever. It calls pause(2) on amd64 and ppoll(2) with no descriptors
// and no timeout on arm64, in a loop.
func idleELF(arch string) ([]byte, error) {
	const (
		baseVA     = 0x400000
		codeOffset = 0x100
		pageSize   = 4096
	)
	var code []byte
	var machine elf.Machine
	switch arch {
	case "arm64":
		machine = elf.EM_AARCH64
		words := []uint32{
			0xd2800000, // MOVZ X0, #0 (fds)
			0xd2800001, // MOVZ X1, #0 (nfds)
			0xd2800002, // MOVZ X2, #0 (timeout: none)
			0xd2800003, // MOVZ X3, #0 (sigmask)
			0xd2800004, // MOVZ X4, #0 (sigsetsize)
			0xd2800ec8, // MOVZ X8, #118 (SYS_ppoll)
			0xd4000001, // SVC #0
			0x17fffff9, // B back to the first instruction
		}
		for _, w := range words {
			code = binary.LittleEndian.AppendUint32(code, w)
		}
	case "amd64":
		machine = elf.EM_X86_64
		code = []byte{
			0x48, 0xc7, 0xc0, 0x22, 0x00, 0x00, 0x00, // MOV RAX, 34 (SYS_pause)
			0x0f, 0x05, // SYSCALL
			0xeb, 0xf5, // JMP back to the MOV
		}
	default:
		return nil, fmt.Errorf("%w: no idle program for architecture %q", ErrInvalidConfig, arch)
	}
	img := make([]byte, pageSize)
	copy(img[0:4], []byte{0x7f, 'E', 'L', 'F'})
	img[4] = byte(elf.ELFCLASS64)
	img[5] = byte(elf.ELFDATA2LSB)
	img[6] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(img[16:18], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(img[18:20], uint16(machine))
	binary.LittleEndian.PutUint32(img[20:24], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint64(img[24:32], baseVA+codeOffset)
	binary.LittleEndian.PutUint64(img[32:40], 64)
	binary.LittleEndian.PutUint16(img[52:54], 64)
	binary.LittleEndian.PutUint16(img[54:56], 56)
	binary.LittleEndian.PutUint16(img[56:58], 1)
	ph := img[64 : 64+56]
	binary.LittleEndian.PutUint32(ph[0:4], uint32(elf.PT_LOAD))
	binary.LittleEndian.PutUint32(ph[4:8], uint32(elf.PF_R|elf.PF_X))
	binary.LittleEndian.PutUint64(ph[16:24], baseVA)
	binary.LittleEndian.PutUint64(ph[24:32], baseVA)
	binary.LittleEndian.PutUint64(ph[32:40], pageSize)
	binary.LittleEndian.PutUint64(ph[40:48], pageSize)
	binary.LittleEndian.PutUint64(ph[48:56], pageSize)
	copy(img[codeOffset:], code)
	return img, nil
}

// openIdleExecutable writes the idle program to a private tmpfs that no mount
// namespace contains and opens it for execution.
func openIdleExecutable(sctx gvcontext.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, arch string) (*vfs.FileDescription, error) {
	data, err := idleELF(arch)
	if err != nil {
		return nil, err
	}
	fs, rootDentry, err := tmpfs.FilesystemType{}.GetFilesystem(sctx, vfsObj, creds, "", vfs.GetFilesystemOptions{})
	if err != nil {
		return nil, fmt.Errorf("gvx: create the idle program filesystem: %w", err)
	}
	mnt := vfsObj.NewDisconnectedMount(fs, rootDentry, &vfs.MountOptions{})
	fs.DecRef(sctx)
	root := vfs.MakeVirtualDentry(mnt, rootDentry)
	defer root.DecRef(sctx)
	pop := &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse("idle")}
	w, err := vfsObj.OpenAt(sctx, creds, pop, &vfs.OpenOptions{Flags: linux.O_WRONLY | linux.O_CREAT, Mode: 0o755})
	if err != nil {
		return nil, fmt.Errorf("gvx: create the idle program: %w", err)
	}
	_, err = w.Write(sctx, usermem.BytesIOSequence(data), vfs.WriteOptions{})
	w.DecRef(sctx)
	if err != nil {
		return nil, fmt.Errorf("gvx: write the idle program: %w", err)
	}
	fd, err := vfsObj.OpenAt(sctx, creds, pop, &vfs.OpenOptions{Flags: linux.O_RDONLY})
	if err != nil {
		return nil, fmt.Errorf("gvx: open the idle program: %w", err)
	}
	return fd, nil
}
