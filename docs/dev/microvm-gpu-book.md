# GPU passthrough and suspend/resume for micro-VM actors

How a GPU gets into a micro-VM actor, how it gets out again so the actor can be
snapshotted, and how it comes back. Written after the whole cycle was proven on a
Tesla T4, and structured so the *reasoning* survives even where the code changes.

Two bugs in this document were found only by running it on real hardware, and
both came from the same mistake. That story is section 7; it is the most useful
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
`ResumeActor` path. See section 11.

---

## 2. Vocabulary

Nine terms carry most of this document. Skim now, refer back later.

| Term | What it is |
|---|---|
| **VFIO** | The Linux framework for handing a physical PCI device to userspace (here, a VMM) safely. Appears as `/dev/vfio/<group>`. |
| **IOMMU** | Hardware that translates device DMA addresses. Without it, a passed-through device could DMA anywhere in host memory, so passthrough requires it. |
| **BAR** | Base Address Register. The field on a PCI device that says *where in the physical address space* its registers and memory live. The driver reads it, then talks to the device at that address. |
| **MMIO** | Memory-Mapped I/O. Reading and writing device registers by reading and writing memory addresses — the thing BARs point at. |
| **`_CRS`** | An ACPI table where the platform declares which address ranges a PCI host bridge actually routes. Linux uses it to decide where it may place BARs. Central to §7.2. |
| **ACPI eject** | The mechanism by which a VMM asks a guest to surrender a hot-plugged device. The *guest* performs the removal; the VMM only requests it. |
| **CDI** | Container Device Interface. A vendor-neutral spec describing everything a container needs for a device: node paths, library mounts, env, cgroup entries. |
| **NVRC** | NVIDIA's init binary inside their Kata GPU guest. Loads the driver, starts daemons, generates the guest's CDI spec, then execs kata-agent. |
| **memfd** | An anonymous memory-backed file. cloud-hypervisor backs guest RAM with one so `vm.snapshot` can write a **sparse** image — only the pages actually touched. |

---

## 3. The road to this design

The shape of the solution is mostly a record of what was ruled out.

### Kata's own GPU support does not apply

Kata Containers supports NVIDIA GPUs, but **only under QEMU**. There is no
`configuration-clh-nvidia-gpu.toml` anywhere in the kata-static tarball — only
`qemu-nvidia-gpu` and its tdx/snp variants. Kata's clh driver has no GPU path.

That was the first correction of the project: *cloud-hypervisor supports GPU
passthrough* and *Kata+clh+GPU works* are different claims. The first is true —
clh's `--device path=...` has worked for years. The second is not.

So ateom drives cloud-hypervisor's own VFIO passthrough directly, bypassing
Kata's GPU integration entirely. Internally this was "Option B", and it is why
`internal/ch` has device code at all.

### A migration-capable driver would have been easier, and does not exist

The clean answer to "snapshot a VM with a device" is VFIO **migration v2**,
where the device itself serialises its state. Investigated and rejected:

- **No NVIDIA GPU offers migration v2 for full passthrough.**
  `nvgrace-gpu-vfio-pci`, the only in-tree NVIDIA variant driver, contains
  **zero migration code** and is ARM64/Grace-only.
- **`nvidia-vgpu-vfio` (vGPU/mdev) does implement it**, but the vGPU guest
  driver **has never loaded under cloud-hypervisor** — failures spanning clh
  v29→v50, drivers 470→580, kernels 5.15 and 6.8, on V100/T4/L40S, in both mdev
  and SR-IOV modes, while the identical setup works under QEMU. Unowned by
  either vendor for three years.
- Newer silicon does not help: the constraint is the driver↔VMM pairing.

Hence detach-and-reattach rather than migrate.

### GKE could not host this at all

Every GKE node image reports **zero IOMMU groups** — GCE exposes no vIOMMU to
its VMs, so nested VFIO passthrough is impossible. Not a configuration problem;
there is nothing to configure. The work moved to a bare-metal T4 host, which is
why every result in this document comes from one machine.

### The initrd path was removed before any of this

The earlier GPU guest was an **initrd**. An initrd unpacks into guest RAM, so
all 876 MB of it would land in **every memory snapshot**. The current guest is a
disk image, demand-paged from virtio-blk, which never enters the snapshot. That
change (`ch.PayloadConfig.Initramfs` deleted) is a prerequisite for snapshots
being a sane size at all.

### Rejected alternatives, in one table

