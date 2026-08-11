# Design: GPU passthrough for ateom-microvm (end-to-end)

**Date:** 2026-08-06
**Branch:** `eliranw/cloud-hypervisor-gpu-support`
**Status:** Design approved; pending spec review → implementation plan.

## Goal

A microVM actor running on a GPU worker pool can run `nvidia-smi` (and CUDA) end
to end, delivered as one PR. "End to end" means a real actor on a real GPU node,
not just unit-tested plumbing.

## Non-goals (this PR)

- Checkpoint/restore of a GPU actor. cloud-hypervisor cannot snapshot a VM
  holding a VFIO device, and VFIO state does not migrate. GPU actors are
  **run-only**; a snapshot gate rejects the attempt cleanly.
- GPUDirect / NVLink P2P tuning across multiple GPUs in one actor
  (`x_nv_gpudirect_clique`). Independent multi-GPU works; P2P-fabric is deferred.
- Large-BAR / 64-bit MMIO aperture tuning for big datacenter cards (A100/H100).
  First target is a small-BAR card (T4). Big-card BAR sizing is a known
  follow-up.
- Building our own driver-carrying guest image. We reuse NVIDIA's prebuilt Kata
  GPU image (see Image strategy). Own-build is a possible fast-follow.

## Background: current state

The `microvm` runtime (`cmd/ateom-microvm`, kata + cloud-hypervisor) boots a
**stock upstream Kata guest** assembled from kata-static: `vmlinux`
(`kata-kernel`) + `rootfs.img` (`kata-image`) + `configuration-clh.toml`
(`kata-config`), plus `cloud-hypervisor` and `virtiofsd`. ateom drives the
kata-agent directly over ttrpc (no kata shim). Actor workloads run as containers
*inside* that one guest via the agent + an overlay rootfs (virtio-fs RO lower +
guest-tmpfs upper).

Substrate has **no GPU concept today**: no accelerator resource type, no VFIO
injection into the actor OCI bundle, no in-guest driver. The existing gVisor GPU
work (separate branches) requests GPUs via the standard `nvidia.com/gpu`
resource on `WorkerPool.Resources`, gates it to gVisor-only pools, and consumes
a host CDI spec (parsed without the CDI library). That gVisor path relies on
gVisor sharing the host kernel + nvproxy — a model that does **not** cross the
VM boundary.

The actor→worker model is **one actor per worker**, enforced by the control
plane: `AssignWorkerStep.findFreeWorker`
(`cmd/ateapi/internal/controlapi/workflow_resume.go`) assigns an actor to a free
worker and fails with "no free workers available" otherwise; workers are freed
on pause/suspend. This is sandbox-class-agnostic, so GPU inherits it for free.

## Image strategy: reuse NVIDIA's UVM (decided)

The microVM needs a guest with NVIDIA kernel modules built + signed against *its*
kernel, plus NVRC (the NVIDIA guest-side runtime that scans the PCI bus, loads
modules, creates `/dev/nvidia*`, and generates a guest-side CDI spec). We reuse
**NVIDIA's prebuilt Kata GPU image ("UVM")** — their kata kernel + NVIDIA rootfs,
with NVRC and a CDI-capable kata-agent baked in.

Because the current guest is *already* a Kata guest, adopting the UVM is a
**SandboxConfig asset swap, not code**: a new `microvm-gpu` SandboxConfig whose
`kata-kernel` + `kata-image` `AssetFile`s (URL + SHA256) point at the UVM instead
of kata-static. ateom's boot path already fetches whatever those asset names
resolve to. The one risk — does our vendored kata-agent ttrpc API match the
agent inside the UVM — is retired by Spike 0 (below).

Why not build our own rootfs (Option B): it is a build-pipeline project
(chroot, compile + sign modules against our kernel, package) and would sink the
E2E PR on toolchain work. Its long-term value (keeping our own tuned guest,
agent-version control, provenance, size) is real but orthogonal to shipping E2E.

## Topology

GPU is a property of a **dedicated microVM GPU worker pool**, not the standard
guest. Non-GPU actors are unchanged.

- Each GPU worker pod requests `nvidia.com/gpu: K` (via
  `WorkerPool.Template.Resources`). k8s + the GPU Operator's vfio-manager grant
  the pod K GPUs, exposing `/dev/vfio/<group>` (and a host CDI spec) into the
  pod.
- The GPU `ActorTemplate` sets `WorkerSelector` → the GPU pool. Existing
  `findFreeWorker` routes it there and gives **one actor per GPU worker**
  automatically.
- That one actor receives **all K** of its worker pod's GPUs.
- A node with 4 GPUs → 4 GPU worker pods → 4 single-actor VMs, each with 1 GPU
  (or fewer pods with more GPUs each). No scheduler changes; no per-worker cap
  needed.

## Architecture: three tracks

### 1. Config track — no Go code
- `microvm-gpu` SandboxConfig (class `microvm`): `kata-kernel` + `kata-image`
  → UVM assets; `cloud-hypervisor`, `virtiofsd`, `kata-config` unchanged
  (kata-config possibly GPU-tuned).
