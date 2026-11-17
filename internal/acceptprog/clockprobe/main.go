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

//go:build linux

// Command clockprobe reports how the guest reads the time. It prints one
// "key=value" line for each measurement:
//
//	vdso_ready    the monotonic ready flag of the vDSO parameter page
//	now_ns        the average cost of time.Now
//	unix_ns       the wall time of the guest, in nanoseconds since the epoch
//	sleep_ms      a 200 ms sleep, timed with time.Now
//	monotonic_ok  1 when 100000 reads of time.Now never went back
//
// With the argument "boot", it prints only boot_vdso_ready and
// boot_vdso_realtime_ready, read before any wait.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
	_ "unsafe" // for go:linkname
)

// vdsoMonotonic is CLOCK_MONOTONIC read through the vDSO: the Go runtime
// reads it there without a system call.
//
//go:linkname vdsoMonotonic runtime.nanotime
func vdsoMonotonic() int64

// syscallMonotonic is CLOCK_MONOTONIC read through the system call.
func syscallMonotonic() int64 {
	var ts syscall.Timespec
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0); errno != 0 {
		return -1
	}
	return ts.Nano()
}

// monotonicSteps reads CLOCK_MONOTONIC for d, alternating the vDSO and the
// system call. It returns the largest backward step of each source, and the
// largest backward step from one source to the next reading of the other.
func monotonicSteps(d time.Duration) (vdsoBack, sysBack, crossBack int64, reads int) {
	end := vdsoMonotonic() + int64(d)
	lastV, lastS := vdsoMonotonic(), syscallMonotonic()
	for {
		v := vdsoMonotonic()
		vdsoBack = max(vdsoBack, lastV-v)
		crossBack = max(crossBack, lastS-v)
		sv := syscallMonotonic()
		sysBack = max(sysBack, lastS-sv)
		crossBack = max(crossBack, v-sv)
		lastV, lastS = v, sv
		reads += 2
		if v >= end {
			return
		}
	}
}

// atSysinfoEhdr is AT_SYSINFO_EHDR: the auxv key of the vDSO address.
const atSysinfoEhdr = 33

// vdsoParamPage returns the address of the parameter page, which the loader
// maps one page below the vDSO.
func vdsoParamPage() uintptr {
	auxv, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 0
	}
	for i := 0; i+16 <= len(auxv); i += 16 {
		if binary.LittleEndian.Uint64(auxv[i:]) == atSysinfoEhdr {
			return uintptr(binary.LittleEndian.Uint64(auxv[i+8:])) - uintptr(os.Getpagesize())
		}
	}
	return 0
}

func main() {
	// With the argument "boot", the probe reports the ready flags of the
	// vDSO parameter page at once, before any wait, and exits.
	if len(os.Args) > 1 && os.Args[1] == "boot" {
		ready, realtimeReady := uint64(0), uint64(0)
		if p := vdsoParamPage(); p != 0 {
			ready = *(*uint64)(unsafe.Pointer(p + 8))
			realtimeReady = *(*uint64)(unsafe.Pointer(p + 40))
		}
		fmt.Printf("boot_vdso_ready=%d\nboot_vdso_realtime_ready=%d\n", ready, realtimeReady)
		return
	}
	// With the argument "wait", the probe prints "waiting" and reads one
	// line of stdin before it measures. A checkpoint test stops the guest
	// there, so the measurement runs in the restored guest.
	if len(os.Args) > 1 && os.Args[1] == "wait" {
		fmt.Println("waiting")
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			fmt.Fprintln(os.Stderr, "read stdin:", err)
			os.Exit(1)
		}
		// Right after a restore, the timekeeper calibrates again. Monotonic
		// time must not go back meanwhile, in either source.
		vb, sb, cb, n := monotonicSteps(2 * time.Second)
		fmt.Printf("mono_vdso_back_ns=%d\nmono_sys_back_ns=%d\nmono_cross_back_ns=%d\nmono_reads=%d\n", vb, sb, cb, n)
	}
	// The timekeeper writes the page within a second of the first task. A
	// restored timekeeper calibrates again, and its time settles within
	// about 2 seconds (see TestRestoredGuestReadsTheClockThroughTheVDSO).
	settle := 1500 * time.Millisecond
	if len(os.Args) > 1 && os.Args[1] == "wait" {
		settle = 3 * time.Second
	}
	time.Sleep(settle)
	ready, realtimeReady := uint64(0), uint64(0)
	if p := vdsoParamPage(); p != 0 {
		// The page holds the sequence counter, then four words of the
		// monotonic clock, then four words of the realtime clock. Each
		// group starts with its ready flag.
		ready = *(*uint64)(unsafe.Pointer(p + 8))
		realtimeReady = *(*uint64)(unsafe.Pointer(p + 40))
	}
	fmt.Printf("vdso_ready=%d\n", ready)
	fmt.Printf("vdso_realtime_ready=%d\n", realtimeReady)

	const n = 100000
	prev := time.Now()
	monotonic := 1
	start := time.Now()
	for i := 0; i < n; i++ {
		now := time.Now()
		if now.Before(prev) {
			monotonic = 0
		}
		prev = now
	}
	fmt.Printf("now_ns=%d\n", time.Since(start).Nanoseconds()/n)
	fmt.Printf("monotonic_ok=%d\n", monotonic)
	fmt.Printf("unix_ns=%d\n", time.Now().UnixNano())

	a := time.Now()
	time.Sleep(200 * time.Millisecond)
	fmt.Printf("sleep_ms=%d\n", time.Since(a).Milliseconds())
}
