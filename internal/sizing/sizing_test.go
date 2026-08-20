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

package sizing

import (
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestFromLimits(t *testing.T) {
	got := FromLimits(1500, 2147483648)
	if got.MilliCPU != 1500 || got.MemoryBytes != 2147483648 {
		t.Fatalf("FromLimits() = %+v", got)
	}
}

func TestFromLimitsClampsNegative(t *testing.T) {
	got := FromLimits(-5, -1)
	if got.MilliCPU != 0 || got.MemoryBytes != 0 {
		t.Fatalf("FromLimits() = %+v, want zero", got)
	}
}

func TestVCPUs(t *testing.T) {
	cases := []struct {
		milli int64
		want  int
	}{
		{0, 0},
		{1, 1},
		{999, 1},
		{1000, 1},
		{1001, 2},
		{2500, 3},
		{4000, 4},
	}
	for _, c := range cases {
		if got := (SandboxSize{MilliCPU: c.milli}).VCPUs(); got != c.want {
			t.Errorf("VCPUs(%d) = %d, want %d", c.milli, got, c.want)
		}
	}
}

func TestApplyToOCISpec(t *testing.T) {
	spec := &specs.Spec{}
	(SandboxSize{MilliCPU: 2000, MemoryBytes: 1073741824}).ApplyToOCISpec(spec)

	if spec.Linux == nil || spec.Linux.Resources == nil {
		t.Fatal("resources not set")
	}
	cpu := spec.Linux.Resources.CPU
	if cpu == nil || cpu.Quota == nil || cpu.Period == nil {
		t.Fatal("cpu not set")
	}
	if *cpu.Period != cpuQuotaPeriodMicros || *cpu.Quota != 2*cpuQuotaPeriodMicros {
		t.Errorf("cpu = quota %d period %d", *cpu.Quota, *cpu.Period)
	}
	mem := spec.Linux.Resources.Memory
	if mem == nil || mem.Limit == nil || *mem.Limit != 1073741824 {
		t.Errorf("memory limit not set correctly: %+v", mem)
	}
}

func TestApplyToOCISpecPreservesExistingAndSkipsUnset(t *testing.T) {
	shares := uint64(1024)
	spec := &specs.Spec{Linux: &specs.Linux{Resources: &specs.LinuxResources{
		CPU: &specs.LinuxCPU{Shares: &shares},
	}}}
	// Only memory set; CPU limit unset must not clobber existing shares and must
	// not add a quota.
	(SandboxSize{MemoryBytes: 512}).ApplyToOCISpec(spec)

	if spec.Linux.Resources.CPU.Shares == nil || *spec.Linux.Resources.CPU.Shares != 1024 {
		t.Error("existing cpu shares clobbered")
	}
	if spec.Linux.Resources.CPU.Quota != nil {
		t.Error("cpu quota set despite unset MilliCPU")
	}
	if spec.Linux.Resources.Memory == nil || *spec.Linux.Resources.Memory.Limit != 512 {
		t.Error("memory limit not applied")
	}
}

func TestApplyToOCISpecNoopWhenEmpty(t *testing.T) {
	spec := &specs.Spec{}
	(SandboxSize{}).ApplyToOCISpec(spec)
	if spec.Linux != nil {
		t.Error("empty SandboxSize mutated spec")
	}
}

// The quota must stay proportional to the declared milli-cores, and never fall
// below the 1ms the kernel accepts: a spec carrying a smaller quota is refused
// at container create with an error naming the cgroup write, not the limit.
func TestCPUQuota(t *testing.T) {
	tests := []struct {
		name      string
		milliCPU  int64
		wantQuota int64
	}{
		{name: "one core", milliCPU: 1000, wantQuota: cpuQuotaPeriodMicros},
		{name: "two cores", milliCPU: 2000, wantQuota: 2 * cpuQuotaPeriodMicros},
		{name: "a fraction of a core", milliCPU: 200, wantQuota: 20000},
		{name: "exactly the floor", milliCPU: 10, wantQuota: cpuQuotaMinMicros},
		{name: "just under the floor is raised", milliCPU: 9, wantQuota: cpuQuotaMinMicros},
		{name: "smallest declarable limit is raised", milliCPU: 1, wantQuota: cpuQuotaMinMicros},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			quota, period := CPUQuota(tc.milliCPU)
			if period != cpuQuotaPeriodMicros {
				t.Errorf("period = %d, want %d", period, cpuQuotaPeriodMicros)
			}
			if quota != tc.wantQuota {
				t.Errorf("quota = %d, want %d", quota, tc.wantQuota)
			}
			if quota < cpuQuotaMinMicros {
				t.Errorf("quota = %d, below the kernel minimum of %d", quota, cpuQuotaMinMicros)
			}
		})
	}
}

// ApplyToOCISpec derives the quota through CPUQuota, so a limit small enough to
// land under the kernel minimum reaches the spec at the floor.
func TestApplyToOCISpecClampsSmallCPULimit(t *testing.T) {
	spec := &specs.Spec{}
	(SandboxSize{MilliCPU: 1}).ApplyToOCISpec(spec)

	cpu := spec.Linux.Resources.CPU
	if cpu == nil || cpu.Quota == nil {
		t.Fatal("cpu quota not set")
	}
	if *cpu.Quota != cpuQuotaMinMicros {
		t.Errorf("quota = %d, want the kernel minimum of %d", *cpu.Quota, cpuQuotaMinMicros)
	}
}
