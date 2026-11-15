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
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
	"unsafe"
)

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
	// The timekeeper writes the page within a second of the first task.
	time.Sleep(1500 * time.Millisecond)
	ready := uint64(0)
	if p := vdsoParamPage(); p != 0 {
		// The page starts with the sequence counter, then monotonicReady.
		ready = *(*uint64)(unsafe.Pointer(p + 8))
	}
	fmt.Printf("vdso_ready=%d\n", ready)

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