| Alternative | Why rejected |
|---|---|
| Snapshot with the GPU attached | Torn memory image, and no device state captured |
| `nvidia-vgpu-vfio` (vGPU/mdev) for migration v2 | Guest driver has never loaded under clh; three years, zero successes, unowned |
| `nvgrace-gpu-vfio-pci` | Zero migration code; ARM64/Grace only |
| Newer GPU (A100/H100/Grace) | Same generic `vfio-pci`; the constraint is driver↔VMM, not silicon |
| `cuda-checkpoint` instead of detach | Releases a CUDA context, not the *device*; the VMM-level attachment is what blocks the snapshot |
| Snapshot before containers start | Loses application warmth, which is the whole point of substrate's snapshots |
| Broad "allow all" device cgroup | Unnecessary — CDI supplies exact majors |

---

## 4. Why this is harder than "pass the device through"

Passing a GPU into a VM is routine. The difficulty is that substrate's actors
are **snapshotted**, and a passthrough device is hostile to that in three
separate ways.

### 4.1 A snapshot with a live device is silently corrupt

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

### 4.2 "Eject requested" is not "ejected"

`vm.remove-device` returns **204**, meaning the request was accepted. The actual
teardown happens later, when the guest executes the ACPI `_EJ0` method. Treating
the 204 as completion races the device straight back into the corruption above.

### 4.3 The guest cannot be asked anything

NVIDIA's Kata GPU guest rootfs contains `/bin/busybox` and **no `/bin/sh` or
`/bin/bash`**; `/init` points straight at NVRC. kata-agent's debug console
spawns `bash` then `sh`, so it accepts a connection and immediately dies. No
agent RPC fills the gap either — `ExecProcess` is container-scoped and
`CopyFile` only writes host→guest.

**Nothing can run a command in that guest.** Any design step of the form "then
the guest does X" is unimplementable. This invalidated an entire component
(section 9).

---

## 5. The design

### 5.1 Lifecycle

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

### 5.2 Decisions worth knowing

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

## 6. The code

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

### 6.1 The eject oracle

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

### 6.2 Identifying VFIO devices

Cold-plugged devices never produce an `add-device` reply, so their ids must be
read back from the device tree. Two traps, both from cloud-hypervisor's source:

- **`_vfio_user` shares the prefix** and is a different device class. The filter
  requires *digits* after `_vfio`.
- **The name counter is global** across all auto-named devices, so the first
  passthrough device is *not* necessarily `_vfio0`. Observed on hardware:
  `_vfio3`, becoming `_vfio4` after a detach/re-attach.

### 6.3 Giving the container the GPU

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

### 6.4 The snapshot gate

The last check before the memory image is written:

```go
func errIfPassthroughSnapshot(ctx, client) error   // checkpoint.go
```

Three properties, each chosen against a specific failure:

- **It asks the VMM, not us.** An earlier version keyed off the worker's
  allocation; that cannot see a detach, so once detach existed it would have
  refused every snapshot. Bookkeeping was the other option and is worse — it
  agrees with itself even when an eject silently failed.
- **`device_tree`, not `config.devices`.** clh drops a device from `config` the
  moment an eject is *requested*, so `config` reports it gone during exactly the
  in-flight window this exists to catch.
- **Unreadable is an error, never assumed-empty.** A malformed `vm.info` must
  not read as "nothing attached".

It rejects **both** snapshot scopes. FULL is unarguable — it serialises the guest
RAM the device is writing into. DATA is rejected conservatively rather than
provably: it captures only the host-backed durable share and never serialises
guest RAM, but the guest is still paused with the device live, and a paused vCPU
does not stop DMA into a page backing a durable-share mapping.

### 6.5 Ordering, and why it is the correctness argument

`detachPassthrough` does four things, and the order is the whole point:

```
1. release the guest's hold      nvidia-smi -pm 0 in the container
2. request every eject           vm.remove-device × N
3. confirm every eject           WaitDeviceRemoved × N
4. assert nothing remains        errIfPassthroughSnapshot
```

Step 1 before step 2 because the eject is *accepted* either way and then blocks
in the driver's remove callback — an eject requested first simply waits for the
timeout (§7.1). All of step 2 before any of step 3 because the guest can process
ejects concurrently; interleaving them turns one slow device into a timeout for
all of them.

Both properties are mutation-tested: swapping steps 1 and 2, or interleaving 2
and 3, each fails a test.

### 6.6 The `(deleted)` trap

