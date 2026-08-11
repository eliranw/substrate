# Design: GPU attach/detach for microVM actors

**Date:** 2026-08-10
**Author:** Eliran Wolff
**Branch:** `eliranw/cloud-hypervisor-gpu-support`
**Status:** Design — hardware-validated, pending implementation
**Scope note:** `main` has **no** micro-VM GPU support; this branch introduces it. This design supersedes the branch's initial run-only scope *before it ships* (see §2.0).
**Supersedes scope of:** `2026-08-06-ateom-microvm-gpu-design.md` (which declared GPU actors run-only)

---

## 1. Executive summary

**`main` has no GPU support for the micro-VM runtime at all.** Adding it is the work of
this branch (`eliranw/cloud-hypervisor-gpu-support`), and nothing here has shipped.

The branch's first implementation was deliberately narrow — cold-plug passthrough with a
gate that rejects snapshots — which would have made GPU actors **run-only**: able to boot
with a GPU, but unable to be snapshotted, forked from a golden image, or suspended and
resumed. That excludes them from substrate's core capabilities.

This design **supersedes that scope before it ships**, so GPU actors are first-class from
the start. It does so by decoupling *when the VM's memory image is frozen* from *when the
GPU is attached*: the GPU is present only while the actor is running, and every snapshot
happens in a GPU-less window.

The complete cycle has been **proven on real hardware** (bare-metal Tesla T4,
cloud-hypervisor v52):

```
GPU attached → quiesce → guest unbind → vm.remove-device → vm.pause → vm.snapshot
  → vm.restore → vm.resume → vm.add-device → re-bind → nvidia-smi ✅
```

**Delivered:** golden snapshots, fast fork, and suspend/resume for GPU actors.
**Not delivered:** preservation of live CUDA contexts and VRAM across suspend.

---

## 2. Background

### 2.0 What already exists on this branch

`main` has **no** micro-VM GPU support — `cmd/ateom-microvm/devices.go` does not exist there.
Everything below is unmerged branch work, unit-tested but **never yet run end-to-end as an
actor**. This design builds on it rather than replacing it:

| Commit | What it added | Still correct under this design? |
|---|---|---|
| `dbeb3186` | Resolve the worker pod's VFIO GPUs; cold-plug into `vm.create` | ✅ — this is the boot-with-GPU path (D7) |
| `3c7375f4` | Source the allocated GPU from `PCI_RESOURCE_NVIDIA_COM_*`, not `/dev/vfio` (the worker is privileged and sees every host group) | ✅ |
| `cf482fd6`, `954c5eea` | Snapshot gate + closing two bypasses (restored actors, nil actor state) | ✅ — now an **invariant** (§4.2) rather than a limitation |
| `4c69a79a`, `0c0017ef`, `a420798a` | `microvm-gpu` SandboxConfig, GPU WorkerPool, `assemble-gpu.sh` | ✅ — asset URLs change with the §12 image bump |
| `04307805`, `49ab2da3` | `kata-initrd` asset + boot path (the 535 UVM ships an initrd) | ⚠️ — becomes unnecessary if we bump to 595, which ships a **disk image** (`kata-image`, the pre-existing path). Keep as the fallback for the old image |

**What this design adds:** detach-before-snapshot, attach-on-resume, and the golden-actor
sequencing — i.e. everything that turns the branch's run-only passthrough into full
suspend/resume.

### 2.1 Why the branch's initial scope was run-only

A snapshot of a VM holding a VFIO passthrough device is not merely incomplete — it is
**corrupt**. cloud-hypervisor does *not* refuse it:

- `VfioPciDevice`'s `Pausable` implementation is **empty**, so `vm.pause` stops vCPUs
  but does **not** quiesce the GPU.
- The GPU is a bus master. It continues DMA-ing into guest RAM **while clh writes out
  the memory ranges**, producing a **torn memory image**.
- `vm_snapshot` has no VFIO check, so the call returns success.

Our `errIfPassthroughSnapshot` gate is the only thing preventing this.

Separately, the device's own state cannot be captured: the T4 binds to **generic
`vfio-pci`**, which implements no migration protocol.

### 2.2 Why "get a migration-capable driver" is not the answer

Investigated and rejected (see §7.2 and Appendix B):

- **No NVIDIA GPU offers VFIO migration v2 for full passthrough.** `nvgrace-gpu-vfio-pci`
  (the only in-tree NVIDIA variant driver) contains **zero migration code** and is
  ARM64/Grace-only.
- **`nvidia-vgpu-vfio` (vGPU/mdev) does implement migration v2**, but the vGPU guest
  driver **has never loaded under cloud-hypervisor** — documented failures spanning
  clh v29→v50, drivers 470→580, kernels 5.15 and 6.8, on V100/T4/L40S, in both mdev and
  SR-IOV modes, while the identical configuration works under QEMU. Unowned by either
  vendor for three years.
- Newer hardware (A100/H100/Grace) does not change this; the constraint is the
  driver↔VMM pairing, not the silicon.

### 2.3 Guest-image landscape (why no version constraint remains)

The pinned artifact `nvcr.io/nvidia/cloud-native/kata-gpu-artifacts:ubuntu22.04-535.54.03`
(built 2023-08-02) predates NVRC. Its `/init` is a 448-byte bash script:

