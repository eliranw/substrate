# ateom-microvm GPU Passthrough Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A microVM actor on a GPU worker pool runs `nvidia-smi` (and CUDA) end to end, by cold-plugging the worker pod's VFIO GPU into cloud-hypervisor and letting NVIDIA's UVM guest inject `/dev/nvidia*` into the actor container.

**Architecture:** ateom (already its own cloud-hypervisor launcher) resolves the worker pod's granted VFIO GPU(s), cold-plugs them into `vm.create`, boots NVIDIA's prebuilt Kata GPU image (UVM) via a SandboxConfig asset swap, and drives the UVM's kata-agent with a CDI annotation so NVRC's guest-created `/dev/nvidia*` nodes land in the actor container. GPU actors are run-only (a snapshot gate blocks Full-scope snapshots of a VFIO-holding VM).

**Tech Stack:** Go; cloud-hypervisor REST API; kata-agent ttrpc; Linux VFIO (`/dev/vfio`, `/sys/kernel/iommu_groups`); Kubernetes CRDs (SandboxConfig, WorkerPool); NVIDIA GPU Operator (vfio mode) + UVM image.

## Global Constraints

- Guest-facing and `/sys`/`/dev`-reading Go files are `//go:build linux`. Tests for them run on Linux only — on darwin, run via `docker run --rm -v "$PWD":/src -w /src golang:1.26 go test ...`.
- Module path: `github.com/agent-substrate/substrate`. The ch package import is `github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch`.
- TDD: write the failing test first, watch it fail, implement minimally, watch it pass, commit. One logical change per commit.
- No new upstream cloud-hypervisor or kata changes. No scheduler/atelet changes (1-actor-per-worker is inherited from `AssignWorkerStep.findFreeWorker`).
- `WorkerPool.Template.Resources` already flows to the pod (`workerpool_apply.go:158-163`); `nvidia.com/gpu` is pure config.
- Fail closed: if a VFIO group is present in the pod but cannot be resolved, error; if no group is present, boot normally (non-GPU worker).
- Copy the Apache license header block present in sibling files into every new `.go` file.

---

## Task ordering & gating

- Tasks 1–4 are hardware-independent and can proceed immediately.
- Task 5 (compatibility spike) is a **manual gate** on real hardware; it produces the concrete values Task 6 needs.
- Task 6 depends on Task 5's outputs. Task 7 (E2E) depends on Task 6.
- Task 8 is an optional follow-up (CDI-sourced discovery), not required for E2E.

---

### Task 1: Rework GPU source to pod-visible VFIO → PCI devices

Replace the existing actor-spec-reading resolver with one that enumerates the **worker pod's** granted VFIO groups and resolves them to cloud-hypervisor passthrough devices.

**Files:**
- Modify (rewrite): `cmd/ateom-microvm/gpu.go`
- Modify (rewrite): `cmd/ateom-microvm/gpu_test.go`

**Interfaces:**
- Produces: `func resolveWorkerGPUs() ([]ch.DeviceConfig, error)` — the pod's GPU passthrough devices (nil if the worker has no GPU). Consumed by Task 2.
- Produces (unchanged, kept): package-level `var iommuGroupsDir = "/sys/kernel/iommu_groups"` and `func pciDevicesInIOMMUGroup(group string) ([]string, error)`.
- Produces: package-level `var vfioDevRoot = "/dev/vfio"` (overridable in tests).

- [ ] **Step 1: Rewrite the test** (`cmd/ateom-microvm/gpu_test.go`)

