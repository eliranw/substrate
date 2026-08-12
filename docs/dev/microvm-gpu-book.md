# GPU passthrough and suspend/resume for micro-VM actors

How a GPU gets into a micro-VM actor, how it gets out again so the actor can be
snapshotted, and how it comes back. Written after the whole cycle was proven on a
Tesla T4, and structured so the *reasoning* survives even where the code changes.

Two bugs in this document were found only by running it on real hardware, and
both came from the same mistake. That story is section 5; it is the most useful
part of this file.

---

## 1. What this achieves

A micro-VM actor can be given a physical GPU, use it, be suspended to a
snapshot, and be resumed with the GPU working again. While suspended, **the GPU
is free for another actor** — which is the point. GPUs are the scarce resource;
an actor that holds one while idle is the thing worth fixing.

Verified end to end on hardware:

| | |
|---|---|
| Guest boots with the GPU cold-plugged | NVIDIA's Kata GPU guest, EROFS root, no dm-verity |
| Container can use the GPU | `nvidia-smi` reports the T4 from inside a **bare ubuntu** rootfs |
| Device detaches for a snapshot | ACPI eject completes, VMM drops its `/dev/vfio` fd |
| Snapshot taken with the device gone | memfd-backed, sparse `memory-ranges` |
| Restore + re-attach | guest resumes, device returns |
| **Container still uses the GPU afterwards** | `nvidia-smi` again, same UUID |

What is *not* yet proven is the layer above: the real `SuspendActor` /
`ResumeActor` path. See section 8.

---

## 2. Why this is harder than "pass the device through"

Passing a GPU into a VM is routine. The difficulty is that substrate's actors
are **snapshotted**, and a passthrough device is hostile to that in three
separate ways.

### 2.1 A snapshot with a live device is silently corrupt

cloud-hypervisor's `VfioPciDevice` has an **empty `Pausable` implementation**.
`vm.pause` stops the vCPUs and does nothing to the device. A GPU is a bus
master: it writes to guest RAM on its own initiative, with no CPU involved. So
pausing and snapshotting a VM with a live GPU serialises memory *while the
device is still writing into it*.

The result is a torn memory image. Not one missing device state — one where the
memory itself is internally inconsistent. **cloud-hypervisor does not refuse
this.** It returns success.

That single fact shapes the whole design: the device must be *gone*, verifiably,
before the guest is frozen.

### 2.2 "Eject requested" is not "ejected"

`vm.remove-device` returns **204**, meaning the request was accepted. The actual
teardown happens later, when the guest executes the ACPI `_EJ0` method. Treating
the 204 as completion races the device straight back into the corruption above.

### 2.3 The guest cannot be asked anything

NVIDIA's Kata GPU guest rootfs contains `/bin/busybox` and **no `/bin/sh` or
`/bin/bash`**; `/init` points straight at NVRC. kata-agent's debug console
spawns `bash` then `sh`, so it accepts a connection and immediately dies. No
agent RPC fills the gap either — `ExecProcess` is container-scoped and
`CopyFile` only writes host→guest.

**Nothing can run a command in that guest.** Any design step of the form "then
the guest does X" is unimplementable. This invalidated an entire component
(section 7).

---

## 3. The design

### 3.1 Lifecycle

```
RUN       resolve PCI_RESOURCE_* → cold-plug at vm.create → boot
          NVRC brings up the driver, writes /var/run/cdi/nvidia.yaml
          container created WITH the CDI annotation → gets /dev/nvidia*

SUSPEND   clear GPU persistence (nvidia-smi -pm 0, in the container)
          vm.remove-device for every device
          confirm each eject (device_tree + VMM's /dev/vfio fd)
          assert nothing is still attached
          pause → snapshot → tear down

RESUME    relaunch VMM → restore (EAGER memory) → resume
          vm.add-device for every device
          confirm they are back in the device tree
```

### 3.2 Decisions worth knowing

**Cold-plug at boot, not hot-plug.** NVRC generates the guest's CDI spec exactly
once, *before* it forks the kata-agent. A device added afterwards never appears
in `/var/run/cdi/nvidia.yaml`, so the container would get no injection at all.
Boot-time attachment is load-bearing.

**One actor per worker.** `AssignWorkerStep.findFreeWorker` already guarantees
it, so "this worker's GPU allocation" and "this actor's GPUs" are the same set.
That is why `resolveWorkerDevices` can read the environment and be right.

**The device layer is vendor-agnostic.** cloud-hypervisor passes through
anything bound to `vfio-pci`, so the resolver matches the bare `PCI_RESOURCE_`
prefix any Kubernetes device plugin sets, and nothing below that point inspects
what kind of device it got. Only the CDI kind (`nvidia.com/gpu`) is
NVIDIA-specific.

