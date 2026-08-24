# Suspending a GPU actor that is using its GPU

**Answer: it works.** A CUDA process checkpointed with NVIDIA's
`cuda-checkpoint` releases the driver reference the eject blocks on, so an actor
holding a live context suspends and resumes normally, its device memory survives
intact, and its kernels still compute **correct** results afterwards — verified
against exact arithmetic and an independent CPU reference.

It works for **vLLM** too — suspended mid-serve, resumed, and answering
byte-identically afterwards — but only once you enumerate the right processes.
**The enumeration method is load-bearing**, and getting it wrong looks exactly
like the technique not working.

It also survives landing on a **different physical GPU**, which is the ordinary
case as soon as a node has more than one: actors resume on whichever worker is
free.

Measured on `ipp1-1984`, A100-PCIE-40GB, guest driver 595.58.03,
cloud-hypervisor v52, 2026-08-19 and 2026-08-20; cross-card on `a4u8g-0069`,
8x A40, cloud-hypervisor v53, 2026-08-24. Throwaway fixture on
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

## Which processes to checkpoint

Ask the wrong oracle and the technique appears to fail. Two ways to find "the
processes using the GPU", and for a process tree they do not agree:

| | answers | vLLM reported |
|---|---|---|
| `nvidia-smi --query-compute-apps` | who the driver counts as a **compute app** | **1** pid — `VLLM::EngineCore` |
| `grep /dev/nvidia\|libcuda /proc/*/maps` | who has the device or driver **mapped** | **2** pids — the above, plus the `vllm serve` launcher |

The eject blocks on `usage_count`, which counts **open file descriptors**. So the
second list is the one that matches the constraint. For a single process the two
coincide, which is why every earlier fixture here was unaffected.

That difference is the whole result. Toggling only the compute app leaves the
launcher holding a reference and the eject times out at 30 s. Toggling both:

```
state before pid 127: running      pid 199: running
TOGGLE pid 127 rc=0                TOGGLE pid 199 rc=0      toggle total 4816ms
state after  pid 127: checkpointed pid 199: checkpointed
Detached passthrough devices for snapshot   took 3.71s
suspend exit 0, 155s wall  ->  STATUS_SUSPENDED       resume 100s
device back after 6s of polling
UNTOGGLE pid 127 rc=0              UNTOGGLE pid 199 rc=0    untoggle total 4914ms
VERDICT: IDENTICAL -- greedy output matches across the cycle
VERDICT: long-prompt output also identical
```

Greedy decode, so identical output is a real check rather than a coincidence:
the same prompt through the same weights must produce the same tokens, and a
corrupted KV cache or a half-restored context would show up immediately. Both a
short prompt and a 40-sentence one match. Reproduced across three runs.

Note pid 129: `nvidia-smi` never listed it, yet `cuda-checkpoint --get-state`
reported it `running` and toggled it cleanly. It was holding driver state the
whole time, invisible to the tool being used to look for exactly that.