```go
//go:build linux

// <copy the Apache license header block from gpu.go>

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeGroup(t *testing.T, root, group string, addrs ...string) {
	t.Helper()
	dir := filepath.Join(root, group, "devices")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if err := os.WriteFile(filepath.Join(dir, a), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// mkVfio creates a fake /dev/vfio containing the given group nodes plus the
// "vfio" control node.
func mkVfio(t *testing.T, groups ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "vfio"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if err := os.WriteFile(filepath.Join(root, g), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveWorkerGPUs_MultiFunctionAndMultiGPU(t *testing.T) {
	groupsRoot := t.TempDir()
	iommuGroupsDir = groupsRoot
	t.Cleanup(func() { iommuGroupsDir = "/sys/kernel/iommu_groups" })
	// Group 42: GPU (.0) + audio (.1); group 7: a second GPU.
	writeGroup(t, groupsRoot, "42", "0000:01:00.1", "0000:01:00.0")
	writeGroup(t, groupsRoot, "7", "0000:02:00.0")

	vfioDevRoot = mkVfio(t, "42", "7")
	t.Cleanup(func() { vfioDevRoot = "/dev/vfio" })

	got, err := resolveWorkerGPUs()
	if err != nil {
		t.Fatalf("resolveWorkerGPUs: %v", err)
	}
	want := []string{
		"/sys/bus/pci/devices/0000:01:00.0/",
		"/sys/bus/pci/devices/0000:01:00.1/",
		"/sys/bus/pci/devices/0000:02:00.0/",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d devices %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Errorf("device[%d] = %q, want %q", i, got[i].Path, want[i])
		}
		if got[i].Iommu {
			t.Errorf("device[%d] Iommu = true, want false", i)
		}
	}
}

func TestResolveWorkerGPUs_NoVfioDir(t *testing.T) {
	vfioDevRoot = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { vfioDevRoot = "/dev/vfio" })
	got, err := resolveWorkerGPUs()
	if err != nil {
		t.Fatalf("resolveWorkerGPUs: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil devices on non-GPU worker, got %v", got)
	}
}

func TestResolveWorkerGPUs_OnlyControlNode(t *testing.T) {
	vfioDevRoot = mkVfio(t) // only /dev/vfio/vfio, no groups
	t.Cleanup(func() { vfioDevRoot = "/dev/vfio" })
	got, err := resolveWorkerGPUs()
	if err != nil {
		t.Fatalf("resolveWorkerGPUs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no devices, got %v", got)
	}
}

func TestResolveWorkerGPUs_GroupPresentButUnresolvable(t *testing.T) {
	// A numeric group node exists in /dev/vfio, but there is no matching
	// iommu_groups entry -> fail closed.
	iommuGroupsDir = t.TempDir() // empty: group "9" won't resolve
	t.Cleanup(func() { iommuGroupsDir = "/sys/kernel/iommu_groups" })
	vfioDevRoot = mkVfio(t, "9")
	t.Cleanup(func() { vfioDevRoot = "/dev/vfio" })

	if _, err := resolveWorkerGPUs(); err == nil {
		t.Fatal("expected error when a granted VFIO group cannot be resolved")
	}
}
```

- [ ] **Step 2: Run the tests, verify they fail**

Run (darwin): `docker run --rm -v "$PWD":/src -w /src golang:1.26 go test ./cmd/ateom-microvm/ -run TestResolveWorkerGPUs -v`
Expected: FAIL — `resolveWorkerGPUs`, `vfioDevRoot` undefined.

- [ ] **Step 3: Rewrite `cmd/ateom-microvm/gpu.go`**

Replace the entire file body (keep the license header) with:

```go
//go:build linux

// <keep the existing Apache license header block>

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
)

// vfioDevRoot is the pod-visible VFIO device dir. The GPU device plugin injects
// one /dev/vfio/<group> node per granted GPU IOMMU group into the worker pod.
// A var so tests can point it at a fixture.
var vfioDevRoot = "/dev/vfio"

// iommuGroupsDir lists each IOMMU group's member PCI devices. A var for tests.
var iommuGroupsDir = "/sys/kernel/iommu_groups"

// resolveWorkerGPUs enumerates the VFIO groups granted to this worker pod and
// resolves each to the cloud-hypervisor passthrough devices for every PCI
// function in that group. Because a GPU worker hosts exactly one actor, all of
// the pod's GPUs go to that actor's VM. Results are deduped by PCI address and
// sorted for a stable vm.create body.
//
// Returns (nil, nil) when the worker has no GPU (no /dev/vfio, or only the
// control node). Returns an error when a granted group node is present but
// cannot be resolved (fail closed).
func resolveWorkerGPUs() ([]ch.DeviceConfig, error) {
	entries, err := os.ReadDir(vfioDevRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // no /dev/vfio -> non-GPU worker
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", vfioDevRoot, err)
	}
	seen := map[string]bool{}
	var addrs []string
	for _, e := range entries {
		group := e.Name()
		if _, err := strconv.Atoi(group); err != nil {
			continue // skip the "vfio" control node and any non-group entries
		}
		pci, err := pciDevicesInIOMMUGroup(group)
		if err != nil {
			return nil, fmt.Errorf("resolving VFIO group %s: %w", group, err)
		}
		for _, addr := range pci {
			if !seen[addr] {
				seen[addr] = true
				addrs = append(addrs, addr)
			}
		}
	}
	sort.Strings(addrs)
	devs := make([]ch.DeviceConfig, 0, len(addrs))
	for _, addr := range addrs {
		devs = append(devs, ch.DeviceConfig{Path: filepath.Join("/sys/bus/pci/devices", addr) + "/"})
	}
	return devs, nil
}

// pciDevicesInIOMMUGroup lists the PCI addresses (BDF, e.g. 0000:01:00.0) bound
// into an IOMMU group via /sys/kernel/iommu_groups/<group>/devices.
func pciDevicesInIOMMUGroup(group string) ([]string, error) {
	dir := filepath.Join(iommuGroupsDir, group, "devices")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	addrs := make([]string, 0, len(entries))
	for _, e := range entries {
		addrs = append(addrs, e.Name())
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("IOMMU group %s has no devices", group)
	}
	return addrs, nil
}
```

- [ ] **Step 4: Run the tests, verify they pass**

Run (darwin): `docker run --rm -v "$PWD":/src -w /src golang:1.26 go test ./cmd/ateom-microvm/ -run TestResolveWorkerGPUs -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/ateom-microvm/gpu.go cmd/ateom-microvm/gpu_test.go
git commit -m "feat(ateom-microvm): resolve worker-pod VFIO GPUs to CH devices"
```

---

### Task 2: Wire resolved GPUs into vm.create and track on the actor

Change `Run`'s device source from the old resolver to `resolveWorkerGPUs`, and record the GPU count on `runningActor` for the snapshot gate (Task 3).

**Files:**
- Modify: `cmd/ateom-microvm/run.go` (call site ~289; `buildVMConfig` ~449; `runningActor` struct ~44 and its construction ~360)
- Verify: `cmd/ateom-microvm/internal/ch/createvm.go` (the `Devices []DeviceConfig` field + `DeviceConfig` struct already exist)

**Interfaces:**
- Consumes: `resolveWorkerGPUs()` from Task 1.
- Produces: `runningActor.gpuCount int` — number of passthrough PCI functions on the VM. Consumed by Task 3.

- [ ] **Step 1: Add the `gpuCount` field to `runningActor`**

In `cmd/ateom-microvm/run.go`, add to the `runningActor` struct:

```go
	// gpuCount is the number of VFIO passthrough PCI functions cold-plugged into
	// this actor's VM. >0 means the VM holds a GPU and cannot be snapshotted.
	gpuCount int
```

- [ ] **Step 2: Replace the device source at the `buildVMConfig` call site**

In `cmd/ateom-microvm/run.go`, replace the existing GPU-resolution block (the `collectVFIODevices(ctrs)` call before `buildVMConfig`) with:

```go
	// GPU passthrough: cold-plug the worker pod's granted VFIO GPU(s) into this
	// actor's VM (a GPU worker hosts exactly one actor, so it gets all of them).
	gpuDevices, err := resolveWorkerGPUs()
	if err != nil {
		return nil, fmt.Errorf("while resolving worker GPUs: %w", err)
	}
	vmCfg := buildVMConfig(actorUID, kernel, image, kparams, serialLog, memMiB, vcpus, gpuDevices)
```

- [ ] **Step 3: Record `gpuCount` when constructing the `runningActor`**

In `cmd/ateom-microvm/run.go`, at the `ra := &runningActor{...}` construction (~360), add `gpuCount: len(gpuDevices),` to the struct literal.

- [ ] **Step 4: Build for the real target**

Run: `GOOS=linux go build ./cmd/ateom-microvm/...`
Expected: builds clean.

- [ ] **Step 5: Add a test that `buildVMConfig` places the GPU devices**

Append to `cmd/ateom-microvm/gpu_test.go`:

```go
func TestBuildVMConfigIncludesGPUDevices(t *testing.T) {
	devs := []ch.DeviceConfig{{Path: "/sys/bus/pci/devices/0000:01:00.0/"}}
	cfg := buildVMConfig("uid", "vmlinux", "rootfs.img", "", "/tmp/serial.log", 2048, 2, devs)
	if len(cfg.Devices) != 1 || cfg.Devices[0].Path != devs[0].Path {
		t.Fatalf("buildVMConfig Devices = %v, want %v", cfg.Devices, devs)
	}
}
```

Add the `ch` import to the test file if not present:
`"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"`

- [ ] **Step 6: Run tests, verify pass**

Run (darwin): `docker run --rm -v "$PWD":/src -w /src golang:1.26 go test ./cmd/ateom-microvm/ -run 'TestResolveWorkerGPUs|TestBuildVMConfig' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/ateom-microvm/run.go cmd/ateom-microvm/gpu_test.go
git commit -m "feat(ateom-microvm): cold-plug worker GPUs into vm.create, track on actor"
```

---

### Task 3: Snapshot gate — reject Full-scope snapshot for a GPU actor

cloud-hypervisor cannot snapshot a VM holding a VFIO device. Reject Full-scope snapshots at the ateom boundary with a clear error.

**Files:**
- Modify: `cmd/ateom-microvm/checkpoint.go` (the Full-scope snapshot entry point)
- Modify/Add test: `cmd/ateom-microvm/checkpoint_test.go`

**Interfaces:**
- Consumes: `runningActor.gpuCount` from Task 2.

- [ ] **Step 1: Locate the Full-scope snapshot path**

Run: `grep -n 'SNAPSHOT_SCOPE_FULL\|func .*Checkpoint\|Scope\|Snapshot' cmd/ateom-microvm/checkpoint.go`
Note the function that handles a Full-scope checkpoint and how it accesses the `runningActor` (call it `ra`).

- [ ] **Step 2: Write the failing test** (`cmd/ateom-microvm/checkpoint_test.go`)

```go
//go:build linux

// <copy the Apache license header block>

package main

import (
	"strings"
	"testing"
)

func TestFullSnapshotRejectedForGPUActor(t *testing.T) {
	ra := &runningActor{gpuCount: 1}
	err := errIfGPUFullSnapshot(ra)
	if err == nil {
		t.Fatal("expected Full snapshot of a GPU actor to be rejected")
	}
	if !strings.Contains(err.Error(), "GPU") {
		t.Errorf("error should mention GPU, got %q", err.Error())
	}
}

func TestFullSnapshotAllowedForNonGPUActor(t *testing.T) {
	if err := errIfGPUFullSnapshot(&runningActor{gpuCount: 0}); err != nil {
		t.Fatalf("non-GPU actor should be snapshottable, got %v", err)
	}
}
```

- [ ] **Step 3: Run test, verify it fails**