`WaitDeviceRemoved` reads `/proc/<pid>/fd` and matches `/dev/vfio/<group>`. The
kernel appends `" (deleted)"` to a `/proc/<pid>/fd` symlink whose backing file
has been unlinked — so a regex anchored with `$` silently stops matching, the
check passes vacuously, and the snapshot proceeds with the device attached.
This was reproduced during review, not theorised. The matcher is
`/dev/vfio/[0-9]+( \(deleted\))?$`.

### 6.7 The guest image

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

## 7. The two production bugs

Both were invisible to every unit test, and both came from **adopting NVIDIA's
guest configuration wholesale**.

### 7.1 `nvidia-persistenced` holds the device open

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

### 7.2 `pci=nocrs` breaks hot-plug MMIO

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

### 7.3 A third, found the same way

`restoreFullScope` hardcoded `OnDemand` memory restore. Re-attaching a VFIO
device maps guest memory into the IOMMU, and VFIO must **pin** those pages;
pages still demand-paged through userfaultfd cannot be pinned, so
`vm.add-device` fails with `VfioDmaMap(IommuDmaMap(Error(14)))` — EFAULT. QEMU
has the same incompatibility between postcopy and VFIO.

Every GPU actor would have restored cleanly and then failed to get its device
back. Now `Copy` is selected whenever the worker holds a passthrough device,
which costs the ~75ms restore and the sparse memfd that `OnDemand` was buying.

---

## 8. The debugging journey

Fifteen hardware runs. Almost every one found something, and the *order* matters
— several conclusions were wrong until a later run corrected them.

| # | What happened | What it taught |
|---|---|---|
| 1 | `arm64` binaries on an x86 host | `assemble.sh` defaults to `ARCH=arm64` and neither script checks against the host |
| 2 | Guest booted; debug console `EOF` | **Claim A settled** — NVRC boots on a non-verity EROFS root |
| 3 | `ls /mnt/gpuroot/bin/` — no `/bin/sh` | The guest can never be asked anything |
| 4 | Eject-only detach **worked** | Led to deleting the guest-side layer — correct code, wrong reason (see §9) |
| 5–7 | Restore 500s: spent virtiofsd, pid-file `flock`, bound vsock socket | All artifacts of reusing one actor id; production restores under a new one |
| 8 | `VfioDmaMap … Error(14)` | **Real bug**: `OnDemand` restore is incompatible with VFIO |
| 9–13 | Container spec rejected: capabilities, pid ns, read-only root, dynamic busybox, blocking reads | The cost of driving `CreateContainer` without atelet's pipeline |
| 13 | `nvidia-smi` runs in a bare ubuntu container | **CDI injection proven** — every NVIDIA bit came from the guest |
| 14 | Eject hangs once a container has used the GPU | Persistence mode. §9's deletion was premature |
| 15 | `pci=nocrs` stripped → **claim B passes** | The last bug |

### 8.1 Five diagnostics that asserted their own hypothesis

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

### 8.2 Measuring the wrong thing

`/sys/.../resource` is the kernel's *record* of a BAR assignment. After a
restore that record comes back with the rest of guest memory — so comparing it
either side of a cycle compares the kernel's memory against itself. PCI **config
space** is the device.

More generally: after a restore, *anything read from guest memory is a memory of
the past, not an observation of the present.*

### 8.3 What actually cracked it

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

## 9. What was deleted

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

## 10. How this is tested

Three layers, each catching a different class of problem.

### Unit tests with fakes

A stand-in cloud-hypervisor on a unix socket, a stubbed guest console, a fixture
`/proc` tree. They are fast, run in CI, and are **blind to the environment** —
every bug in §7 was invisible to them, because a fake console always answers and
a fixture `/proc` has no persistence mode.

What they *are* good at is ordering and contracts, which is why the seams exist:

```go
var (
    waitDeviceGone     = (*ch.Client).WaitDeviceRemoved
    releasePersistence = clearGPUPersistence
)
```

Thin orchestration is exactly where the correctness argument lives and exactly
what unit tests skip as "too trivial to test".

### Mutation testing

Every non-trivial change here was checked by breaking it deliberately and
confirming a test fails. It found real holes that coverage would have reported
as green:

- An unconditional CDI annotation **survived** the suite — the only
  no-allocation test hit an early return and never reached the annotate line.
- A literal (non-quote-split) scan sentinel **survived** — no test covered "the
  console echoed the command but never ran it".

Both were fixed by tightening the test, not the code. A line can be *executed*
by one test and *checked* by none.

### Hardware tests (`-tags gpuhw`)

