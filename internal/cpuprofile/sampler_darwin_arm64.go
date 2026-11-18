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

//go:build darwin && arm64

package cpuprofile

import (
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"
	_ "unsafe" // for go:linkname
)

//go:cgo_import_dynamic cpuprofile_dlsym dlsym "/usr/lib/libSystem.B.dylib"

func dlsymTrampoline()
func addrOfDlsym() uintptr

//go:linkname libcCall syscall.syscall6
func libcCall(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// rtldDefault is RTLD_DEFAULT of <dlfcn.h>.
const rtldDefault = ^uintptr(1)

const (
	itimerProf = 2 // ITIMER_PROF of <sys/time.h>.
	// threadBasicInfo is THREAD_BASIC_INFO of <mach/thread_info.h>. The
	// record has 10 32-bit words. Words 0 and 1 hold the user time in seconds
	// and microseconds, words 2 and 3 the system time, and word 6 the run
	// state.
	threadBasicInfo      = 3
	threadBasicInfoWords = 10
	thRunState           = 6
	thStateRunning       = 1 // TH_STATE_RUNNING
)

// threadSample is the state of one thread at one tick.
type threadSample struct {
	cpu     int64 // User and system time in microseconds.
	running bool
}

// libc holds the addresses of the libSystem functions that the sampler calls.
type libc struct {
	taskSelf           uint32
	setitimer          uintptr
	taskThreads        uintptr
	threadInfo         uintptr
	machPortDeallocate uintptr
	vmDeallocate       uintptr
	pthreadKill        uintptr // __pthread_kill takes a Mach thread port.
}

func lookup(name string) (uintptr, error) {
	cname, err := syscall.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	sym, _, _ := libcCall(addrOfDlsym(), rtldDefault, uintptr(unsafe.Pointer(cname)), 0, 0, 0, 0)
	if sym == 0 {
		return 0, fmt.Errorf("cpuprofile: libSystem does not export %s", name)
	}
	return sym, nil
}

var (
	libcOnce sync.Once
	libcTab  libc
	libcErr  error
)

func loadLibc() (*libc, error) {
	libcOnce.Do(func() {
		for _, e := range []struct {
			name string
			dst  *uintptr
		}{
			{"setitimer", &libcTab.setitimer},
			{"task_threads", &libcTab.taskThreads},
			{"thread_info", &libcTab.threadInfo},
			{"mach_port_deallocate", &libcTab.machPortDeallocate},
			{"vm_deallocate", &libcTab.vmDeallocate},
			{"__pthread_kill", &libcTab.pthreadKill},
		} {
			if *e.dst, libcErr = lookup(e.name); libcErr != nil {
				return
			}
		}
		var self uintptr
		if self, libcErr = lookup("mach_task_self_"); libcErr != nil {
			return
		}
		libcTab.taskSelf = *(*uint32)(ptrAt(self))
	})
	return &libcTab, libcErr
}

// ptrAt converts an address that libSystem returned, outside the Go heap,
// to a pointer. The double indirection keeps go vet from reading it as a
// conversion of a Go heap address.
func ptrAt(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}

// disarmProfTimer stops the ITIMER_PROF that runtime/pprof armed.
func (l *libc) disarmProfTimer() error {
	var zero [4]int64 // struct itimerval: two timevals of 16 bytes.
	if r, _, errno := libcCall(l.setitimer, itimerProf, uintptr(unsafe.Pointer(&zero)), 0, 0, 0, 0); int32(r) != 0 {
		return fmt.Errorf("cpuprofile: setitimer(ITIMER_PROF, 0): %w", errno)
	}
	return nil
}

// threadCPU records the CPU time and run state of each thread of the
// process, keyed by Mach thread port name.
func (l *libc) threadCPU(into map[uint32]threadSample) {
	clear(into)
	var list uintptr
	var count uint32
	if kr, _, _ := libcCall(l.taskThreads, uintptr(l.taskSelf), uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&count)), 0, 0, 0); kr != 0 {
		return
	}
	ports := unsafe.Slice((*uint32)(ptrAt(list)), count)
	for _, port := range ports {
		var info [threadBasicInfoWords]int32
		n := uint32(threadBasicInfoWords)
		if kr, _, _ := libcCall(l.threadInfo, uintptr(port), threadBasicInfo, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&n)), 0, 0); kr == 0 {
			into[port] = threadSample{
				cpu:     int64(info[0]+info[2])*1_000_000 + int64(info[1]+info[3]),
				running: info[thRunState] == thStateRunning,
			}
		}
		// task_threads returns one send right for each thread.
		libcCall(l.machPortDeallocate, uintptr(l.taskSelf), uintptr(port), 0, 0, 0, 0)
	}
	libcCall(l.vmDeallocate, uintptr(l.taskSelf), list, uintptr(count)*4, 0, 0, 0)
}

// signal sends SIGPROF to the thread with Mach port name port. The name stays
// valid only while the thread lives; a stale name fails without effect.
func (l *libc) signal(port uint32) bool {
	r, _, _ := libcCall(l.pthreadKill, uintptr(port), uintptr(syscall.SIGPROF), 0, 0, 0, 0)
	return r == 0
}

// startSampler disarms the process timer and starts the goroutine that sends
// SIGPROF in its place.
func startSampler() (func() Stats, error) {
	l, err := loadLibc()
	if err != nil {
		return nil, err
	}
	if err := l.disarmProfTimer(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	result := make(chan Stats, 1)
	go func() {
		result <- sample(l, done)
	}()
	return func() Stats {
		close(done)
		return <-result
	}, nil
}

// sample sends one SIGPROF to a thread for each period of CPU time that the
// thread used, until done closes. A signal records the stack at delivery, so
// the sampler signals only a thread that runs at that tick: a thread that
// already sleeps would record its wait. The used time stays banked until the
// thread runs at a tick. A thread banks at most two periods, because signals
// do not queue.
func sample(l *libc, done <-chan struct{}) Stats {
	var stats Stats
	last := map[uint32]threadSample{}
	now := map[uint32]threadSample{}
	bank := map[uint32]int64{}
	l.threadCPU(last)
	tick := time.NewTicker(periodMicros * time.Microsecond / 2)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return stats
		case <-tick.C:
		}
		l.threadCPU(now)
		for port, cur := range now {
			prev, seen := last[port]
			if !seen || cur.cpu < prev.cpu {
				// A new thread, or a reused port name.
				bank[port] = 0
				continue
			}
			b := min(bank[port]+cur.cpu-prev.cpu, 2*periodMicros)
			if b >= periodMicros && cur.running && l.signal(port) {
				b -= periodMicros
				stats.Signals++
			}
			bank[port] = b
		}
		for port := range bank {
			if _, ok := now[port]; !ok {
				delete(bank, port)
			}
		}
		last, now = now, last
	}
}