Run (darwin): `docker run --rm -v "$PWD":/src -w /src golang:1.26 go test ./cmd/ateom-microvm/ -run TestFullSnapshot -v`
Expected: FAIL — `errIfGPUFullSnapshot` undefined.

- [ ] **Step 4: Add the guard and call it**

In `cmd/ateom-microvm/checkpoint.go`, add:

```go
// errIfGPUFullSnapshot rejects a Full-scope snapshot of a VM that holds a VFIO
// GPU: cloud-hypervisor cannot snapshot passthrough-device state, so GPU actors
// are run-only.
func errIfGPUFullSnapshot(ra *runningActor) error {
	if ra.gpuCount > 0 {
		return status.Errorf(codes.FailedPrecondition,
			"cannot take a Full-scope snapshot of a GPU actor (%d passthrough device(s)); GPU actors are run-only", ra.gpuCount)
	}
	return nil
}
```

Then, at the start of the Full-scope checkpoint path identified in Step 1, add:

```go
	if err := errIfGPUFullSnapshot(ra); err != nil {
		return err
	}
```

Ensure the `status` and `codes` imports are present (they are already used elsewhere in the package: `google.golang.org/grpc/status`, `google.golang.org/grpc/codes`). Add them to `checkpoint.go` if missing.

- [ ] **Step 5: Run tests + build, verify pass**