```bash
mount_setup
nvidia_get_devices          # just: lspci -d 10de:
nvidia_check_if_supported
# Only if we're cold-plugging we would detect GPUs in this phase,
# skip any GPU related setup if we don't have any GPUs
if [ -n "${gpu_device_ids}" ]; then
        nvidia_toggle_FLR
        nvidia_hook
fi
exec /sbin/init                # AGENT_INIT: kata-agent is PID 1
```

and `nvidia_hook` is three idempotent commands:

```bash
nvidia-persistenced
nvidia-ctk system create-device-nodes --control-devices --load-kernel-modules
nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.json
```

It contains **no `modules_disabled` lockdown**, so bring-up is re-runnable at any point
after boot.

Current NVRC (verified by unpacking the UPX-packed binary from the 595 image) *does*
write 1 to `/proc/sys/kernel/modules_disabled`, and its GPU hot-plug watcher was removed
in Dec 2025.

**Neither fact constrains this design**, because the lockdown blocks
`init_module`/`finit_module`/`delete_module` — **not** sysfs bind/unbind of an
already-resident driver. Since we always boot **with** the GPU (D7), the modules are
loaded before any lockdown is applied, and every subsequent operation is a bind or an
unbind. The design therefore runs unmodified on both image generations, and we prefer the
newest (§12).

---

## 3. What was proven on hardware

Environment: bare-metal Ubuntu 22.04, host kernel 5.15, Intel VT-d enabled (160 IOMMU
groups), 6× Tesla T4 (TU104GL) bound to `vfio-pci` by the GPU Operator in
`sandboxWorkloads`/`vm-passthrough` mode; cloud-hypervisor v52; NVIDIA Kata GPU guest
(kernel `vmlinuz-5.19.2-109-nvidia-gpu` + initrd).

| # | Experiment | Result |
|---|---|---|
| 1 | Cold-plug boot with a T4, `nvidia-smi` in guest | ✅ works |
| 2 | Boot with **no GPU**, `vm.add-device` a T4, run bring-up, `nvidia-smi` | ✅ works (validates hot-attach itself; the GPU-less *boot* strategy was later dropped — D7) |
| 3 | Quiesce (`pkill nvidia-persistenced`) then guest unbind | ✅ returns immediately; **no `usage_count` spin** |
| 4 | `vm.remove-device` → vfio **group** fd released | ✅ `device_tree` and `config.devices` empty |
| 5 | `vm.pause` + `vm.snapshot` after eject | ✅ 204; complete image; `state.json` contains no `vfio` |
| 6 | `vm.restore` → `vm.resume` → `vm.add-device` → re-bind → `nvidia-smi` | ✅ works |

Experiments 3–6 constitute a full suspend/resume round trip.

### 3.1 Mechanics learned (these drive the component design)

- **`nvidia-persistenced` was the only holder of the device.** No auxiliary-module
  teardown was needed. The documented hot-unplug hazards all concern ejecting a **busy**
  GPU; substrate suspends **idle** actors, which is a different and safe regime. The
  detach path always stops the daemon first (D9), exactly as this experiment did.
- **Guest unbind returns `EIO` but succeeds.** The NVIDIA driver's remove path returns
  an error that propagates to the sysfs write after `device_release_driver()` has
  completed. Success must be verified by the **absence of
  `/sys/bus/pci/devices/<BDF>/driver`**, not by exit status.
- **Do not parse `lspci` for bound state.** `Kernel modules:` is a candidate list and is
  always present; `Kernel driver in use:` is the state line. Conflating them is easy and
  was done twice during validation.
- **`vm.remove-device` returns 204 meaning "eject requested."** Real teardown happens
  when the guest executes ACPI `_EJ0`. `config` is cleared up front and will lie.
- **Leftover fds are benign.** After eject, clh retains `anon_inode:kvm-vfio` ×2 and
  `/dev/vfio/vfio` (the container control node). The **group** fd — the one that pins
  the device — is released. These leftovers did **not** block re-add on v52, so no clh
  version bump is required.
- **`vm.add-device` returns 200 with `{"id","bdf"}`.** The id changes across cycles
  (`_vfio0` → `_vfio1`); the guest reuses the same BDF. The returned id is the handle
  required for a later eject and **must be captured**.
- **Bring-up is idempotent and partially persistent.** `/dev/nvidia*` survive the whole
  cycle (`nvidia-ctk` logs "Skipping: … already exists"), so resume is a rebind rather
  than a full setup.

---

## 4. Architecture

**Principle:** the GPU is attached only while the actor is running. No snapshot ever
occurs with a VFIO device attached.

### 4.1 Lifecycle

| Transition | GPU action | Mechanism |
|---|---|---|
| **Golden warmup** | *no special case* (D3) — it is an ordinary actor that boots with a GPU, warms, and is then suspended | the golden snapshot **is** the Suspend row below |
| **Run** (cold) | attach at `vm.create` (cold-plug, already implemented) | `vm.add-device` → bring-up → normal CDI injection at container creation |
| **Resume** (restore) | **attach — always explicit** | `vm.add-device` → **verify the driver re-bound** (`/sys/bus/pci/devices/<BDF>/driver` appears; explicitly bind if not) → done. No container injection: the nodes and cgroup came back with the snapshot (§4.3) |
| **Suspend** | detach before snapshot | verify no holders → unbind → `vm.remove-device` → verify ejected → `vm.pause` → `vm.snapshot` |
| **Teardown** | none | VM exits; fd close releases and resets the device |

