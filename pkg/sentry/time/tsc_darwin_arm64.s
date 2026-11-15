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

// Rdtsc and getCNTFRQ are declared in upstream files without a body. On
// darwin they jump to Go functions that call libSystem (cycles_darwin_arm64.go).

TEXT ·Rdtsc(SB),NOSPLIT,$0-8
	JMP	·machCycles(SB)

TEXT ·getCNTFRQ(SB),NOSPLIT,$0-8
	JMP	·machCyclesFrequency(SB)

TEXT ·machAbsoluteTimeTrampoline(SB),NOSPLIT,$0-0
	JMP	libc_mach_absolute_time(SB)

TEXT ·addrOfMachAbsoluteTime(SB),NOSPLIT,$0-8
	MOVD	$·machAbsoluteTimeTrampoline(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·machTimebaseInfoTrampoline(SB),NOSPLIT,$0-0
	JMP	libc_mach_timebase_info(SB)

TEXT ·addrOfMachTimebaseInfo(SB),NOSPLIT,$0-8
	MOVD	$·machTimebaseInfoTrampoline(SB), R0
	MOVD	R0, ret+0(FP)
	RET