Run (darwin): `docker run --rm -v "$PWD":/src -w /src golang:1.26 sh -c "go build ./cmd/ateom-microvm/... && go test ./cmd/ateom-microvm/ -run TestFullSnapshot -v"`
Expected: build clean, both tests PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/ateom-microvm/checkpoint.go cmd/ateom-microvm/checkpoint_test.go
git commit -m "feat(ateom-microvm): reject Full-scope snapshot for GPU actors"
```

> **Follow-up (out of scope for this task):** mirror this gate in ateapi (reject Full-scope suspend for GPU workers), matching the existing `gate-gpu-full-suspend` branch. Track separately.

---

### Task 4: Config artifacts — `microvm-gpu` SandboxConfig, GPU WorkerPool, demo

Declare the GPU guest (UVM asset swap) and a GPU worker pool. No Go code.

**Files:**
- Create: `demos/counter/counter-microvm-gpu.yaml.tmpl` (or the repo's demo location — mirror `demos/counter/counter-microvm.yaml.tmpl`)

**Interfaces:** none (Kubernetes manifests).

- [ ] **Step 1: Copy the existing microvm demo as a starting point**

Run: `cp demos/counter/counter-microvm.yaml.tmpl demos/counter/counter-microvm-gpu.yaml.tmpl`
Then open both to align field names (`SandboxConfig.spec.assets`, `WorkerPool.spec`).

- [ ] **Step 2: Edit the SandboxConfig to point kernel+image at the UVM**

In the new file, set the `SandboxConfig` (name `microvm-gpu`, `sandboxClass: microvm`) so that under `spec.assets.<arch>`:
- `kata-kernel` → the UVM kernel `AssetFile` (`url` + `sha256`)
- `kata-image` → the UVM rootfs `AssetFile` (`url` + `sha256`)
- `cloud-hypervisor`, `virtiofsd`, `kata-config` → unchanged from `counter-microvm.yaml.tmpl`

Leave the UVM `url`/`sha256` as clearly-marked template values to be filled from the staged UVM assets (they are produced/validated in Task 5, Step 2).

- [ ] **Step 3: Edit the WorkerPool to request a GPU**

In the new file's `WorkerPool`:
- reference the `microvm-gpu` SandboxConfig (via `SandboxConfigName` or default resolution, matching how `counter-microvm.yaml.tmpl` links pool→config)
- set `spec.template.resources.limits["nvidia.com/gpu"]: "1"` (and matching `requests`)
- keep `spec.replicas` at the number of GPU workers you want (start with 1)

- [ ] **Step 4: Validate the manifests parse**

Run: `kubectl apply --dry-run=client -f <(BUCKET_NAME=ate-snapshots envsubst < demos/counter/counter-microvm-gpu.yaml.tmpl)`
Expected: `configured (dry run)` for each object, no schema errors.

- [ ] **Step 5: Commit**

```bash
git add demos/counter/counter-microvm-gpu.yaml.tmpl
git commit -m "feat(demos): microvm-gpu SandboxConfig + GPU WorkerPool"
```

---

### Task 5: Compatibility spike (MANUAL — gates Task 6)

Runs on a real GPU node. Produces the concrete values Task 6 needs. This is an investigation, not a code change; record findings in the runbook file.

**Files:**
- Create: `docs/superpowers/runbooks/ateom-microvm-gpu-spike.md`

- [ ] **Step 1: Obtain the UVM assets and record their identity**

Get NVIDIA's Kata GPU image (UVM) kernel + rootfs for amd64. Compute `sha256`, host them where atelet can fetch (the cluster's rustfs/GCS, like other microvm assets), and paste the `url`+`sha256` into `counter-microvm-gpu.yaml.tmpl` (Task 4, Step 2). Record the source + versions in the runbook.

- [ ] **Step 2: Spike 0 — boot the UVM with NO GPU**

Bring up the microvm control plane (per `hack/microvm-assets/README.md`), apply the `microvm-gpu` SandboxConfig, and run a **non-GPU** actor on it. Confirm:
- the VM boots and the kata-agent answers ateom's `CreateSandbox`/`CreateContainer` (the vendored ttrpc API is compatible),
- the overlay rootfs mounts and the container runs,
- a normal (non-GPU) actor lifecycle works.

Record PASS/FAIL. **If FAIL:** the UVM's agent is incompatible → stop; escalate to image Option B (own-built rootfs). Do not proceed to Task 6.

- [ ] **Step 3: Spike 1 — boot the UVM WITH a GPU (T4)**

On a T4 node with the GPU bound to `vfio-pci` (GPU Operator vfio mode), run a GPU actor. Get a shell in the guest (debug console) and record answers to:
1. Do `/dev/nvidia*` nodes exist in the guest after boot (NVRC ran)? Does `nvidia-smi` work **in the guest**?
2. What makes the nodes appear **in the actor container**: the inner-runtime CDI annotation, or `NVIDIA_VISIBLE_DEVICES` env? Record the **exact annotation key/value** the agent honors.
3. Does the agent need a `CreateContainerRequest.devices` entry (`Device{Type:"vfio", ...}`) or does NVRC bus-scan alone suffice?
4. Does the CDI injection set the container device cgroup, or must ateom add the nvidia majors?
5. Does the vfio-manager emit a **host CDI spec** in the pod, or only raw `/dev/vfio` nodes?
6. Any MMIO/BAR errors from cloud-hypervisor at `vm.create`/boot?

- [ ] **Step 4: Commit the runbook with findings**

```bash
git add docs/superpowers/runbooks/ateom-microvm-gpu-spike.md
git commit -m "docs: ateom-microvm GPU compatibility spike findings"
```

---

### Task 6: In-guest node injection via CDI annotation (uses Task 5 findings)

Make the actor container receive `/dev/nvidia*`. Primary path: emit the inner-runtime CDI annotation on the `CreateContainer` OCI spec. Fallback: `NVIDIA_VISIBLE_DEVICES` env. Exact annotation key comes from Task 5, Step 3(2).

**Files:**
- Modify: `cmd/ateom-microvm/internal/kata/overlay_linux.go` (`StartOverlayWorkload`, ~204-248)
- Modify (only if Task 5 finds cgroup not handled by CDI): `cmd/ateom-microvm/spec.go` (`defaultKataResources`)
- Modify: `cmd/ateom-microvm/run.go` (pass a GPU flag into the overlay-workload call)

**Interfaces:**
- Consumes: whether the actor has a GPU (`gpuCount > 0` on the `runningActor`, or a bool threaded to `startOverlayContainer`).

- [ ] **Step 1: Add a GPU-injection constant with the spike's value**

In `cmd/ateom-microvm/internal/kata/overlay_linux.go`, add near the top:

```go
// gpuCDIAnnotationKey / gpuCDIAnnotationValue is the inner-runtime CDI request
// the UVM's kata-agent resolves against NVRC's guest CDI spec to inject
// /dev/nvidia* into the container. Values confirmed by the GPU compatibility
// spike (docs/superpowers/runbooks/ateom-microvm-gpu-spike.md).
const (
	gpuCDIAnnotationKey   = "<FROM SPIKE Task 5 Step 3(2)>"
	gpuCDIAnnotationValue = "<FROM SPIKE Task 5 Step 3(2)>"
)
```

> If Task 5 found env-based injection instead, skip the annotation and set `NVIDIA_VISIBLE_DEVICES=all` in the container OCI env in Step 3 instead of the annotation.

- [ ] **Step 2: Thread a `gpu bool` into the overlay workload start**

In `cmd/ateom-microvm/run.go`, where `startOverlayContainer(...)` is called (in `startActorContainers`), pass whether this actor has a GPU (derive from the same `len(gpuDevices) > 0` computed in Task 2 — thread it through `startActorContainers`/`actorContainer`). In `overlay_linux.go`, add a `gpu bool` parameter to `StartOverlayWorkload`.

- [ ] **Step 3: Emit the annotation when `gpu` is true**

In `StartOverlayWorkload`, after `pbSpec := SpecToAgentPB(spec)` and before `CreateContainer`, add:

```go
	if gpu {
		if pbSpec.Annotations == nil {
			pbSpec.Annotations = map[string]string{}
		}
		pbSpec.Annotations[gpuCDIAnnotationKey] = gpuCDIAnnotationValue
	}
