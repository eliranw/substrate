// Copyright 2026 Google LLC
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

// Package sizing right-sizes a sandbox to the actor's declared resource limits.
// The bulk of right-sizing is writing the correct cgroup values, which is
// identical for the gVisor and micro-VM runtimes, so both ateom binaries share
// this package: the actor's limits arrive over the ateom RPCs (RunWorkload /
// RestoreWorkload) and ApplyToOCISpec writes them into the container OCI spec.
// runsc then applies them to the host cgroup leaf (gVisor) and the kata-agent
// applies them to the guest cgroup (micro-VM). The micro-VM additionally sizes
// the VM itself from the same SandboxSize (see VCPUs).
package sizing

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	// cpuQuotaPeriodMicros is the cgroup v2 cpu.max period (100ms, the kernel
	// default) against which the CPU quota is expressed.
	cpuQuotaPeriodMicros = 100000
	// cpuQuotaMinMicros is the smallest quota the kernel accepts:
	// tg_set_cfs_bandwidth rejects anything below 1ms with EINVAL.
	cpuQuotaMinMicros = 1000
)

// SandboxSize is the sandbox's target size, derived from the actor's declared
// resource limits. A zero field means "unset": the caller keeps its own default
// (the kata config for the micro-VM, unlimited for gVisor).
type SandboxSize struct {
	// MilliCPU is the CPU limit in millicores (1000 = one core), or 0 if unset.
	MilliCPU int64
	// MemoryBytes is the memory limit in bytes, or 0 if unset.
	MemoryBytes int64
}

// FromLimits builds a SandboxSize from an actor's declared limits (millicores and
// bytes) as carried on the ateom RPCs. It is runtime-agnostic; both ateom-gvisor
// and ateom-microvm call it. Negative values are clamped to zero ("unset").
func FromLimits(milliCPU, memoryBytes int64) SandboxSize {
	if milliCPU < 0 {
		milliCPU = 0
	}
	if memoryBytes < 0 {
		memoryBytes = 0
	}
	return SandboxSize{MilliCPU: milliCPU, MemoryBytes: memoryBytes}
}

// VCPUs converts the CPU limit to a whole vCPU count for the micro-VM, rounding
// up so a fractional limit still yields a usable core (minimum 1 when a limit is
// set). Returns 0 when the CPU limit is unset, letting the caller keep its
// default.
func (s SandboxSize) VCPUs() int {
	if s.MilliCPU <= 0 {
		return 0
	}
	v := (s.MilliCPU + 999) / 1000
	if v < 1 {
		v = 1
	}
	return int(v)
}

// CPUQuota converts a milli-core limit into the cgroup v2 cpu.max pair: a
// container limited to N milli-cores may run for N/1000 of every period.
//
// A quota under 1ms is raised to that floor, as kubelet's MilliCPUToQuota does.
// The kernel rejects a smaller one outright, so an unclamped limit below 10m
// would fail the cgroup write rather than the declared field, with an error
// naming neither. Every caller that turns a limit into an OCI spec goes through
// here so the floor cannot be applied on one path and missed on another.
//
// milliCPU must be positive; callers treat a non-positive limit as "unset" and
// leave the quota off the spec entirely.
func CPUQuota(milliCPU int64) (quota int64, period uint64) {
	return max(milliCPU*cpuQuotaPeriodMicros/1000, cpuQuotaMinMicros), cpuQuotaPeriodMicros
}

// ApplyToOCISpec writes the pod's CPU/memory limits into the container OCI spec's
// linux.resources so the sandbox cgroup is created with the right values. This is
// the piece shared by both runtimes: runsc applies it to the host cgroup leaf
// (gVisor) and the kata-agent applies it to the guest cgroup (micro-VM). Fields
// that are unset in SandboxSize are left untouched, preserving any existing values
// (e.g. the micro-VM's device allowlist and CPU shares).
func (s SandboxSize) ApplyToOCISpec(spec *specs.Spec) {
	if s.MilliCPU <= 0 && s.MemoryBytes <= 0 {
		return
	}
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	if spec.Linux.Resources == nil {
		spec.Linux.Resources = &specs.LinuxResources{}
	}
	if s.MilliCPU > 0 {
		if spec.Linux.Resources.CPU == nil {
			spec.Linux.Resources.CPU = &specs.LinuxCPU{}
		}
		quota, period := CPUQuota(s.MilliCPU)
		spec.Linux.Resources.CPU.Period = &period
		spec.Linux.Resources.CPU.Quota = &quota
	}
	if s.MemoryBytes > 0 {
		if spec.Linux.Resources.Memory == nil {
			spec.Linux.Resources.Memory = &specs.LinuxMemory{}
		}
		limit := s.MemoryBytes
		spec.Linux.Resources.Memory.Limit = &limit
	}
}
