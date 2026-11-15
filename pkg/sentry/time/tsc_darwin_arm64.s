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

#include "textflag.h"

// commpageTimebaseOffset is _COMM_PAGE_TIMEBASE_OFFSET; the byte after it is
// _COMM_PAGE_USER_TIMEBASE, which names the counter that user space reads.
#define commpageTimebaseOffset 0xfffffc088

// Rdtsc returns mach_absolute_time. A guest under Hypervisor.framework reads
// the same value from CNTVCT_EL0, so the guest vDSO converts its counter with
// the parameters that the Sentry calibrates against Rdtsc. The host
// CNTVCT_EL0 can count at another rate, so a plain read gives a wrong guest
// time.
//
// The code follows mach_absolute_time in libsystem_kernel: the commpage names
// the counter, and the counter plus the timebase offset is the result. It
// reads the offset before and after the counter until both reads agree. It
// makes no call, so it is safe in nosplit code and after the host lockdown.
TEXT ·Rdtsc(SB),NOSPLIT,$0-8
	MOVD	$commpageTimebaseOffset, R3
	MOVBU	8(R3), R2
	CMP	$3, R2
	BEQ	apple
	CMP	$2, R2
	BEQ	selfsync
	ISB	$15
virtual:
	MOVD	(R3), R1
	WORD	$0xd53be040     // MRS CNTVCT_EL0, R0
	MOVD	(R3), R2
	CMP	R1, R2
	BNE	virtual
	B	done
selfsync:
	MOVD	(R3), R1
	WORD	$0xd53be0c0     // MRS CNTVCTSS_EL0, R0
	MOVD	(R3), R2
	CMP	R1, R2
	BNE	selfsync
	B	done
apple:
	MOVD	(R3), R1
	WORD	$0xd53cfac0     // MRS S3_4_C15_C10_6, R0: the Apple timebase counter
	MOVD	(R3), R2
	CMP	R1, R2
	BNE	apple
done:
	ADD	R1, R0, R0
	MOVD	R0, ret+0(FP)
	RET

// getCNTFRQ returns the frequency of Rdtsc: 24 MHz, the mach_absolute_time
// rate of every Apple Silicon chip and the CNTFRQ_EL0 that a guest reads.
TEXT ·getCNTFRQ(SB),NOSPLIT,$0-8
	MOVD	$24000000, R0
	MOVD	R0, ret+0(FP)
	RET