Two, both build-tagged so they never run in CI or on a laptop:

| Test | Needs | Covers |
|---|---|---|
| `TestGPUCycleOnHardware` | GPU + assets | boot, detach, snapshot, restore, re-attach — VM level only, ~20s |
| `TestGPUContainerSeesDeviceOnHardware` | + a container rootfs | CDI injection and whether the **container** can use the GPU across the cycle |

Neither needs Kubernetes, atelet, ateapi or OCI bundles. That was the decision
that made this tractable: the risky code is ~400 lines of VM lifecycle, and the
runbook wraps it in a cluster, a scheduler and an image registry — none of which
was uncertain. Splitting the *hardware* dependency from the *orchestration*
dependency turned one blocked test into one runnable immediately.

The cost is visible in §8: nine iterations were spent rebuilding, by hand, the
OCI spec that atelet supplies for free.

---

## 11. What is proven, and what is not

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
allocation is precisely what §7.2 was about.

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

## 12. Running it

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

## 13. If you are picking this up

Read in this order: §4 (why it is hard), §7 (the two bugs), §9 (the deletion).
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

---

## 13b. Known issues and operational consequences

Four found in review after the hardware work. Two were code bugs and are fixed;
two are properties of the approach that operators need to know.

### Fixed: the CDI annotation was applied to any passthrough device

`resolveWorkerDevices` matches the bare `PCI_RESOURCE_` prefix, which is the
convention **every** Kubernetes PCI plugin follows — SR-IOV NICs, RDMA adapters,
FPGAs. That is right for the VFIO plumbing, which genuinely does not care what
the device is. It was wrong for the CDI annotation: an actor holding only an
SR-IOV NIC would have been annotated `nvidia.com/gpu=all`, and an unresolvable
CDI device **fails `CreateContainer`** rather than being ignored. Not a slower
path — the actor would not start.

`withGuestCDIDevices` now checks the device's sysfs `vendor` and `class`, and
annotates only for NVIDIA display-class devices. This also excludes the card's
HDMI-audio function, which arrives in the same IOMMU group.

### Fixed: every checkpoint paid for the snapshot gate

`detachPassthrough` read the device tree, returned early when empty, and then
`errIfPassthroughSnapshot` read it **again** — so every suspend on every worker
paid two `vm.info` round trips. Worse, the gate's deliberate "an unreadable
device tree is an error" turned a slow or malformed `vm.info` into a *failed
snapshot* for an actor that never had a device.

`detachPassthrough` now reports whether it detached anything, and the gate runs
only then. It remains an assertion about a detach that just happened, which is
the only thing it was ever asserting.

### Consequence: a GPU actor hard-reserves its whole memory allocation

VFIO **pins** the guest's RAM into the IOMMU for the lifetime of the attachment
— that is the same property that makes §7.3's `OnDemand` restore impossible.
Pinned pages are unevictable and unswappable, so a GPU actor's full memory
allocation is reserved on the node for real, not merely accounted for. Non-GPU
actors sharing that node lose reclaim headroom and any overcommit the scheduler
assumed.

The eager restore compounds it. Dropping `OnDemand` also drops the sparse memfd,
so resuming a GPU actor reads the entire memory image at once — an I/O burst
against the same storage other guests are demand-paging their EROFS roots from.

Neither is a bug, but a node's GPU actors should be sized as if their memory
were fully committed, because it is.

Encoding that in a worker's resource requests is the obvious response and is
**deliberately not done here**. Worker sizing is unresolved and owned elsewhere:
the pod-vs-guest memory relationship has its own open issues (an undersized
worker pod host-OOM-kills the VM), and `ActorTemplate.spec.resources` sizing is
being designed separately. Numbers invented in a demo fixture would cut across
that work and be wrong the moment it lands. The right place for this constraint
is whatever emerges there; the point to carry over is that GPU actors cannot be
overcommitted, because the kernel will not allow it.

### Reduced: the suspend path holds a service-wide lock across the eject

`CheckpointWorkload` takes `s.lock` for its whole duration, and
`RunWorkload`/`RestoreWorkload` take the same lock, so a slow eject blocks
unrelated operations on that ateom.

**This is deliberate, not an oversight.** The field says so:

```go
// lock serializes RPCs; like ateom-gvisor, the run/checkpoint/restore
// lifecycle is not safe to drive concurrently.
lock sync.Mutex
```