### 4.2 Two invariants

1. **Never snapshot with a VFIO device attached.** The existing `errIfPassthroughSnapshot` gate
   remains, upgraded from a limitation to a safety assertion.
2. **Quiesce before eject.** The actor must be idle and `nvidia-persistenced` stopped.
   This keeps us in the regime validated in §3.

### 4.3 Injection — one-time, at container creation

Booting with the GPU (D7) collapses the *timing* half of this problem: the container is
created while the GPU is present, so whatever is injected is injected once and then simply
has to survive (table below). It does **not** collapse the injection itself.

> **Correction (verified at this HEAD).** An earlier draft of this section assumed "normal
> CDI injection at `CreateContainer` supplies everything." That is false for this
> architecture. Kata's CDI injection lives in the **containerd shim**, which ateom-microvm
> bypasses: it builds the OCI spec itself (`cmd/ateom-microvm/spec.go`) and hands it
> straight to kata-agent over vsock. Two consequences, both provable without hardware:
>
> 1. **No injector exists.** `cmd/ateom-microvm/` contains no CDI, no `nvidia-ctk`, and no
>    `/dev/nvidia` handling of any kind. (Contrast the gVisor path, which needed a whole
>    `cmd/ateom-gvisor/gpu.go` to do this — it does not come for free.)
> 2. **The device cgroup actively denies the GPU.** `defaultKataResources`
>    (`spec.go:138-149`) is `{Allow: false, Access: "rwm"}` followed by a fixed allowlist
>    containing no NVIDIA major (195) and no dynamically-assigned `nvidia-uvm` major.
>    `internal/kata/specconv.go:111` copies that list verbatim into the agent request, so
>    the deny reaches the guest. Even if the nodes and libraries were present, an
>    `open("/dev/nvidia0")` inside the actor container would fail.
>
> **C5 is therefore not a fallback — it is required work.** Something must supply the
> driver libraries, the `/dev/nvidia*` nodes, and a widened device cgroup at container
> creation. §12 step 5 still decides whether that injection must be *repeated* after
> re-attach, which is the separate question the table below answers.

**Resolution (verified against kata-containers source).** The guest's kata-agent does
the injection itself, driven by one annotation — so C5 is an annotation, not a device
subsystem:

- `handle_cdi_devices` (`src/agent/src/device/mod.rs:430`) is called unconditionally from
  `create_container` (`src/agent/src/rpc.rs:278`). Present since kata **3.11.0**; we ship
  4.0.0. It scans the OCI spec's annotations for the `cdi.k8s.io/` prefix — the suffix is
  free-form — and injects every CDI device named in the value.
- It reads **only** `/var/run/cdi` in the guest (hardcoded at `rpc.rs:281`; `/etc/cdi` is
  deliberately not scanned because `/etc` may be read-only).
- The spec there is generated **inside the guest, at boot**: NVRC runs
  `nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml` (`nvrc/src/toolkit.rs:31-46`)
  and blocks on it before forking the agent (`main.rs:63` then `main.rs:126`), so there is
  no race with the first `CreateContainer`.
- CDI injection adds the device-cgroup entries too — `container_edits.rs:104-109` calls
  `add_linux_resources_device` for every injected node. **We hand-write neither the nodes
  nor the allowlist**; doing so would duplicate the agent's work with host-side numbers
  that are wrong inside the guest.
- Guest-generated means major/minor are already guest-native, so the host→guest rewriting
  in `update_spec_devices` (`device/mod.rs:703`) does not apply to this path.

Two constraints this imposes on the rest of the design:

1. **Inherited `cdi.k8s.io/` annotations must be stripped.** The host's sandbox device
   plugin advertises kind `nvidia.com/pgpu`, which does not exist in the guest's spec, and
   an unresolvable CDI device *fails* `CreateContainer` rather than being ignored. Kata's
   own shim clears them for exactly this reason (commit `1561d7fb`).
2. **NVRC generates the CDI spec exactly once, at boot** — there is no udev/inotify
   rescan. A GPU attached *after* boot never appears in `/var/run/cdi/nvidia.yaml`. This
   is why the container must be created while the GPU is present (D7), and it means the
   re-attach path must not depend on a fresh CDI resolution. *(Inferred from the code
   paths; not yet observed on hardware.)*

Those artifacts then **survive the whole cycle**:

| Artifact | Lives in | Survives snapshot/restore? |
|---|---|---|
| Driver libraries (`libcuda.so`, `libnvidia-ml.so`) | container mounts | ✅ plain files |
| `/dev/nvidia*` nodes | the container's `/dev` **tmpfs** — i.e. guest RAM | ✅ captured in the memory snapshot |
| Device cgroup allowlist | container cgroup state, in guest RAM | ✅ captured |

A device node is just a `(major, minor)` pair. Detaching the GPU does not delete the
node; it merely makes it refer to a device that is temporarily absent. On re-attach the
driver rebinds and the same major/minor become live again, so the container's existing
nodes start working without being touched. The `nvidia-uvm` major — the one value that is
dynamically assigned — is stable across the cycle because the **modules stay resident**
(D7): nothing is unloaded, so nothing is re-assigned.

**Consequence:** there is no restore-time container injection to perform. No `mknod` into
`/proc/<pid>/root/dev/`, no `UpdateContainer` cgroup widening, no CDI re-application. That
removes the only part of the design that worked *around* the kata-agent rather than
through it.