The enumeration above is [dims' wrapper from PR
#96](https://github.com/agent-substrate/substrate/pull/96), which does the same
scan for the gVisor + nvproxy path. His blocker is different — `runsc checkpoint`
refusing with *"can't save with live nvproxy clients"* rather than the driver's
`usage_count` loop — but both are per-holder counters, so both are satisfied only
when **every** holder is drained.

### Untoggle after the rebind, not after a timer

The toggle-back has to wait for the driver rebind, not merely for the actor to be
running again. Firing it too early fails hard:

```
UNTOGGLE pid 129 rc=1 : Error initializing CUDA:      untoggle total 80ms
```

Polling `nvidia-smi -L` until the device answers fixes it, and needed **6
seconds**. The fixture had been sleeping 2400.

### What this does not show

Single GPU, so there were no collectives to tear down. It says nothing about
tensor parallelism, where `vllm-project/vllm#37921` (RFC #34303) is adding
`gpu_worker` `suspend()`/`resume()` that destroy and reinitialise NCCL
communicators. That work may still be required above TP=1.

## Restoring onto a different physical card

Actors resume on whichever worker is free, so a restore lands on a different GPU
as soon as the node has more than one. Everything above was measured on a
single-GPU node, where the actor could only ever return to the card it left.

Suspended on one A40, resumed on another **on a different NUMA node**:

```
CARD BEFORE : GPU-d96411d6-804a-a758-344e-dae4f51bd940   0000:24:00.0  NUMA 0
CARD AFTER  : GPU-6e6cfab7-fdd7-3862-2e69-a9c27582bcbe   0000:e1:00.0  NUMA 1

TOGGLE   pid 87 rc=0 in  247ms
device back after 2s of polling
UNTOGGLE pid 87 rc=0 in 1898ms

round=14..21  exact=ok(mismatched=0) ref=ok(maxerr=3.499e-03) custom=ok(mismatched=0)
```

**The UUID changing is the result.** The fixture prints `VACUOUS` rather than a
pass when it does not, because a same-card landing produces three green checks
indistinguishable from a real one — and with the device plugin choosing which
card each worker gets, a same-card landing is entirely possible.

`maxerr` is bit-identical across the move. The custom kernel is again the
load-bearing check: an `nvcc`-compiled module lives in context state
`cuda-checkpoint` has to reconstruct, and here it was reconstructed against
**different silicon**.

Forcing the landing takes some care. Suspending an actor frees the worker it
just left, so a naive resume puts it straight back on the same card. Deleting
that worker's pod first is enough: the replacement re-runs device allocation and
came back with a different card.

### What this does not show

One observation, same GPU model, same driver version. Nothing here speaks to
restoring onto a **different model** -- different BAR sizes and device IDs, and
the guest driver would be resuming a context captured against other silicon.

## Still untested

**In-flight work** -- the device was quiesced before every checkpoint, which
sidesteps precisely the restriction NVIDIA documents. Also multi-GPU per actor,
IPC and peer access, CUDA graphs, and NCCL (reached only through vLLM's failure
at TP=1, never exercised directly).

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

## Defects found along the way

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

### A restore reported success over a dead VM

`verifyGuestGPU` already asks the guest whether the device came back, and already
reports why when it has not. The caller logged that at `WARN` and continued, so
an actor whose GPU was dead still reached `STATUS_RUNNING` and ateom still
emitted `Actor restored`.

Seen when the VMM aborted three seconds after the attach: ateom logged the
warning, dialled a vsock whose process was gone, logged that too, and reported
success. Every status an operator could see said the actor was healthy; the
workload had never started, and the stale log from the *previous* actor on that
pod made it look merely slow.

`detachPassthrough` already takes the opposite stance and says why -- it refuses
an eject it cannot confirm rather than snapshotting on the strength of it.
Restore had the same shape and the opposite behaviour. Now returns the error.

The two are mirror images: `STATUS_SUSPENDING` strands an actor after a failure,
this one dresses a failure as success. Both end with the actor's status not
describing reality.

### Envoy sizes its worker pool from the host, not the cgroup

Not GPU-related, and reachable by anyone deploying on a large node. On a
256-CPU host Envoy tried to start 256 workers and exhausted the file-descriptor
limit before finishing startup:

```
[warn] evutil_make_internal_pipe_: pipe: Too many open files
[err]  evsig_init_: socketpair: Too many open files
```

Both atenet-router and atenet-egress crashloop with the Go container beside them
healthy, which reads as an atenet fault rather than a sizing one. Pinned to four
workers.

### cloud-hypervisor aborts on a large-BAR GPU

Upstream, and unfixed as of v53.0. A vCPU thread touches the GPU's BAR and clh
unwraps a `None`:

```
thread 'vcpu0' panicked at pci/src/vfio.rs:1365:53:
called `Option::unwrap()` on a `None` value
thread 'vmm' panicked at device_manager.rs:5599:41: PoisonError
panic in a destructor during cleanup -- aborting
```

Intermittent -- 3 of 4 attempts on v52.0, 1 of 4 on v53.0 -- and it hits cold
boot and restore alike, always ~26-28s in, which is when the guest driver first
programs the BAR. The A40's 48 GB BAR takes ~26s to map against the
A100-PCIE-40GB's ~12s, and the A100 never showed it, so a wider race window is
the likely explanation. v53's #8237 ("return all-ones for unregistered MMIO/PIO
reads") and #8369 ("fix a PCI device hotplug race by deferring device
visibility") narrowed it without closing it.

Guest memory is ruled out: re-running on the 2048 MiB config the A100 proved
panicked identically.

### `ActorTemplate` spec is immutable

Sensible -- a changed template invalidates its snapshots -- but it means each
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