and the surrounding comment notes it is already "held across a cold boot with
its retry, across a snapshot write, and across a restore" — which is why
`activeActor` is atomic and `GetWorkloadStats` takes no lock at all. Long holds
were designed for.

**Per-actor locking would be unsafe.** `deactivateActorNetworking` operates on
`s.atunnelIngress` and `s.atunnelEgress`, which are service-scoped singletons,
not per-actor state. Splitting the lock by actor would let two actors race on
shared networking — trading a latency problem for a correctness one.

So the GPU work does not introduce an architectural problem; it adds one more
long operation to a lock that already spans cold boots and restores. What it
*could* have added was a worst case of N × `ejectTimeout` for an N-GPU actor,
because the ejects were requested together and then awaited in sequence. They
are now awaited **concurrently**, which is how the guest processes them anyway,
bounding the contribution to roughly one timeout regardless of device count.

Narrowing the lock itself remains open, and belongs to whoever owns that
lifecycle: it is shared code with no coverage from this work, and the comment
suggests the serialisation is load-bearing beyond networking.

## 14. What we would do differently

**Restore under a second actor id.** Three separate failures — a spent
virtiofsd, a held pid-file `flock`, a bound vsock socket — were all one root
cause: the hardware test reuses a single actor id where production always uses a
fresh one. `rewriteSnapshotSocketPaths` exists precisely to repoint a snapshot
at new paths. Collapsing that lifecycle saved setup and cost three rounds of
debugging.

**Mount the guest image on day one.** It was downloadable from the start.
Thirty seconds with `mount -o loop,offset=1048576` would have shown no
`/bin/sh`, and invalidated an entire component *before* it was written, along
with the design section that specified it.

**Wire every subprocess's output immediately.** `LaunchVMMOptions.Stdout` and
`VirtiofsdOptions.Log` both existed and both were left nil. A 500 from clh and
an exit from virtiofsd each cost a hardware round-trip to diagnose something the
process had already printed.

**Read the first successful output as carefully as the failures.**
`Persistence-M: On` was printed in the run that first proved `nvidia-smi`
worked. It was the cause of the next two failures, sitting in a column nobody
read.

---

## 15. Appendix — the commit trail

41 commits. The ones that changed production behaviour:

| Commit | |
|---|---|
| `98017645` | VFIO passthrough with attach/detach primitives |
| `bf5ef9f1` | The CDI annotation — the actor container gets its GPU |
| `2d236b2c` | Boot the guest with its own root parameters (EROFS) |
| `bf5571a4` | Detach for snapshots, re-attach on restore |
| `965ede1d` | **Restore eagerly when a passthrough device is attached** |
| `ddde5f25` | Drop the guest-side detach (574 lines out) |
| `afd11a6b` | **Release the guest's GPU hold before ejecting** |
| `129c7fc4` | **Drop `pci=nocrs` from the generated GPU config** |

The other 33 are tests, diagnostics and documentation — including the
instrumentation that found the three bold entries above. That ratio is the
honest shape of the work: the fixes are small, and finding them was not.

---

## 16. Sources

Claims in this document that came from reading source, not from inference:

| Claim | Where |
|---|---|
| `VfioPciDevice`'s `Pausable` is empty | cloud-hypervisor `vmm/src/device_manager.rs` |
| VFIO ids are `_vfio<N>`, counter global, never reset | clh `device_manager.rs:176`, `:3844-3865` |
| `config` is cleared on eject request; `device_tree` is not | clh `device_manager.rs:5101`, `:5136-5139` |
| kata-agent does in-guest CDI injection since 3.11.0 | kata `src/agent/src/device/mod.rs:430`, `rpc.rs:278` |
| CDI adds cgroup entries, not just nodes | `container_edits.rs:104-109` |
| NVRC writes the CDI spec before forking the agent | nvrc `src/toolkit.rs:31-46`, `main.rs:63`→`:126` |
| NVRC starts `nvidia-persistenced` unconditionally | nvrc `src/main.rs:55`, `daemon.rs:15-21` |
| The daemon defaults to persistence **enabled** | nvidia-persistenced `options.c:100`, `option-table.h:87-93` |
| The driver spins waiting for `usage_count` | `kernel-open/nvidia/nv-pci.c:2324-2350` |
| Persistence must be off before removal | NVML docs, `nvmlDeviceRemoveGpu_v2` |
| No `nvrc.*` param disables persistence | nvrc `src/kernel_params.rs:31-38` (complete list) |
| The guest image is MBR, p1 EROFS, p2 verity | read directly from the shipped image |