*This is the one inference in the design not yet directly observed* — our §3 experiment
validated the VM-level cycle, not container-level node reuse across it. §12 step 5 must
confirm a container's pre-existing `/dev/nvidia*` work after re-attach. If they do not,
the fallback is the previous plan: create nodes into `/proc/<pid>/root/dev/` from the
guest and widen the cgroup via `UpdateContainer`, both driven by the guest CDI spec.


### 4.4 Booting the GPU guest — root parameters and PCI

The GPU guest is not a drop-in for the stock one. From upstream's own
`configuration-qemu-nvidia-gpu.toml` (kata-static 4.0.0):

| | stock guest | NVIDIA GPU guest |
|---|---|---|
| `rootfs_type` | `ext4` | **`erofs`** |
| `kernel_params` | `cgroup_no_v1=all systemd.unified_cgroup_hierarchy=1` | **`cgroup_no_v1=all pci=realloc pci=nocrs pci=assign-busses`** |
| root params | `root=/dev/vda1 rootflags=data=ordered,errors=remount-ro ro rootfstype=ext4` | `root=/dev/vda1 rootflags=ro rootfstype=erofs` |

Three things this settles, all verified against kata source rather than inferred:

- **The root parameters are not in `kernel_params`.** Kata assembles them per
  filesystem in `GetKernelRootParams` (`virtcontainers/hypervisor.go:132-216`), so
  passing the config's `kernel_params` through — which ateom already does — does
  not carry them. `rootfs_type` is the only key that says which layout applies,
  which is why ateom now parses it.
- **`root=/dev/vda1` is right for both.** The erofs image is not a raw filesystem
  at offset 0: `create_erofs_rootfs_image` builds an MBR label and `dd`s the
  filesystem into **p1** (`osbuilder/image-builder/image_builder.sh:581-633`).
  Independent arithmetic check: `data_blocks × data_block_size = 123392 × 4096` is
  exactly 482 MiB, consistent with p1 starting at the 1 MiB boundary.
- **dm-verity is optional, and we skip it.** `veritysetup format --no-superblock`
  writes the hash tree to a *separate* p2 and never touches p1
  (`image_builder.sh:511-529`), so p1 is a self-contained mountable filesystem.
  Booting it directly is valid. We skip the verity mapping deliberately: the image
  is fetched under a sha256 pinned in its SandboxConfig and attached read-only, so
  a verity chain would add a `root_hash` that changes on **every image rebuild**
  (the salt is freshly random per `veritysetup` run) without covering a threat
  those two do not. The fallback, if it is ever needed, is
  `dm-mod.create="dm-verity,,,ro,0 <sectors> verity 1 /dev/vda1 /dev/vda2 4096 4096 <data_blocks> 0 sha256 <root_hash> <salt>" root=/dev/dm-0`.

`pci=realloc pci=nocrs pci=assign-busses` deserves note: it is BAR reallocation
and bus reassignment, which is what a passed-through GPU with large BARs needs.
The stock config has none of it because it never sees a passthrough device, so
this is a boot/enumeration difference the guest swap alone would not have caught.

**Open risk (UNVERIFIED).** NVRC is the guest init, not systemd. Whether it
refuses to proceed when root is not a dm-verity device has not been confirmed —
no kata-side code requires it, but NVRC is an external binary. If it does, the
verity cmdline above becomes mandatory and the `root_hash` must be read from the
config shipped with that image build.

---

## 5. Design decisions

### D1. Attach/detach around snapshots, rather than snapshotting with the GPU attached
**Chosen because** no NVIDIA GPU exposes VFIO migration v2 to a generic VMM (§2.2), and
snapshotting with a device attached is actively corrupting (§2.1). Validated in §3.

### D2. Hot-**add** is in scope; hot-**remove** only against an unheld device
Every documented hot-unplug hazard — the `usage_count` infinite spin, Xid 79, "RM API
doesn't support hot plug" — concerns ejecting a **busy** GPU. Substrate suspends idle
actors, and D9 removes the only systematic holder, so we never enter that state at all.
That is stronger than handling the hazard well: the dangerous condition is unreachable.
Proven in §3.

### D3. The golden actor needs no special handling

Under the single strategy (D7), a golden actor is an ordinary actor: it boots with a GPU,
warms up, and is then **suspended** — and the golden snapshot *is* that suspend, taken via
the normal `CheckpointWorkload` path. Because suspend always detaches first (§4.1), the
golden snapshot is device-free for free.

So there is **no golden-actor detection** and no `atespace == ate-golden` branch. The
earlier design needed one only because it treated golden actors differently (never
attaching a GPU); that distinction is gone.

**Consequence:** the golden image contains no warm GPU — the device was detached before
the snapshot. Each actor pays a driver rebind on attach. Accepted; the expensive part of
cold start (kernel boot, agent init, virtio-fs setup, app warmup) is still amortised.

### D4. Container injection happens once, at creation — nothing at restore
Because the container is created with the GPU present (D7), normal CDI injection supplies
libraries, device nodes and the device cgroup through the supported path. All three live
in guest RAM (mounts, `/dev` tmpfs, cgroup state) and are captured by the snapshot, and a
`(major, minor)` node becomes live again when the driver rebinds. See §4.3.