- GPU WorkerPool referencing it: `Template.Resources → nvidia.com/gpu: K`,
  node placement on GPU nodes.

### 2. Cluster track — ops + one tiny code change
- GPU Operator in `sandboxWorkloads`/vfio mode (binds GPUs to `vfio-pci`) — ops.
- Flip the existing validation that rejects `nvidia.com/gpu` on non-gVisor pools
  to also allow `microvm` (`pkg/api/v1alpha1` workerpool validation).

### 3. Code track — the ateom PR

| # | Component | Where | What |
|---|---|---|---|
| 1 | GPU source | `cmd/ateom-microvm/gpu.go` (rework) | `gpu.go` already exists but reads the **actor OCI spec**; rework it to parse the pod's **host CDI spec** (reuse the gVisor CDI reader) → `/dev/vfio/<group>`s → PCI BDFs via `/sys/kernel/iommu_groups`. Fall back to raw pod `/dev/vfio` enumeration if no CDI spec. Dedup + sort. |
| 2 | Into the VM | `internal/ch/createvm.go`, `run.go` | `VmConfig.Devices` ← resolved `DeviceConfig{Path}` (already built). Cold-plug at boot. |
| 3a | Nodes → container | `internal/kata/overlay_linux.go` | Emit the inner-runtime **CDI annotation** on the OCI spec in `CreateContainer` → UVM agent injects `/dev/nvidia*` from NVRC's guest CDI spec. Exact key = Spike 1 output; env (`NVIDIA_VISIBLE_DEVICES`) is the validated fallback. |
| 3b | VFIO device → agent | `internal/kata/overlay_linux.go` | Populate `CreateContainerRequest.devices` with `Device{Type:"vfio"}` — or confirm NVRC bus-scan alone suffices (Spike 1). |
| 3c | Guest cgroup allow | `spec.go` `defaultKataResources` | Allow nvidia majors in the actor container device cgroup — unless CDI injection already sets them (Spike 1). |
| 4 | Snapshot gate | `checkpoint.go` + ateapi | Reject Full-scope snapshot when the VM holds a VFIO device; mirror `gate-gpu-full-suspend`. |

**CDI unifies items 1 and 3a:** the same CDI device reference resolves to a PCI
BDF for the VM passthrough *and* is passed as the annotation for in-guest
injection — one source of truth. Note there are two distinct CDI specs: the
**host** vfio spec (source for item 1) and the **guest** spec NVRC generates
inside the UVM (consumed by the agent for item 3a). We parse the host one and
only reference the guest one via the annotation.

**The CDI→VMM bridge is unavoidable:** CDI describes container device
nodes/mounts, but cloud-hypervisor needs the host PCI sysfs path. So even sourced
from CDI, we map `/dev/vfio/<group>` → PCI BDF via sysfs — the same translation
Kata's runtime does internally.

Net ateom code: items 1, 2 (done), 3a–3c, 4, plus the validation flip. No
boot-path, scheduler, or atelet changes.

## Data flow

**Setup (once per pool):** GPU Operator (vfio mode) binds node GPUs to
`vfio-pci`. GPU WorkerPool pods request `nvidia.com/gpu: K` → k8s grants K GPUs,
injecting `/dev/vfio/<group>` + host CDI spec into the pod. `microvm-gpu`
SandboxConfig points `kata-kernel`/`kata-image` at the UVM.

**Per-actor activation:**
1. **Placement.** GPU `ActorTemplate` (`WorkerSelector` → GPU pool) →
   `ResumeActor` → `findFreeWorker` picks a free GPU worker → ateom `Run`.
2. **Assets.** ateom fetches the UVM kernel + rootfs (asset resolution, no code
   change).
3. **GPU source.** ateom parses the pod's host CDI spec → `/dev/vfio` groups →
   PCI BDFs.
4. **Launch VMM.** `LaunchVMM` (cloud-hypervisor).
5. **Create VM.** `buildVMConfig` → `VmConfig{ UVM kernel/image, Devices = GPU
   BDFs }` → `CreateVM` → `BootVM`. GPUs cold-plugged at boot.
6. **In-guest bring-up (UVM, automatic).** NVRC scans PCI, modprobes NVIDIA
   modules, creates `/dev/nvidia*`, generates the guest CDI spec.
7. **Sandbox + net.** ateom dials the kata-agent → `CreateSandbox` →
   `configureGuestNetwork` (existing).
8. **Create container.** `CreateContainer` carries the OCI spec + CDI annotation
   (3a) [+ `Device{vfio}` (3b), + nvidia cgroup allow (3c) as Spike 1 dictates].
   Agent resolves the annotation against the guest CDI spec → injects
   `/dev/nvidia*`.
9. **Start.** `StartContainer` → actor runs → `nvidia-smi` works. (Success
   signal.)

**Suspend path:**
10. Suspend/pause → snapshot gate rejects Full-scope for a GPU actor. Pause still
    frees the worker; the actor is run-only this PR.