**Verification reads observed state, never bookkeeping.** Our own record of what
we detached would agree with itself even when an eject silently failed. Every
check asks the VMM or the guest what is actually true, and an *unreadable*
answer is an error rather than an assumed-empty one.

---

## 4. The code

~700 lines of production code across 14 files.

| File | + / − | What it does |
|---|---|---|
| `devices.go` | +77 | Reads `PCI_RESOURCE_*` → `[]ch.DeviceConfig`. Vendor-agnostic. |
| `internal/ch/device.go` | +158 | `AddDevice`, `RemoveDevice`, `DeviceIDs`, `VFIOPassthroughIDs` |
| `internal/ch/eject.go` | +216 | `WaitDeviceRemoved` — the eject-completion oracle |
| `internal/ch/api.go` | +32 | `putJSON` (captures a response body; `put` discards it) |
| `internal/ch/createvm.go` | +18/−1 | `DeviceConfig`; dropped the initramfs path |
| `gpucdi.go` | +107 | The CDI annotation that gives the container its GPU |
| `gpuattach.go` | +227 | `detachPassthrough`, `attachPassthrough`, persistence release |
| `checkpoint.go` | +55 | Detach before pause; the snapshot safety gate |
| `restore.go` | +25/−2 | Eager memory restore when a device is attached |
| `run.go` | +50/−8 | Cold-plug at `vm.create`; root params from `rootfs_type` |
| `internal/kata/config.go` | +7 | Parses `rootfs_type` |
| `internal/kata/agentclient.go` | +33 | `ExecProcess` — the only way to run anything in the guest |
| `hack/microvm-assets/assemble-gpu.sh` | +146 | Builds the GPU guest **and** generates a clh config |
| `manifests/.../sandboxconfig-microvm-gpu.yaml.tmpl` | +57 | The GPU sandbox class |

### 4.1 The eject oracle

`vm.remove-device` returns 204 = "requested". `WaitDeviceRemoved` polls **two
independent signals** before any caller may proceed:

1. the device leaving `vm.info`'s `device_tree`, and
2. the VMM process dropping its `/dev/vfio/<group>` file descriptor.

It reads `device_tree` rather than `config.devices` deliberately.
cloud-hypervisor drops a device from `config` the moment an eject is
*requested*, so `config` reports it gone during exactly the in-flight window the
check exists to detect.

This was written after a hand-run of the same logic reported "EJECT COMPLETE"
while the device was still attached — both checks were vacuous (a wrong id, and
the `sudo` wrapper's PID instead of the VMM's). Hence `assertIsVMM`.

### 4.2 Identifying VFIO devices

Cold-plugged devices never produce an `add-device` reply, so their ids must be
read back from the device tree. Two traps, both from cloud-hypervisor's source:

- **`_vfio_user` shares the prefix** and is a different device class. The filter
  requires *digits* after `_vfio`.
- **The name counter is global** across all auto-named devices, so the first
  passthrough device is *not* necessarily `_vfio0`. Observed on hardware:
  `_vfio3`, becoming `_vfio4` after a detach/re-attach.

### 4.3 Giving the container the GPU

This turned out to be one annotation, not a subsystem:

```
cdi.k8s.io/ate-passthrough = nvidia.com/gpu=all
```

kata-agent runs `handle_cdi_devices` on every `CreateContainer` (since kata
3.11.0), scans annotations for the `cdi.k8s.io/` prefix, and injects the device
nodes, the driver library mounts **and the device-cgroup entries**. The spec it
resolves against is generated inside the guest at boot by NVRC, so its
major/minor numbers are already guest-native.

We therefore write neither the nodes nor the allowlist. Doing so would duplicate
the agent's work with host-side numbers that are wrong inside the guest.

Inherited `cdi.k8s.io/` annotations are **stripped** first: the host's sandbox
device plugin advertises kind `nvidia.com/pgpu`, which does not exist in the
guest's spec, and an unresolvable CDI device *fails* `CreateContainer` rather
than being ignored. Kata's own shim clears them for the same reason.

Proof it works, from the agent's own cgroup dump on hardware — entries nothing
in our code writes:

```
major 195 minor 0    /dev/nvidia0
major 195 minor 255  /dev/nvidiactl
major 195 minor 254  nvidia-modeset
major 239 minor 0    nvidia-uvm
```

### 4.4 The guest image

NVIDIA's GPU guest ships in the **same** kata-static tarball as the stock one,
so this is a file-selection difference rather than a second supply chain. But it
differs from the stock guest in ways that matter:

- **EROFS**, not ext4 — so `root=`/`rootflags=`/`rootfstype=` all change.
- **dm-verity hash tree in a second partition.** Partition 1 is a self-contained
  mountable EROFS filesystem, so booting it without verity is valid. Confirmed
  by reading the shipped image: MBR valid, p1 at 1.0 MiB spanning 482.0 MiB with
  EROFS magic `0xe0f5e1e2`, p2 at 483.0 MiB.
- **Its verity parameters describe one specific build.** `987136 sectors =
  8 × 123392` is exactly the `dataSectors` kata computes from the config's
  `data_blocks`. A hardcoded root hash would break on every image bump.

Upstream ships **no clh flavour** of the NVIDIA config — kata's GPU support is
QEMU-only — so `assemble-gpu.sh` derives one, lifting `rootfs_type` and
`kernel_params` from upstream's GPU config so they track the release.

---

## 5. The two production bugs

Both were invisible to every unit test, and both came from **adopting NVIDIA's
guest configuration wholesale**.

### 5.1 `nvidia-persistenced` holds the device open

**Symptom.** With a container that had used the GPU, `vm.remove-device` was
accepted and the eject *never completed*. The VM-only test ejected fine, because
nothing in that guest had ever opened the GPU.

**Cause.** NVRC starts `nvidia-persistenced` unconditionally
(`nvrc/src/main.rs:55`), and that daemon defaults to persistence mode **enabled**
(`options.c:100`). It holds the device's file descriptors from boot. The driver
then spins in its PCI remove callback:

```c
// kernel-open/nvidia/nv-pci.c:2324-2350
// "We can't return from this function without corrupting state,
//  so we wait for the usage count to go to zero"
while (atomic64_read(&nvl->usage_count) != 0) { os_delay(500); }
```

An unbounded blocking loop. NVIDIA documents the contract for their own removal
API in the same terms: *"persistence mode counts as an attachment to the GPU
thus it must be disabled prior to this call"* (`nvmlDeviceRemoveGpu_v2`).

**Fix.** `detachPassthrough` runs `nvidia-smi -pm 0` in the actor's containers
before requesting the eject. That does not write a kernel flag — it RPCs the
daemon, which closes the device and drops the refcount. It must run in a
*container* because the guest has no shell, and CDI mounts both `nvidia-smi` and
the daemon's IPC socket into every GPU container.

Best-effort by design: a workload that removed `nvidia-smi` from its image
cannot be helped, and failing the suspend then is worse than letting the eject
report the real state a moment later.

**No boot-time alternative exists.** `nvrc.uvm.persistence.mode=off` only drops
the `--uvm-persistence-mode` flag and leaves the daemon's own default intact; no
`NVreg_*` module parameter controls persistence in 595.58.03; and no released
NVRC tag handles unplug — the code that did was removed in `fd395ac`
("switching to cold-plug per default").

### 5.2 `pci=nocrs` breaks hot-plug MMIO

**Symptom.** After a re-attach the GPU was unusable. Device nodes present, BARs
apparently correct, driver bound, `/proc/driver/nvidia/gpus/` entry present —
and `nvidia-smi` reporting *"Unable to determine the device handle for gpu
0000:00:05.0: Unknown Error"*. cloud-hypervisor logged, every single run:

```
Failed moving device BAR: failed allocating new MMIO range:
0xd2000000 -> 0x80000000 (0x1000000), keeping old BAR
```

**Cause.** `pci=nocrs` tells Linux to **ignore the ACPI `_CRS` host-bridge
windows** — the address ranges the platform declares it actually routes — and
fall back to assuming MMIO begins just above top-of-RAM. It is a reasonable
workaround for bare-metal BIOSes that publish wrong windows, which is why
NVIDIA ships it.

cloud-hypervisor publishes *correct* windows. So ignoring them replaces good
information with a guess, and the guess is wrong: the guest demands
`0x80000000` — **exactly the top of a 2048 MiB guest**, which is why the address
never varied across seven runs — clh cannot route there, and the guest then
drives MMIO where the device is not.

**Why only hot-plug broke.** At cold boot clh lays the BARs out and the guest
only *reads* them, so a wrong map costs nothing. Hot-plug is the first time the
guest must *assign* an address — and re-attaching after a suspend is exactly
that.

**Fix.** `assemble-gpu.sh` strips `pci=nocrs` from the `kernel_params` it copies
out of NVIDIA's config. With it gone the kernel picks `0xc0000000`, inside clh's
aperture, and the GPU works after a hot-plug. `pci=realloc` is kept — stripping
it alone reproduced the failure identically, so it is not implicated.