Rejected: (a) snapshotting before containers start — loses app warmth, the substrate
demo's core property; (b) hand-rolled restore-time injection (`mknod` into
`/proc/<pid>/root/dev/` plus `UpdateContainer` cgroup widening) — this was the plan while
the golden actor booted GPU-less, and it is retained only as the fallback if §12 step 5
shows the container's existing nodes do not survive.

### D5. Verify eject by observed state, never by HTTP status
`vm.remove-device` returning 204 means "requested". The completion oracle is the
`vm/device-removed` event **plus** the vfio group fd leaving `/proc/<clh-pid>/fd`, with a
hard deadline. During validation, a naive poll produced a **false success** because both
of its checks were vacuous (grepping a wrong id; an empty PID making `ls` fail silently).
A check that cannot distinguish "absent" from "could not look" is not a check.

### D6. Snapshot gate keys off the worker, not per-actor state
`resolveWorkerDevices()` rather than `runningActor.gpuCount`. One actor per worker and the
actor receives every GPU the pod was granted, so the worker's allocation is the actor's.
This closed two live bypasses (restored actors never set `gpuCount`; a nil
`runningActor` after an ateom restart). Already fixed in commit `954c5eea`.

### D7. One strategy: always boot with the GPU, always detach before snapshotting

**The single requirement on a guest image:** the `nvidia` kernel modules must be
**resident** when we re-bind. Every GPU-booted guest satisfies this by construction, so
the design imposes no version constraint at all — prefer the newest image.

We considered a second strategy — boot the golden actor **GPU-less** and hot-attach later
— and rejected it. It is not merely unnecessary; it is worse on three counts:

| | Boot with GPU, detach (**chosen**) | Boot GPU-less, attach (rejected) |
|---|---|---|
| Post-restore kernel op required | **bind** an already-resident driver (sysfs) — never blocked | **load a kernel module** (`init_module`) — forbidden once `modules_disabled` is set |
| Works on current NVRC images (595) | ✅ | ❌ NVRC takes `cpu` mode, never loads the driver, then locks — a hot-added GPU is inert |
| Warm state for GPU-dependent startup | ✅ app completes its GPU probe/init during warmup | ❌ app warms with **no GPU visible**; anything calling `nvidia-smi` / `torch.cuda.is_available()` warms on a degraded path or fails |
| Code paths | one | two (per-image branch) |

The chosen strategy needs strictly *less* from the kernel — only bind/unbind, never module
loading — which is why it is image-agnostic. §3 proved both halves on hardware.

**Cost, stated plainly:** the golden path gains real machinery (quiesce → unbind → verify
→ eject → verify ejected → pause → snapshot), each step subject to D5's completion-oracle
discipline. The GPU-less variant's only advantage was avoiding that, and it bought it by
restricting us to a 2023 image.

*Historical note:* the GPU-less strategy was the original design, chosen when hot-unplug
looked dangerous (the `usage_count` spin; "the RM API doesn't really support hot plug").
The §3 experiments showed detach is safe against an **idle** device, which removed the
risk that motivated it.

**What the bump buys (verified by inspecting the image):** driver **595.58.03**, kernel
**6.18.35**, Ubuntu Noble, plus DCGM / fabricmanager / NVSwitch / InfiniBand support, and
a driver ≥ r550 (the floor for `cuda-checkpoint`). It ships in the **same
`kata-static-<ver>-amd64.tar.zst`** that `assemble.sh` already downloads, so acquisition
is a file-selection change.

**What it costs:** the guest is an **EROFS disk image with dm-verity** (so the boot
cmdline needs verity parameters rather than plain `root=/dev/vda1`, and we return to the
`kata-image` asset path); NVRC starts `nvidia-persistenced` itself (see D9); and
`cuda-checkpoint` is **still not shipped** (verified absent from the 595 image), so T2
must inject it regardless of image.

### D8. No clh version bump required
v52 retains benign non-group fds after eject but re-add succeeds (§3.1). v53 remains
desirable (second eject-leak fix, hotplug race) but is not a dependency. Bumping would
require revalidating the non-GPU snapshot path, since the virtiofsd pin is coupled to it.

### D9. Stop `nvidia-persistenced` before every detach

`nvidia-persistenced` is the single systematic holder of the GPU and the reason an unbind
would otherwise block. Since we always boot **with** the GPU (D7), the guest's own
bring-up starts it — on the 535 image via `nvidia_hook()` (`init_functions:29`, the only
starter; there is no systemd unit because the guest runs `AGENT_INIT`), and on the 595
image by NVRC itself (`/bin/nvidia-persistenced`).

So the detach path **always stops it first**: `stop persistenced → unbind → verify →
eject`. That is precisely what §3 validated, and the daemon was the *only* holder — no
auxiliary-module teardown was needed, and there was no `usage_count` spin because the
actor is idle at suspend.

**What the daemon buys, and why it is worthless here.** Persistence mode keeps the driver
initialized when *no client* has the GPU open, so the next CUDA process skips
re-initialization. It achieves that by holding the device files open. It is a latency
optimization *between clients* — and this lifecycle **ejects the device at suspend**,
tearing down all driver state regardless. Its benefit is destroyed by the very operation
we are building, while its cost (a blocked unbind) is exactly what we must avoid.
NVIDIA's own internal tracking records the same failure: *"GPU cannot be unplugged
because the driver cannot be unbound while `nvidia-persistenced` still has it open."*

