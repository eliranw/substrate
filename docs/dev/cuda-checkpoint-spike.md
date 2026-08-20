# Suspending a GPU actor that is using its GPU

**Answer: it works, for single-process workloads.** A CUDA process checkpointed
with NVIDIA's `cuda-checkpoint` releases the driver reference the eject blocks
on, so an actor holding a live context suspends and resumes normally, its device
memory survives intact, and its kernels still compute **correct** results
afterwards — verified against exact arithmetic and an independent CPU reference.

Multi-process servers are a different matter: vLLM cannot be checkpointed
externally and needs native support, which upstream is building.

Measured on `ipp1-1984`, A100-PCIE-40GB, guest driver 595.58.03,
cloud-hypervisor v52, 2026-08-19. Throwaway fixture on
`eliranw/poc-cuda-checkpoint`; nothing here is product code.

Reproduction detail — environment, pod shapes, full manifests, raw logs — is in
[`cuda-checkpoint-benchmark.md`](cuda-checkpoint-benchmark.md). This document
argues the result; that one records how it was obtained.

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

## Confirmed with PyTorch

The first fixture held a bare driver context with one allocation and never
launched a kernel, which was a fair objection to the result. A second fixture
ran the real thing: **torch 2.13.0a0 on the A100**, a 512 MiB checksummed
tensor, and continuous 2048x2048 cuBLAS matmuls on a non-default stream with
568 MiB live in the caching allocator.

Checkpointed mid-workload, then suspended and resumed:

```
toggling back; state: checkpointed
UNTOGGLE rc=0
matmuls=420  allocated=568MiB  digest=e4cda77b566f6763  integrity=OK
matmuls=440  allocated=568MiB  digest=e4cda77b566f6763  integrity=OK
```

Same digest as before the cycle, and the matmul counter resumed from where it
stopped. It has since run past 112,000 matmuls, integrity OK throughout.

So the kernels launch and complete without faulting. But that fixture verified a
digest over a *static* tensor the kernels never touched, so it proved liveness,
not correctness — a GPU returning plausible-but-wrong numbers would still have
reported `integrity=OK`.

## Correctness, measured

A third fixture closes that gap with three checks that fail for different
reasons, each with its output buffer zeroed first so "the kernel silently did
not run" fails the same way "the kernel returned garbage" does:

| check | rules out |
|---|---|
| `ones @ ones == 1024`, exact equality | cuBLAS returning wrong values. 1024 is exactly representable, so no tolerance is needed and TF32 cannot muddy it |
| `a @ b` against a **float64 CPU reference** | wrong results on irregular data, measured against something the device under test cannot corrupt |
| a hand-written `3x+7` kernel built with `nvcc` | the non-library path, and whether a compiled module survives |

Across a full cycle — checkpoint, suspend, GPU ejected, restore, re-attach,
driver rebind, un-checkpoint:

```
round=13  exact=ok(mismatched=0) ref=ok(maxerr=3.343e-03) custom=ok(mismatched=0)   <- last before
          [ actor checkpointed / restored, device ejected and re-attached ]
round=14  exact=ok(mismatched=0) ref=ok(maxerr=3.343e-03) custom=ok(mismatched=0)   <- first after
```

`maxerr` is **bit-identical** either side, not merely within tolerance. Still
passing at round 138. Cycle cost: toggle 465 ms out, suspend 87.4 s, resume
67.4 s, toggle 2010 ms back.

The custom kernel is the load-bearing one: an `nvcc`-compiled module, loaded
into the context, still executes correctly after the device was ejected and the
driver rebound. Module state lives in the context `cuda-checkpoint` has to
reconstruct, so it was the most likely thing to break.

## Where it does not hold: multi-process servers

vLLM does **not** suspend, even checkpointed. `nvidia-smi` reports one compute
PID, `cuda-checkpoint --toggle` moves it to `checkpointed` cleanly, and the
eject still times out at 30s.

The reason is upstream, not here. `vllm-project/vllm#37921` (RFC #34303) is
implementing checkpoint/restore *inside* vLLM, including `gpu_worker`
`suspend()`/`resume()` that destroy and reinitialise NCCL communicators — 
application-level coordination no external tool can perform. `nvidia-smi
--query-compute-apps` lists processes holding a CUDA *context*, while the
driver's `usage_count` counts open *file descriptors*; for a single process
those coincide, for a process tree they do not.

So the dividing line is not synthetic versus real. It is **single-process versus
multi-process-with-collectives**. Serving frameworks need native support, and
vLLM's is in flight.

## Still untested

**In-flight work** — the device was quiesced before every checkpoint, which
sidesteps precisely the restriction NVIDIA documents. Also multi-GPU, IPC and
peer access, NCCL, CUDA graphs, and restoring onto a **different physical card**
(actors resume on whichever worker is free; behaviour across GPU UUIDs is
unknown, and this is a single-GPU node).

## What it costs

Guest RAM is the gate, and there is no per-actor override: `default_memory`
comes only from the staged kata-config. PyTorch needs more than the stock 2048
MiB before it allocates anything on the device, so this ran on a separate
`microvm-gpu-poc` SandboxConfig at 8192 MiB.

Snapshot size follows the checkpointed VRAM, because that is where it lands:

| snapshot (`memory-ranges.zstd`) | size |
|---|---|
| golden, idle GPU, 8 GB guest | 149 MB |
| PyTorch, 568 MiB allocated, checkpointed | **1,132 MB** |

The suspend/resume path itself is unchanged — detach 3.32 s, re-attach 12.57 s,
restore 37.5 s, all within noise of an idle actor. What grows is the snapshot,
and it is read back **eagerly**, since a passthrough device forbids lazy
restore.

One number worth chasing: suspend wall time was 325 s, of which ateom's own
work was 21 s. The remaining ~5 minutes sat in the control plane before ateom
was asked to do anything. Not investigated.

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
# demos/gpu/poc-cuda-checkpoint.yaml.tmpl  — bare driver context, 2 GB guest
# demos/gpu/poc-torch-checkpoint.yaml.tmpl — PyTorch, needs microvm-gpu-poc (8 GB)
```

Both are throwaway. The PyTorch one needs the `microvm-gpu-poc` SandboxConfig,
which is the stock GPU config with a `configuration-clh-gpu-8g.toml`
(`default_memory = 8192`, `default_vcpus = 4`) staged under
`kata-gpu-assets/`.

`cuda-checkpoint` is embedded in the template as base64 (5,976 bytes, sha256
`707fa7f5…`). It is fetched rather than staged because SandboxConfig assets
land host-side while this has to run inside the container; bridging that needs
an ateom change shaped like `withNVIDIADriverBindMount`. A real implementation
should use the asset route.