```

If Task 5 Step 3(3) found a `Device` entry is required, also set `Devices: []*agentpb.Device{{Type: "vfio", ...}}` on the `CreateContainerRequest` (fields per the spike; the request already has a `devices` field).

- [ ] **Step 4: (Conditional) add nvidia majors to the container cgroup**

Only if Task 5 Step 3(4) found CDI does NOT set the cgroup: in `cmd/ateom-microvm/spec.go` `defaultKataResources`, append allow-entries for the nvidia device majors recorded in the spike (e.g. `dev("c", 195, -1, "rwm")` for `/dev/nvidia*`, plus the dynamic `nvidia-uvm` major). Use the exact majors from the spike, not guesses.

- [ ] **Step 5: Build**

Run: `GOOS=linux go build ./cmd/ateom-microvm/...`
Expected: builds clean.

- [ ] **Step 6: Verify in-guest (manual, on the T4 node)**

Run a GPU actor via the `microvm-gpu` demo. Exec into the actor container and run `nvidia-smi`.
Expected: the GPU is listed. If not, re-check the annotation key/value against the spike and the fallback env path.

- [ ] **Step 7: Commit**

```bash
git add cmd/ateom-microvm/internal/kata/overlay_linux.go cmd/ateom-microvm/run.go cmd/ateom-microvm/spec.go
git commit -m "feat(ateom-microvm): inject /dev/nvidia* into GPU actor via CDI annotation"
```

---

### Task 7: End-to-end acceptance runbook + demo script (MANUAL)

Prove the whole path and capture it as a repeatable runbook (GPU analog of the counter demo).

**Files:**
- Create: `docs/superpowers/runbooks/ateom-microvm-gpu-e2e.md`
- Optional: `hack/run-microvm-gpu-demo.sh` (mirror `hack/run-microvm-demo.sh`)

- [ ] **Step 1: Write the E2E runbook**

Document, as ordered shell steps: provision a T4 GPU node; GPU Operator in `sandboxWorkloads`/vfio mode; stage UVM assets; apply `counter-microvm-gpu.yaml.tmpl`; create a GPU actor from the template; exec `nvidia-smi` in the actor; (optional) run a tiny CUDA vector-add. Record expected output.

- [ ] **Step 2: Run it end to end and paste real output**

Execute the runbook on the GPU node. Confirm `nvidia-smi` shows the T4 inside the actor container. Paste the actual output into the runbook as the acceptance evidence.

- [ ] **Step 3: Commit**

```bash
git add docs/superpowers/runbooks/ateom-microvm-gpu-e2e.md hack/run-microvm-gpu-demo.sh
git commit -m "docs: ateom-microvm GPU end-to-end runbook + demo"
```

---

### Task 8 (OPTIONAL follow-up): CDI-sourced GPU discovery

Upgrade the GPU source from raw `/dev/vfio` enumeration (Task 1) to parsing the host CDI spec, matching the gVisor path. Only pursue if Task 5 Step 3(5) confirmed the vfio-manager emits a host CDI spec.

**Files:**
- Create: `cmd/ateom-microvm/internal/cdi/cdi.go` (+ `_test.go`) — a minimal reader that extracts `/dev/vfio/<group>` device nodes from a CDI spec, or a factored copy of the gVisor CDI reader.
- Modify: `cmd/ateom-microvm/gpu.go` — prefer CDI-listed groups; fall back to `resolveWorkerGPUs`'s raw enumeration when no CDI spec is present.

- [ ] **Step 1:** Locate the gVisor CDI reader (`cmd/ateom-gvisor/gpu.go` on the GPU branches). If importable/shareable, factor it to `internal/cdi`; else write a minimal CDI JSON reader per the observed spec shape from Task 5.
- [ ] **Step 2:** TDD the reader against a sample vfio CDI spec fixture (device → `/dev/vfio/<group>`), then map group → PCI BDF via the existing `pciDevicesInIOMMUGroup`.
- [ ] **Step 3:** Switch `resolveWorkerGPUs` to try CDI first, raw enumeration second; keep all Task 1 tests green.
- [ ] **Step 4:** Commit.

---

## Self-Review

**Spec coverage:**
- Config track (UVM SandboxConfig, GPU WorkerPool) → Task 4. ✓
- Cluster track (Operator vfio mode = ops in Task 5; `nvidia.com/gpu` passthrough = already flows, no code) → Task 4/5. ✓ (No validation-flip task: this branch has no gVisor-only restriction to flip; `nvidia.com/gpu` on a microvm pool already passes through. If a gVisor-only restriction lands before merge, add a one-line allowlist for `microvm`.)
- Code: GPU source → Task 1; into the VM → Task 2; snapshot gate → Task 3; in-guest injection (3a/3b/3c) → Task 6; CDI-source (item 1 preferred form) → Task 8. ✓
- Error handling (fail closed) → Task 1 (unresolvable group errors) + Task 2 (error propagation). ✓
- Testing + spike → Tasks 1–3 unit tests, Task 5 spike, Task 7 E2E. ✓

**Placeholder scan:** The only intentional deferred values are the UVM `url`/`sha256` (Task 4/5) and the CDI annotation key/value (Task 6) — both are explicit outputs of the manual spike (Task 5) and are marked as such, not vague requirements. No "TBD/add error handling/similar-to" placeholders in code steps.

**Type consistency:** `resolveWorkerGPUs() ([]ch.DeviceConfig, error)` (Task 1) is consumed identically in Task 2; `runningActor.gpuCount` defined in Task 2 is consumed in Task 3 (`errIfGPUFullSnapshot`) and Task 6. `buildVMConfig` signature `(id, kernel, image, kparams, serialLog string, memMiB, vcpus int, gpuDevices []ch.DeviceConfig)` matches its Task 2 call site and the Task 2 test. `ch.DeviceConfig{Path, Iommu}` matches the existing struct.
