# Suspending a GPU actor that is using its GPU

**Answer: it works.** A CUDA process checkpointed with NVIDIA's `cuda-checkpoint`
releases the driver reference the eject blocks on, so an actor holding a live
context suspends and resumes normally, and its device memory survives intact.

Measured on `ipp1-1984`, A100-PCIE-40GB, guest driver 595.58.03,
cloud-hypervisor v52, 2026-08-19. Throwaway fixture on
`eliranw/poc-cuda-checkpoint`; nothing here is product code.

---

## The question

`docs/dev/microvm-gpu-e2e.md` documents "idle GPUs only". The reason is one
loop in the NVIDIA driver's PCI removal path:

```c
while (atomic64_read(&nvl->usage_count) != 0)   // nv-pci.c
```

Ejection cannot complete while anything holds the device. `nvidia-smi -pm 0`
clears persistence mode, but a CUDA context is a separate reference. So: does
`cuda-checkpoint` release it?

## Why the shipped fixture could not answer this

`demos/gpu/gpu-microvm.yaml.tmpl` runs `nvidia-smi` in a loop. That opens and
closes the device in well under a second between five-second sleeps, so it
almost never holds a reference — which is why suspend already succeeds there.

**That is an instance of the limitation, not a counter-example to it.** The
first job was a workload that genuinely holds the device, and confirming that
suspend fails against it. Without that step every later result would have
passed vacuously.

## Method

A fixture that opens a CUDA context, allocates 64 MiB of device memory, writes
a known pattern, and reads it back every five seconds reporting integrity. The
same actor is then suspended twice: once holding the context, once after
`cuda-checkpoint --toggle`.

The guest has no shell and there is no exec into an actor, so the checkpoint is
self-timed from inside the workload. Guest time stops while suspended, so a
sleep spanning the cycle fires once the actor is running again — that is how
the toggle-back happens after restore.

## Result

Identical fixture, identical context, identical VRAM. Only the checkpoint
differs.

| | context held | context checkpointed |
|---|---|---|
| detach | **fails** — 30s timeout, `device _vfio5 still in the device tree` | **3.18 s** |
| suspend | fails, actor left unrecoverable | **exit 0, 25.6 s wall** |
| resume | — | **55.3 s wall** |
| VRAM afterwards | — | **`07264564`, integrity OK** |

The toggle itself was sub-second in both directions, `rc=0`, state moving
`running` → `checkpointed` → `running`.

### It costs nothing on the suspend/resume path

A checkpointed GPU actor is indistinguishable from an idle one:

| | idle GPU | checkpointed |
|---|---|---|
| detach | 2.15 / 3.09 s | 3.18 s |
| re-attach | 12.17 s | 12.21 s |
| total restore | 33.23 s | 32.79 s |

The driver rebind after re-attach is still required. Ampere behaves like the T4
there: the guest's own hot-plug probe fails and the address has to be written to
`/sys/bus/pci/drivers/nvidia/bind`.

## What this does not prove

The tested context is the simplest one that exists:

```c
cuInit(0); cuDeviceGet(&dev, 0); cuCtxCreate_v2(&ctx, 0, dev);
cuMemAlloc_v2(&dptr, 64 MiB); cuMemcpyHtoD_v2(...); cuMemcpyDtoH_v2(...);
```

No kernel launch, no streams, no in-flight work, no Runtime API, no managed or
pinned memory, no cuBLAS/cuDNN/NCCL, no CUDA graphs, one GPU. The device was
fully quiescent at checkpoint time, which sidesteps NVIDIA's documented
restrictions on in-flight operations.

The part that should generalise is the `usage_count` release — that is a
property of the device handle, not of the workload. Whether a *complex* CUDA
state restores correctly is a question about `cuda-checkpoint` itself.

Also untested: restoring onto a **different physical card**. Actors resume on
whichever worker is free, and behaviour across GPU UUIDs is unknown.

## The constraint that decides whether this is usable

**Guest RAM must be at least as large as the VRAM in use.** A checkpoint drains
device memory into guest memory, and guest memory is what the snapshot captures.

We moved 64 MiB against a 2048 MiB guest. A workload holding 20 GB on the card
needs a guest sized for it and produces a snapshot roughly 20 GB larger — read
back **eagerly**, because a passthrough device forbids lazy restore (VFIO must
pin guest memory to map it into the IOMMU, and userfaultfd-backed pages cannot
be pinned).

That interaction, not the checkpoint, is what determines whether this is
practical. It is the next thing worth measuring.

## Two defects found along the way

### A failed suspend strands the actor and leaks its GPU

Suspending with a held context leaves the actor in `STATUS_SUSPENDING`
permanently:

- `resume` → `AssignWorker prerequisite not met (want SUSPENDED or PAUSED)`
- `delete` → `not in a deletable status`
- no self-recovery, and **the worker stays ASSIGNED**

The guest is unharmed throughout — we watched a context stay alive with VRAM
intact for three hours while the actor was unreachable. On a single-GPU node
the card is unreclaimable. Recovery needs a WorkerPool cycle to zero, which
kills every actor on that pool.

The design fails closed on **data integrity** — no torn snapshot, nothing
corrupted, which is what it was built to guarantee. It does not fail cleanly:
the suspend error path never rolls the actor's state back.

This is reachable on merged code today by any GPU actor doing real work.

### `ActorTemplate` spec is immutable

Sensible — a changed template invalidates its snapshots — but it means each
fixture revision needs a new template name. Worth knowing before iterating.

## Reproducing

```bash
git checkout eliranw/poc-cuda-checkpoint
# demos/gpu/poc-cuda-checkpoint.yaml.tmpl — throwaway fixture
```

`cuda-checkpoint` is embedded in the template as base64 (5,976 bytes, sha256
`707fa7f5…`). It is fetched rather than staged because SandboxConfig assets
land host-side while this has to run inside the container; bridging that needs
an ateom change shaped like `withNVIDIADriverBindMount`. A real implementation
should use the asset route.