### 5.3 A third, found the same way

`restoreFullScope` hardcoded `OnDemand` memory restore. Re-attaching a VFIO
device maps guest memory into the IOMMU, and VFIO must **pin** those pages;
pages still demand-paged through userfaultfd cannot be pinned, so
`vm.add-device` fails with `VfioDmaMap(IommuDmaMap(Error(14)))` — EFAULT. QEMU
has the same incompatibility between postcopy and VFIO.

Every GPU actor would have restored cleanly and then failed to get its device
back. Now `Copy` is selected whenever the worker holds a passthrough device,
which costs the ~75ms restore and the sparse memfd that `OnDemand` was buying.

---

## 6. The debugging journey

Fifteen hardware runs. Almost every one found something, and the *order* matters
— several conclusions were wrong until a later run corrected them.

| # | What happened | What it taught |
|---|---|---|
| 1 | `arm64` binaries on an x86 host | `assemble.sh` defaults to `ARCH=arm64` and neither script checks against the host |
| 2 | Guest booted; debug console `EOF` | **Claim A settled** — NVRC boots on a non-verity EROFS root |
| 3 | `ls /mnt/gpuroot/bin/` — no `/bin/sh` | The guest can never be asked anything |
| 4 | Eject-only detach **worked** | Led to deleting the guest-side layer — correct code, wrong reason (see §7) |
| 5–7 | Restore 500s: spent virtiofsd, pid-file `flock`, bound vsock socket | All artifacts of reusing one actor id; production restores under a new one |
| 8 | `VfioDmaMap … Error(14)` | **Real bug**: `OnDemand` restore is incompatible with VFIO |
| 9–13 | Container spec rejected: capabilities, pid ns, read-only root, dynamic busybox, blocking reads | The cost of driving `CreateContainer` without atelet's pipeline |
| 13 | `nvidia-smi` runs in a bare ubuntu container | **CDI injection proven** — every NVIDIA bit came from the guest |
| 14 | Eject hangs once a container has used the GPU | Persistence mode. §7's deletion was premature |
| 15 | `pci=nocrs` stripped → **claim B passes** | The last bug |

### 6.1 Five diagnostics that asserted their own hypothesis

The single most costly recurring mistake, worth naming because it kept
recurring in different disguises:

1. *"the agent could not resolve `nvidia.com/gpu=all`"* — while the serial log
   said it had.
2. *"virtiofsd was STILL ALIVE"* — `Signal(0)` succeeds on a **zombie**; it was
   measuring reaping, not liveness.
3. *"CDI injected no device nodes"* — from **zero bytes** of output, which says
   nothing either way.
4. *"design §4.3 is wrong and C5 is required"* — the nodes were all present;
   C5 would have been an entire component built to fix nothing.
5. *"the BAR mismatch is almost certainly the cause"* — asserted on adjacency in
   a log, then disproved, then *re-proved* by a better measurement.

The rule that would have prevented all five: **a failure message may state what
was attempted and what was observed, but may only state *why* when the
observation itself distinguishes the causes.**

### 6.2 Measuring the wrong thing

`/sys/.../resource` is the kernel's *record* of a BAR assignment. After a
restore that record comes back with the rest of guest memory — so comparing it
either side of a cycle compares the kernel's memory against itself. PCI **config
space** is the device.

More generally: after a restore, *anything read from guest memory is a memory of
the past, not an observation of the present.*

### 6.3 What actually cracked it

Two moves, neither of which was more instrumentation:

- **Splitting the sequence.** Six runs tested eject + snapshot + restore +
  hot-plug as one compound event, so every failure had four possible causes.
  Adding `ATE_SKIP_SNAPSHOT` — eject and re-attach into the same running VM —
  eliminated snapshot and restore in a single run.
- **Following the constant.** `0x80000000` appeared in every run while the
  variables changed. A value that never varies is computed from something fixed;
  it was exactly the guest's 2048 MiB RAM top. `pci=realloc` was a story that
  permitted the behaviour; `pci=nocrs` *predicted the specific number*.

---

## 7. What was deleted

An entire component — `GuestGPUBDFs`, `GuestDetachGPU`, `GuestVerifyGPUBound`,
their seams and twelve tests. **574 lines out, 64 in.**

It existed to stop `nvidia-persistenced` and unbind the driver over the guest
debug console before an eject. It was deleted for two stated reasons:

1. **Impossible** — that guest has no shell, so it could never have run. This
   reason was correct and remains correct.