Steps 1–2, 4–5, 7, 9–10 are existing ateom flow; GPU-specific additions are
step 3, `Devices` in step 5, the annotation/device/cgroup in step 8, and the
gate in step 10. Step 6 is the UVM with no ateom involvement.

## Error handling & failure modes

Principle: **fail closed and loud.** A GPU actor that cannot get its GPU errors
clearly; it never silently boots GPU-less.

| Failure | When | Behavior |
|---|---|---|
| No GPU in pod (not vfio-bound / no `/dev/vfio`) | step 3 | GPU-designated worker with zero resolved devices → hard error at `Run`. Non-GPU worker → zero devices → normal boot. |
| Host CDI spec absent (only raw `/dev/vfio`) | step 3 | Fall back to raw `/dev/vfio` enumeration. Both absent + GPU expected → hard error. |
| Guest driver/NVRC fails (modprobe / driver↔kernel mismatch) | step 6 | No `/dev/nvidia*`, no guest CDI → annotation resolution fails at agent → surface agent error + guest console dump (`DebugConsoleDump`). |
| Agent ignores the CDI annotation (version mismatch) | step 8 | Spike 1 catches pre-PR → fall to env (Option 2). Neither works → UVM-agent incompatibility = blocker (reconsider image). |
| VFIO device busy (2nd actor claims same GPU) | step 5 | Should not happen (1 actor/worker), but CH open fails → fail closed, clear message. |
| Snapshot attempted on GPU actor | step 10 | Gate rejects (`FailedPrecondition`) at ateapi + ateom. |
| Large-BAR/MMIO (big cards) | step 5 | CH boot fails → clear error; known limitation, tuning deferred. |
| Teardown | teardown | CH exit releases the VFIO device to `vfio-pci`; worker keeps its k8s GPU grant; next actor re-resolves. |

**"GPU expected" signal:** inferred from pod grant — if the pod was granted a
GPU (`/dev/vfio` present) but ateom cannot resolve it, that is unambiguously an
error; a pod with no GPU grant is a normal non-GPU worker. No new config flag.

## Testing & the compatibility spike

**Unit tests (CI, no hardware):**
- GPU source resolver: fake host CDI spec + fake `/sys/kernel/iommu_groups`
  fixture → correct PCI BDFs, dedup, control-node skip, multi-GPU (K>1), raw
  fallback. Extends the existing `gpu_test.go`.
- CDI parser (if factored to a shared `internal/cdi`): parse a sample vfio CDI
  spec.
- Snapshot gate: Full-scope rejected with a VFIO device; allowed otherwise.
- Validation flip: `nvidia.com/gpu` accepted on a `microvm` WorkerPool.

**Compatibility spike (pre-PR gate):**
- **Spike 0 — UVM without a GPU.** Publish the UVM as a `microvm` SandboxConfig,
  boot a non-GPU actor. Confirms ateom's overlay + kata-agent ttrpc + snapshot
  flow survive the UVM's agent version. Biggest single risk; testable before any
  GPU work. Fail → reconsider the image.
- **Spike 1 — UVM with a GPU** (T4 on GKE, small BAR). GPU bound to vfio-pci →
  pass through → boot → shell in guest → verify `/dev/nvidia*` (NVRC worked) →
  `nvidia-smi` in guest → then through the actor container (annotation
  injection). Resolves 3a (annotation vs env), 3b (`Device` entry needed), 3c
  (cgroup via CDI or manual), and surfaces MMIO/BAR.

**E2E acceptance (PR success criterion):** on a GPU node, create a GPU
`ActorTemplate` → actor lands on a free GPU worker → the actor container runs
`nvidia-smi` (and ideally a tiny CUDA sanity workload). Scripted as a GPU analog
of `hack/run-microvm-demo.sh`.

**CI reality:** unit tests only in CI (no GPU hardware). E2E is a documented
manual runbook on a GPU node until GPU CI exists — same posture as the current
microvm counter demo.

## Open items to resolve during implementation

- Exact inner-runtime CDI annotation key the UVM agent honors (Spike 1); env
  fallback ready.
- Whether the `Device{Type:"vfio"}` entry is required or NVRC bus-scan suffices
  (Spike 1).
- Whether CDI injection sets the guest cgroup or we add nvidia majors (Spike 1).
- Whether the vfio-manager emits a host CDI spec in the pod or only raw
  `/dev/vfio` (determines primary vs fallback source).
- Whether the gVisor CDI parser is import-shareable or needs factoring into
  `internal/cdi`.
- UVM asset URLs/SHAs and arch (amd64 for GPU nodes).

## Summary of what ships in the PR

Code: CDI-sourced VFIO passthrough into the VM (`gpu.go` source + `DeviceConfig`
in `vm.create`), CDI-annotation injection into the actor container
(`overlay_linux.go`), guest cgroup allow (`spec.go`), snapshot gate
(`checkpoint.go` + ateapi), and the `nvidia.com/gpu` validation flip. Config:
`microvm-gpu` SandboxConfig + GPU WorkerPool. Validation: unit tests + a manual
E2E runbook, gated by the pre-PR compatibility spike.