**Also a prerequisite for the future cuda-checkpoint work (§9).** `cuda-checkpoint`
releases *a CUDA process's* references; it does nothing about the daemon. With
persistenced running, a checkpointed process would let go and the unbind would **still**
block. Holders and their releases:

| Holder | Released by | Present when |
|---|---|---|
| `nvidia-persistenced` | stopping the daemon — nothing else | whenever the guest brought up a GPU |
| the actor's CUDA process(es) | process exit, or `cuda-checkpoint` | only with live CUDA work |

**Optional refinement (untested):** the 595 NVRC exposes an `nvrc.uvm.persistence.mode`
kernel-cmdline knob that may suppress the daemon entirely, which would remove this step.
Worth testing during the image bump (§12); the design does not depend on it.

### D10. Verify the driver re-bound; do not assume it

`vm.add-device` puts the device on the guest PCI bus, after which the kernel's driver
core normally auto-binds it against the resident `nvidia` driver's id_table. §3 evidence
supports this: after re-attach we ran only `nvidia-ctk system create-device-nodes`, which
creates nodes and loads modules but does **not** bind drivers — yet `nvidia-smi` worked,
which requires a bound driver.

That is an inference, and D5's discipline applies: poll for
`/sys/bus/pci/devices/<BDF>/driver` to appear, and explicitly bind if it does not within a
deadline. The same sysfs check we use (in reverse) to confirm detach. This turns an
assumption into a guarded fast path at negligible cost.

---

---

## 6. Components

| # | Component | Location | Responsibility |
|---|---|---|---|
| C1 | `ch.AddDevice` / `ch.RemoveDevice` | `internal/ch/` | `vm.add-device` (returns `{id,bdf}` — capture it), `vm.remove-device` |
| C2 | Eject completion oracle | `internal/ch/` | `device-removed` event + group-fd check on the real clh PID, with deadline; fail loudly, never silently |
| C3 | Guest detach | `internal/kata/` | verify no holders (no `nvidia-persistenced` by D9; no live CUDA client); unbind; confirm via **absence** of `/sys/bus/pci/devices/<BDF>/driver` |
| C4 | Guest attach/bring-up | `internal/kata/` | after `add-device`: **verify the driver re-bound** by polling for `/sys/bus/pci/devices/<BDF>/driver`, and explicitly `echo <BDF> > /sys/bus/pci/drivers/nvidia/bind` if it has not within a deadline. PCI rescan if the device did not enumerate. First boot only: the `nvidia-ctk` bring-up commands |
| C5 | container device injection at **create** | `internal/kata/`, `spec.go` | **Required** (§4.3 correction): no CDI injector exists in ateom-microvm and the device cgroup denies NVIDIA majors. Must supply libraries + `/dev/nvidia*` + a widened cgroup. §12 step 5 decides only whether injection must ALSO be repeated after re-attach (`mknod` into `/proc/<pid>/root/dev/` + `UpdateContainer`) |
| C6 | Attach/detach orchestration | `run.go`, `restore.go`, `checkpoint.go` | sequence the above at the right lifecycle points |


The guest-command channel already exists: `DebugConsoleDump(ctx, vsockPath, cmd)` runs a
shell command in the guest over vsock, and the debug console is enabled unconditionally
by `guestConfig`. C3/C4/C5 use it. (Promoting it from a debug facility to a production
dependency is noted as a risk in §8.)

---

## 7. Error handling