2. **Unnecessary** — the ACPI eject calls the driver's `.remove()` itself, and
   an eject-only detach worked on hardware. **This reason was wrong.**

It was wrong because the test that justified it had no container, therefore no
GPU user, therefore no persistence — it was structurally incapable of producing
the failing case. The *question* the component asked ("make the guest release
the device") was real; only its *mechanism* was unusable.

The replacement does the same job through the container, where CDI has already
mounted `nvidia-smi`. The lesson is narrow and sharp:

> Deleting code because a test passes requires that the test **could have failed
> for the reason the code existed.**

---

## 8. What is proven, and what is not

### Proven on hardware

- Boot with a cold-plugged GPU on a non-verity EROFS root
- CDI injection: nodes, driver libraries, device cgroup
- `nvidia-smi` inside a **bare ubuntu** container — every NVIDIA component
  supplied by the guest through CDI
- Detach with a live container, snapshot, restore, re-attach
- The container using the GPU again afterwards, same UUID

### Not proven

**The product path.** No `SuspendActor`, no `ResumeActor`, no scheduler, no
ActorTemplate. Both hardware tests hand-drive cloud-hypervisor and the
kata-agent. The mechanics are proven; the orchestration above them is not.

**The full production VM shape.** The hardware test uses production's
`buildVMConfig` — so memfd-backed RAM and sparse snapshots are identical — but
it has **no virtio-net**, no overlay rootfs, no durable dirs, no `base-id`
lineage, and it restores under the *same* actor id rather than a new one. The
missing virtio-net is the notable one: production re-attaches a GPU into a VM
where another device is competing for the same MMIO aperture, and MMIO
allocation is precisely what §5.2 was about.

**Multi-GPU.** The code handles N devices; only N=1 has ever run.

**`clearGPUPersistence` on the production path.** It is unit- and
mutation-tested, but the `ExecProcess` call has never run against a real agent —
the hardware probe clears persistence itself.

### Known limits

- **VRAM does not survive suspend.** The device is ejected; GPU memory is gone.
  A workload holding model weights reloads them on resume. `cuda-checkpoint`
  (§13 of the design) would lift this and is deliberately deferred.
- **An actor mid-CUDA-computation cannot be suspended.** The eject blocks on a
  live context, so suspend returns an error rather than corrupting anything.
  Correct, but it means suspend requires an idle GPU.

---

## 9. Running it

```bash
# Assets (~2GB). Note ARCH: assemble.sh defaults to arm64.
ARCH=amd64 hack/microvm-assets/assemble.sh
hack/microvm-assets/assemble-gpu.sh

# A container rootfs for the probe
sudo ctr -n k8s.io images pull docker.io/library/ubuntu:24.04
sudo ctr -n k8s.io images mount --rw docker.io/library/ubuntu:24.04 /mnt/ubuntu

# The full cycle, container included
sudo -E env "PATH=$PATH" \
  PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4=<BDF> \
  ATE_PROBE_ROOTFS=/mnt/ubuntu \
  go test -tags gpuhw -run TestGPUContainer -v -timeout 20m ./cmd/ateom-microvm/
```

Useful knobs:

| Variable | Effect |
|---|---|
| `ATE_SKIP_SNAPSHOT=1` | Eject and re-attach only — isolates hot-plug from restore |
| `ATE_STRIP_KPARAMS="pci=nocrs"` | Drop kernel params without regenerating the config |
| `ATE_PROBE_QUIET_SECS` | How long the probe stays silent before looping |
| `ATE_PROBE_ROOTFS` | Unpacked image rootfs; omit for a static Go probe |

`TestGPUCycleOnHardware` is the VM-only variant: no container, no Kubernetes,
~20s, and enough to catch a regression in boot, detach, snapshot or re-attach.

---

## 10. If you are picking this up

Read in this order: §2 (why it is hard), §5 (the two bugs), §7 (the deletion).
Everything else is reference.

The highest-value next step is the **production path** — deploy substrate on a
GPU host and drive a real `SuspendActor`/`ResumeActor` through
`docs/dev/microvm-gpu-e2e.md`. That is where the untested layer lives, and where
the virtio-net-plus-GPU MMIO question gets answered.

Two things to be careful of, both learned expensively here:

- **Configuration inherited from NVIDIA describes their machine, not ours.**
  `rootfs_type` travels with the guest image. `pci=nocrs` describes bare-metal
  firmware. Nothing in the config distinguishes them.
- **A green unit test says the code works, never that you needed it.** The
  stubs in this package are faithful to the transports and blind to the
  environment — no shell in the guest, persistence mode on, MMIO apertures.
  Those are only visible on hardware.