| Failure | Detection | Response |
|---|---|---|
| Unbind fails (device still bound) | `/sys/bus/pci/devices/<BDF>/driver` still present after timeout | **Abort the suspend.** Leave the actor running. Never eject a bound device. Most likely cause: a live CUDA client, or `nvidia-persistenced` started contrary to D9. |
| Eject not completed within deadline | oracle (C2) times out | **Do not retry `remove-device`** and **do not snapshot.** Quarantine the worker; destroy the VM. |
| `add-device` fails on resume | non-200, or GPU absent from guest bus after rescan | Fail the resume; the actor is unusable without its GPU. Surface the clh error. |
| Bring-up fails (driver won't rebind) | no `/dev/nvidia*`; `nvidia-smi` error | Fail the resume with the guest console output attached. |
| Container injection fails | `mknod` or `UpdateContainer` error | Fail the resume. A partially-injected container is worse than a failed one. |
| Snapshot attempted with a GPU attached | `errIfPassthroughSnapshot` | Reject with `FailedPrecondition`. This is a bug in the caller — the detach should have run. |

**Principle:** every failure in the detach path aborts the suspend and leaves the actor
running; every failure in the attach path fails the resume loudly. Silent degradation to
a GPU-less actor is never acceptable.

---

## 8. Risks

| Risk | Severity | Notes |
|---|---|---|
| VFIO attach DMA-pins all guest memory, defeating `OnDemand`/userfaultfd fast restore (~75 ms → ~1.8 s) | **High** | The main threat to the value of fast fork. Measure before committing to the full implementation. |
| Debug console promoted to a production control path | Medium | 8 s timeout, interactive PTY, no exit codes. Consider hardening or moving to a purpose-built agent call. |
| Guest image pin (D7) | Medium | A future driver bump (e.g. r550+ for cuda-checkpoint) likely means an own-built rootfs; it must preserve re-runnable bring-up. |
| Quiesce assumption (actor idle) | Medium | If an actor holds live CUDA work at suspend, unbind may block. Preflight with `nvidia-smi drain` / verify, and abort rather than force. |
| Guest kernel is a QEMU-targeted NVIDIA build | Low | ACPI hot-add worked in validation, but it is not a configuration NVIDIA exercises. |

---

## 9. Out of scope

- **Preserving live CUDA contexts and VRAM across suspend.** Detach destroys them; on
  resume the application must re-initialize CUDA. This requires `cuda-checkpoint`
  (driver **r550+**; our UVM ships 535) and therefore an own-built guest rootfs.
  Adequate for agent workloads idle between turns; **not** adequate for suspending a
  mid-step training job.
  Note that **D9 is a prerequisite for this future work**, not a shortcut for the
  present design: `cuda-checkpoint` releases a CUDA *process's* references and does
  nothing about `nvidia-persistenced`, so with the daemon running the unbind would still
  block after a successful checkpoint.
- vGPU / mdev (§2.2).
- Retiring the `kata-initrd` asset path. It is required for the 535 image and becomes
  redundant if we bump to 595 (a disk image), but removing it is a separate cleanup.
- Multi-GPU P2P/NVLink tuning (`x_nv_gpudirect_clique`).
- Large-BAR / 64-bit MMIO tuning for datacenter cards.
- Returning the GPU to Kubernetes on detach — the device plugin reserves the GPU to the
  **pod** for its lifetime; releasing it inside the VM does not return it to the cluster.

---

## 10. Testing

**Unit (CI, no hardware):** GPU source resolution; eject-oracle logic including the
timeout and the "cannot observe" case; CDI-spec parsing → device list; golden-actor
detection; snapshot gate.

**Integration (GPU node, manual):** the §3 experiment sequence, scripted as a runbook —
cold-plug boot; GPU-less boot + attach; quiesce/unbind/eject; snapshot; restore + attach;
`nvidia-smi`.

**Acceptance:** a GPU `ActorTemplate` reaches `Ready` (golden snapshot succeeds); an actor
forked from it runs `nvidia-smi`; the actor is suspended and resumed and `nvidia-smi`
still works.

**Performance:** restore latency with and without a GPU attach, to quantify the DMA-pin
risk in §8.

---

## 11. Open questions

1. Measured cost of the DMA re-pin on a userfaultfd-restored VM (§8).
2. Whether the debug console is acceptable long-term as the guest control path, or
   whether bring-up should move into a purpose-built agent RPC.
3. Whether the control plane should signal "warmup" explicitly rather than ateom
   inferring it from `atespace` (D3).

---

## 12. Work item: bump the guest image to kata-static 4.0.0

Per D7, prefer the newest image that satisfies the residency requirement. This is
first-class work, not a risk to avoid — staying on the 2023 image caps us at driver
535 / kernel 5.19 and excludes Hopper/Blackwell.

**Acquisition.** The NVIDIA GPU guest ships inside the *same* tarball `assemble.sh`
already downloads:

```
kata-static-4.0.0-amd64.tar.zst
  opt/kata/share/kata-containers/kata-ubuntu-noble-nvidia-gpu-595.58.03.image
  opt/kata/share/kata-containers/vmlinux-6.18.35-200-nvidia-gpu   (vmlinux-nvidia-gpu.container)
  opt/kata/share/kata-containers/root_hash_nvidia-gpu.txt         (dm-verity)
```

So `assemble-gpu.sh` changes from an `oras` pull of the NGC artifact to selecting
different members of the existing download. The guest becomes a **disk image**, so the
`kata-image` asset path applies and `kata-initrd` becomes the fallback for the old image.

**Verify on 595 before switching (in this order — stop at the first failure):**

1. Boot the 595 guest under cloud-hypervisor **with a GPU cold-plugged**; confirm NVRC
   reaches `gpu` mode and `nvidia-smi` works. Establishes the verity/EROFS boot cmdline.
2. Confirm the modules are resident and `modules_disabled` is set
   (`cat /proc/sys/kernel/modules_disabled` → 1; `lsmod | grep nvidia`).
3. Stop `nvidia-persistenced`; **unbind**; confirm
   `/sys/bus/pci/devices/<BDF>/driver` is absent. *This is the load-bearing step:* it
   proves bind/unbind is unaffected by the lockdown.
4. `vm.remove-device` → verify eject → `vm.pause` → `vm.snapshot`.
5. `vm.restore` → `vm.resume` → `vm.add-device` → **re-bind** → `nvidia-smi`, **and from
   inside a container that existed before the detach**, confirm its pre-existing
   `/dev/nvidia*` still work (this is the §4.3 inference; if it fails, implement C5).
6. Optional: test whether `nvrc.uvm.persistence.mode` on the kernel cmdline suppresses
   `nvidia-persistenced`, which would remove step 3's daemon stop.

Steps 3 and 5 are the ones that could invalidate the bump. Everything else is mechanical.

**Not solved by the bump:** `cuda-checkpoint` is **absent from the 595 image** (verified).
The bump makes it *possible* (driver ≥ r550) but not *present*; T2 must still deliver the
binary into the guest.

---

---

## 13. T2 delivery plan: getting `cuda-checkpoint` into the actor

Out of scope for implementation (see §9), but the delivery mechanism is **decided** so it
is not re-litigated later.

**Where it runs.** `cuda-checkpoint --toggle --pid <pid>` acts on a CUDA process and needs
the nvidia device nodes, privilege over the target, and the target visible in its PID
namespace. It therefore runs **inside the actor's container**, invoked through the
kata-agent's container-scoped **`ExecProcess`** RPC (present in our vendored `agent.proto`,
currently unwired) using container-local PIDs.

**How it gets there — the same route as the driver libraries.** It is a
GPU-independent file, so it follows the static-artifact path already used in §4.3:

```
assemble-gpu.sh      → fetch cuda-checkpoint (pinned url + sha256)
SandboxConfig.assets → new "cuda-checkpoint" asset; atelet fetches it like any other
virtio-fs            → lands in the shared dir already served into the guest
CreateContainer      → bind-mounted into GPU actors' containers, beside the driver libs
suspend              → ExecProcess: cuda-checkpoint --toggle --pid <ctr-pid>
                       then the normal detach sequence (D9 → unbind → eject)
resume               → attach → verify bind (D10) → ExecProcess: cuda-checkpoint --toggle
```

Every step is an existing, supported mechanism: no guest-image rebuild, and it survives
image bumps unchanged.

**Rejected:** baking it into the guest image (forces an own-built rootfs, redone on every
bump); putting it in each actor's container image (pushes the burden onto every GPU
workload); streaming it over the debug console (multi-MB binary through an 8 s
interactive PTY).

**Prerequisites and unknowns:**
1. Driver **r550+** — so this lands only after the §12 image bump. On 535 it is inert.
2. The 595 guest is a minimal appliance (16 binaries; `libcuda`/`libnvidia-ml` present but
   **no `libcudart`**, no python, no CUDA samples). Verify whether `cuda-checkpoint` is
   self-contained or needs libraries beyond those already injected.
3. Restore ordering: the second `--toggle` must run **after** attach and bind, with the
   target process still alive.

---

## Appendix A — Evidence log

All results from bare-metal host `s2029gp-tr-0139`, 2026-08-09/10.

- GPU-less boot + `vm.add-device` + bring-up → `nvidia-smi` shows Tesla T4.
- Quiesce + unbind: `/sys/bus/pci/devices/0000:00:02.0/driver` absent; `lspci` shows no
  `Kernel driver in use:` line.
- `vm.remove-device` id `_vfio0` → 204; `device_tree` = `['_virtio-pci-__rng','__rng','__ioapic','__serial']`;
  `config.devices` = `[]`.
- `vm.pause` 204; `vm.snapshot` 204 → `config.json` 1.1K, `memory-ranges` 8.0G,
  `state.json` 84K; `'vfio' in state.json` → **False**.
- `vm.restore` 204; `vm.resume` 204; `vm.add-device` 200 → `{"id":"_vfio1","bdf":"0000:00:02.0"}`.
- Guest kernel log shows PCI enumeration and BAR assignment for `0000:00:02.0`.
- `nvidia-smi` after resume: Tesla T4, driver 535.86.10, CUDA 12.2, healthy.

### Guest-image inspection (2026-08-10)

- Old image `kata-gpu-artifacts:ubuntu22.04-535.54.03`: `/init` is 448 bytes of bash;
  `grep modules_disabled` → 0; no NVRC; no `cuda-checkpoint`.
- New image `kata-ubuntu-noble-nvidia-gpu-595.58.03.image` (from
  `kata-static-4.0.0-amd64.tar.zst`): MBR, partition 1 at sector 2048, **EROFS**;
  `/init` and `/sbin/init` both symlink `/bin/NVRC-x86_64-unknown-linux-musl`
  (224.5 KB, **UPX-packed**; 568.7 KB unpacked).
- Unpacked NVRC contains `/proc/sys/kernel/modules_disabled` + `1`, mode strings
  (`cpu`, `gpu`, `servicevm-nvl4/5`), `/bin/nvidia-persistenced`,
  `nvidia-ctk … cdi --output=/var/run/cdi/nvidia.yaml`, dm-verity handling
  (`veritysetup`, `root_hash`), DCGM/fabricmanager/nvlsm/InfiniBand, and cmdline knobs
  `nvrc.uvm.persistence.mode`, `nvrc.dcgm`, `nvrc.smi.*`. **No** hotplug/udev/rescan
  strings (confirms the Dec-2025 removal). **No** `cuda-checkpoint` binary.
- Note: searching the raw image with `strings` was misleading — the binary is UPX-packed,
  so absence of a string proved nothing until it was unpacked.

## Appendix B — Rejected alternatives

| Alternative | Why rejected |
|---|---|
| Snapshot with the GPU attached | Torn memory image; no device-state capture (§2.1) |
| `nvidia-vgpu-vfio` (vGPU/mdev) for migration v2 | Guest driver has never loaded under clh; three years, zero successes, unowned (§2.2) |
| `nvgrace-gpu-vfio-pci` | Zero migration code; ARM64/Grace only |
| Newer GPU (A100/H100/Grace) | Same generic `vfio-pci`; constraint is driver↔VMM, not silicon |
| `cuda-checkpoint` instead of detach | Does not detach the device; the VMM-level attachment is what blocks the snapshot. Also requires r550+ |
| Snapshot before containers start | Loses app warmth, which is the substrate demo's core property |
| Broad "allow all" device cgroup | Unnecessary — `UpdateContainer` accepts exact majors from the CDI spec |
