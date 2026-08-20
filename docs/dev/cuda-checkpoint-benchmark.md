# GPU actor suspend/resume with cuda-checkpoint — benchmark report

Reproduction detail for `docs/dev/cuda-checkpoint-spike.md`. That document
argues the result; this one records exactly what was run, on what, and what came
back, so the numbers can be re-derived or disputed.

**Run dates:** 2026-08-19 (context, PyTorch) and 2026-08-20 (kernel correctness,
vLLM) · **Branch:** `eliranw/poc-cuda-checkpoint`
**Everything below is throwaway PoC material**, not product code.

---

## 1. Environment

### Node

| | |
|---|---|
| host | `ipp1-1984` (NVIDIA Colossus lease, bare metal) |
| OS | Ubuntu 22.04.5 LTS |
| kernel | **6.8.0-138-generic** (HWE; ≥6.5 required for `lowerdir+`) |
| CPU | Intel Xeon Silver 4210R, 20 logical |
| RAM | 62 GiB |
| container runtime | containerd 2.2.1 |
| kubelet | v1.36.3 (single-node kubeadm) |
| IOMMU | `intel_iommu=on iommu=pt`, 76 groups |

### GPU

| | |
|---|---|
| device | NVIDIA **A100-PCIE-40GB** (GA100, `10de:20f1`) |
| BDF | `0000:65:00.0` |
| IOMMU group | **1 — the card is alone in it** |
| host driver | `vfio-pci` (`driver_override: vfio-pci`) |
| guest driver | **595.58.03**, baked into `rootfs-gpu.img` |
| advertised resource | `nvidia.com/GA100_A100_PCIE_40GB: 1` |

The A100 being alone in its IOMMU group matters — a T4 shares one with its
HDMI-audio function, which would have to be passed through alongside it.

### GPU Operator — `sandboxWorkloads.defaultWorkload=vm-passthrough`

```
nvidia-vfio-manager-w5w46                     1/1 Running   binds the card to vfio-pci
nvidia-sandbox-device-plugin-daemonset-gqjxf  1/1 Running   advertises the resource
nvidia-sandbox-validator-k6x25                1/1 Running
gpu-operator-node-feature-discovery-worker    1/1 Running
```

No `nvidia-driver-daemonset` — there is **no NVIDIA driver on the host at all**.

Node labels: `nvidia.com/gpu.deploy.vfio-manager=true`,
`nvidia.com/gpu.deploy.sandbox-device-plugin=true`,
`ate.dev/sandboxClass=microvm`.

### substrate

```
ate-api-server (x2), ate-controller, atelet, atenet-egress,
atenet-router, dns, rustfs, valkey-cluster-{0..5}    all Running
```

VMM: cloud-hypervisor **v52.0.0**. Object store: rustfs (in-cluster S3).

---

## 2. Sandbox configuration

Three configs, differing in **one asset** — the guest's memory budget.

```
microvm-gpu                                          microvm-gpu-poc
  cloud-hypervisor  829af01ff075bb96                   cloud-hypervisor  829af01ff075bb96
  kata-kernel       989cf9f00db16354                   kata-kernel       989cf9f00db16354
  kata-image        91d635a2ffc950dd                   kata-image        91d635a2ffc950dd
  virtiofsd         15b2e72a78cc08a9                   virtiofsd         15b2e72a78cc08a9
  kata-config       8f5bd2bb3cf5062c  ← 2048 MiB       kata-config       73d23e92041d2742  ← 8192 MiB
```

**Why a second config:** guest RAM comes only from `[hypervisor.clh]
default_memory` in the staged kata-config. There is no per-actor or
per-template override (`internal/kata/config.go`), and PyTorch's RSS exceeds
2048 MiB before it allocates anything on the device.

`configuration-clh-gpu-8g.toml` (491 bytes, sha256 `73d23e92…`):

```toml
[hypervisor.clh]
rootfs_type = "erofs"
kernel_params = "cgroup_no_v1=all pci=realloc  pci=assign-busses"
default_memory = 8192
default_vcpus = 4
```

`rootfs_type` and `kernel_params` are byte-identical to the generated config, so
the guest image they describe still matches. Note the **absence of
`pci=nocrs`** — `assemble-gpu.sh` strips it, because it makes the guest ignore
the VMM's real MMIO windows and breaks hot-plug.

A third, `microvm-gpu-vllm`, uses `configuration-clh-gpu-16g.toml` (sha256
`21f6a566…`, `default_memory = 16384`, `default_vcpus = 6`) for the vLLM run.

Staged to `s3://ate-snapshots/kata-gpu-assets/`.

---

## 3. Worker pod

One pod per actor, one actor per worker, so **the pod's GPU allocation is the
actor's allocation**.

```
name        poc-torch-6898598fc9-zd8rq
namespace   ate-poc-torch
node        ipp1-1984
podIP       192.168.33.54
labels      ate.dev/worker-pool=poc-torch

containers:
  ateom
    image       localhost:5001/ateom-microvm-…@sha256:e79992d60d627210…
    privileged  true
    resources   limits/requests: nvidia.com/GA100_A100_PCIE_40GB: "1"

volumes     atunnel-egress-trust, atunnel-identity, dev-kvm,
            kube-api-access-…, run-ateom
```

**The check that makes every later result non-vacuous** — the BDF must reach the
process, or the VM boots with no device and everything downstream passes while
proving nothing:

```console
$ kubectl -n ate-poc-torch exec <pod> -c ateom -- env | grep PCI_RESOURCE
PCI_RESOURCE_NVIDIA_COM_GA100_A100_PCIE_40GB=0000:65:00.0
```

`resolveWorkerDevices` matches the bare `PCI_RESOURCE_` prefix, so it picked up
a resource name that did not exist when the code was written. **No Go changes
were needed for a different GPU generation.**

Host-side, throughout:

```
driver_override: vfio-pci
bound to:        vfio-pci
iommu_group:     1
```

---

## 4. Fixtures

### 4a. Bare CUDA context — `demos/gpu/poc-cuda-checkpoint.yaml.tmpl`

Sandbox `microvm-gpu` (2048 MiB guest), image
`nvcr.io/nvidia/cuda:12.6.2-devel-ubuntu24.04@sha256:738fba0f…`.

```yaml
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: poc-cudackpt
  namespace: ate-poc-cudackpt
  labels: {workload: poc-cudackpt}
spec:
  replicas: 1
  sandboxClass: microvm
  sandboxConfigName: microvm-gpu
  ateomImage: ko://github.com/agent-substrate/substrate/cmd/ateom-microvm
  template:
    resources:
      requests: {nvidia.com/GA100_A100_PCIE_40GB: "1"}
      limits:   {nvidia.com/GA100_A100_PCIE_40GB: "1"}
```

The workload `dlopen`s `libcuda.so.1` (it arrives via CDI injection from the
guest driver, not from the image) and holds:

```c
cuInit(0); cuDeviceGet(&dev, 0); cuCtxCreate_v2(&ctx, 0, dev);
cuMemAlloc_v2(&dptr, 64 MiB);
cuMemcpyHtoD_v2(dptr, pattern, 4096);      // byte i = i*31+7
// every 5s: cuMemcpyDtoH_v2(back, dptr, 16) and check
```

Timing knobs: `HOLD_DELAY_SECONDS=90`, `CHECKPOINT_AFTER=120`,
`RESTORE_AFTER=420`.

**On the timers.** `RESTORE_AFTER` is a guest-seconds sleep spanning the cycle,
and every fixture here originally used one. It is the wrong instrument twice
over: 2400 wasted 40 minutes on a result decided in seconds, and 60 fired the
untoggle *before* the driver rebind —

```
UNTOGGLE pid 129 rc=1 : Error initializing CUDA:      untoggle total 80ms
```

The vLLM fixture now sleeps a modest amount to get the guest past the frozen
window, then polls `nvidia-smi -L` until the device answers. That poll needed
**6 seconds**.

### 4b. PyTorch — `demos/gpu/poc-torch-checkpoint.yaml.tmpl`

Sandbox `microvm-gpu-poc` (8192 MiB guest), image
`nvcr.io/nvidia/pytorch:26.07-py3@sha256:2140e699…`.

```python
n = (512 * 1024 * 1024) // 4
g = torch.Generator(device="cuda").manual_seed(1234)
keep = torch.rand(n, generator=g, device=dev, dtype=torch.float32)
want = sha256(keep.cpu().numpy().tobytes())[:16]      # e4cda77b566f6763

stream = torch.cuda.Stream()
a = torch.randn(2048, 2048, device=dev)
b = torch.randn(2048, 2048, device=dev)
while True:
    with torch.cuda.stream(stream):
        for _ in range(20):
            c = a @ b
            a = torch.tanh(c) * 0.01 + a * 0.99
    torch.cuda.synchronize()
    # recompute the digest and report
```

Exercises the Runtime API, the caching allocator, cuBLAS kernels and a
non-default stream. `TENSOR_MIB=512`, `CHECKPOINT_AFTER=150`,
`RESTORE_AFTER=420`.

### 4c. Kernel correctness — `demos/gpu/poc-kernel-correctness.yaml.tmpl`

Sandbox `microvm-gpu-poc` (8192 MiB guest), same PyTorch image. Three checks,
every output buffer zeroed before its check so a kernel that silently does not
run fails the same way one that returns garbage does:

```python
exact_out.zero_(); torch.matmul(ones, ones, out=exact_out)
assert int((exact_out != 1024.0).sum()) == 0          # exact, no tolerance

ref = (a.cpu().double() @ b.cpu().double()).float()    # CPU reference, computed once
err = (a @ b).cpu().sub(ref).abs().max()               # tolerance for irregular data

custom_out.zero_(); mod.affine_launch(x, custom_out)   # nvcc-built 3*x+7
assert int((custom_out != 3*x + 7).sum()) == 0
```

The custom kernel is compiled in-container with `torch.utils.cpp_extension.load_inline`.

**The checkpoint is gated on readiness, not a timer.** A first attempt used a
fixed `CHECKPOINT_AFTER=180s`; `nvcc` took 3.5 minutes, so the checkpoint fired
before any CUDA context existed, toggled zero PIDs, and still printed
`CHECKPOINTED`. The workload now writes `/tmp/ready` after its first passing
round, the wrapper waits for it, and refuses to claim a checkpoint when the PID
list is empty.

### 4d. vLLM — `demos/gpu/poc-vllm-checkpoint.yaml.tmpl`

Sandbox `microvm-gpu-vllm` (16384 MiB guest), `vllm/vllm-openai:v0.11.0`, serving
`Qwen2.5-0.5B-Instruct` at `gpu_memory_utilization=0.10`. Weights are pulled from
an in-cluster `modelhost` Service over plain HTTP rather than from
`huggingface.co`, so the fixture has no external dependency.

vLLM forks, so the launched PID is not the one holding the context. The fixture
prints **both** enumerations and toggles the `/proc/*/maps` list:

```sh
SMIPIDS=$(nvidia-smi --query-compute-apps=pid --format=csv,noheader | tr -d ' ' | tr '\n' ' ')
for d in /proc/[0-9]*; do
  pid=${d#/proc/}; [ "$pid" = "$$" ] && continue; [ -r "$d/maps" ] || continue
  grep -qE '(/dev/nvidia|libcuda\.so|libcudart\.so|libnvidia-ml\.so)' "$d/maps" 2>/dev/null \
    && MAPPIDS="$MAPPIDS $pid"
done
```

Toggles are reported per PID and never abort the run: a process with `libcuda`
mapped but no context returns an initialization error, which is logged and
skipped.

### Constraints that shaped every fixture

**The golden-snapshot window.** A pool's warm-up actor is snapshotted shortly
after start. Snapshotting it while it holds a context would fail to detach,
leaving the pool with no golden snapshot to create actors from. Every fixture
idle for `HOLD_DELAY_SECONDS` first, so the warm-up snapshot happens against an
idle GPU.

**No exec into an actor, and no shell in the guest** (busybox only, no
`/bin/sh`). The checkpoint is therefore **self-timed from inside the workload**.
Guest time stops while suspended, so `sleep $RESTORE_AFTER` spans the whole
cycle and fires once the actor is running again — that is how the toggle-back
happens after restore.

### Delivering `cuda-checkpoint`

Embedded in the ActorTemplate as base64 — 5,976 bytes, sha256
`707fa7f54136824d6c1d6dd724b9b1717610f831033c00d06da474de363a06db`, verified
in-container before use.

Nine fixture revisions went into getting it there. None of the obstacles were
the cluster: no `curl` in the CUDA image; `apt` broken (`Method http has died
unexpectedly`, code 112); `openssl s_client` corrupts a binary stream (correct
`Content-Length`, different bytes every fetch); YAML block-scalar vs heredoc
terminator indentation. **Egress works** — a binary-free `bash /dev/tcp` probe
returned `HTTP/1.1 200 OK` from `example.com:80`.

A production implementation should stage it as a SandboxConfig asset instead.
Assets are a generic `map[arch]map[name]AssetFile`, so fetching needs no code
change — but they land **host-side**, and this has to run inside the guest
container. Bridging that needs an ateom change shaped like
`withNVIDIADriverBindMount`.

---

## 5. Procedure

Identical for every fixture. The control is that **only the checkpoint differs**
between the two suspends.

```bash
# 1. deploy (ko apply on the node — the registry is host-local)
envsubst < demos/gpu/poc-cuda-checkpoint.yaml.tmpl | ko apply -f -

# 2. wait for the pool's golden snapshot (taken during the idle window)
kubectl-ate get actors -a ate-golden

# 3. create and start an actor
kubectl-ate create atespace poc
kubectl-ate create actor -t ate-poc-cudackpt/poc-cudackpt -a poc poc-1
kubectl-ate resume actor -a poc poc-1

# 4. confirm the context is live before proving anything about it
#    -> "CONTEXT OPEN", "VRAM HELD 64 MiB", "integrity=OK"

# 5. BASELINE: suspend while the context is held
kubectl-ate suspend actor -a poc poc-1     # expected to FAIL

# 6. after CHECKPOINT_AFTER the fixture self-checkpoints
#    -> "state before: running" / "TOGGLE rc=0" / "state after: checkpointed"

# 7. suspend again, now checkpointed
kubectl-ate suspend actor -a poc poc-2
kubectl-ate resume  actor -a poc poc-2

# 8. the fixture toggles back on its own after RESTORE_AFTER guest-seconds
#    -> digest must match the pre-cycle value
```

---

## 6. Results

### 6a. Baseline — context held (the control)

```
$ kubectl-ate suspend actor -a poc poc-1
Error: failed to suspend actor: rpc error: code = Internal desc =
  while confirming eject of _vfio5 (the guest may still hold the device:
  nvidia-persistenced keeps it open unless nvidia-smi -pm 0 ran in a container):
  device _vfio5 not ejected within 30s: device _vfio5 still in the device tree
exit=1  wall=34.7s
```

**`nvidia-smi -pm 0` ran and succeeded** — ateom does this before every detach.
It was not sufficient. Persistence mode and a CUDA context are independent
references on `usage_count`; the error message's hint about
`nvidia-persistenced` is a guess, and in this case a misleading one.

### 6b. Checkpointed — bare context

```
state before: running
TOGGLE rc=0
state after : checkpointed

$ kubectl-ate suspend actor -a poc poc-2
STATUS_SUSPENDED     exit=0  wall=25.9s
```

ateom's structured log:

```json
{"msg":"Detached passthrough devices for snapshot","devices":1,"took":3178015745}
{"msg":"Actor checkpointed","pause":1701734,"snapshot":1443913663,"teardown":4386553147}
{"msg":"restoring guest memory","mode":"Copy"}
{"msg":"Re-attached passthrough devices after restore","devices":1,"took":12214750489}
{"msg":"The re-attached GPU is not bound; re-probing the guest driver"}
{"msg":"Actor restored (overlay rootfs)","total":32787467334}
```

### 6c. Checkpointed — PyTorch

```
torch 2.13.0a0+9186a08b2c.nv26.07 on NVIDIA A100-PCIE-40GB
persistent tensor 512 MiB, digest e4cda77b566f6763
matmuls=20   allocated=568MiB digest=e4cda77b566f6763 integrity=OK
...
TOGGLE rc=0    state after : checkpointed
   -> suspend  exit=0  wall=325.3s
   -> resume   wall=65.7s
UNTOGGLE rc=0
matmuls=420  allocated=568MiB digest=e4cda77b566f6763 integrity=OK
matmuls=440  allocated=568MiB digest=e4cda77b566f6763 integrity=OK
```

Digest unchanged; the counter resumed from where it stopped. It has since run
past 112,000 matmuls with `integrity=OK` throughout.

### 6d. Kernel correctness across the cycle

```
14:38:23  GPU compute PIDs: 87  (launched 87)
14:38:29  TOGGLE pid 87 rc=0 in 465ms      state: checkpointed
          round=13  exact=ok(mismatched=0) ref=ok(maxerr=3.343e-03) custom=ok(mismatched=0)
          [ Actor checkpointing / checkpointed / restoring / restored ]
          suspend exit=0 wall=87.4s        resume wall=67.4s
15:18:31  UNTOGGLE pid 87 rc=0 in 2010ms
15:18:34  round=14  exact=ok(mismatched=0) ref=ok(maxerr=3.343e-03) custom=ok(mismatched=0)
```

`maxerr` is bit-identical either side, not merely within tolerance. Still
passing at round 138.

### 6e. vLLM — enumeration decides the outcome

Same fixture, same model, same `gpu_memory_utilization`. The only variable is
**which processes get toggled**.

```
run 1 — pids from nvidia-smi --query-compute-apps
  GPU compute PIDs: 162  (launched pid 92)
  TOGGLE pid 162 rc=4147ms                     running -> checkpointed
  suspend: while confirming eject of _vfio4: device _vfio4 not ejected within 30s

run 2 — pids from /proc/*/maps
  PIDS via nvidia-smi   : 201
  PIDS via /proc/*/maps : 129 201
    pid 129  /usr/bin/python /usr/local/bin/vllm serve /tmp/model --port 8000 ...
    pid 201  VLLM::EngineCore
  state before pid 129: running        pid 201: running
  TOGGLE pid 129 rc=0                  TOGGLE pid 201 rc=0     total 4781ms
  state after  pid 129: checkpointed   pid 201: checkpointed
  Detached passthrough devices for snapshot   took 3.71s
  suspend exit 0, 155s wall -> STATUS_SUSPENDED       resume 99s

run 3 — same, with the untoggle gated on the device instead of a timer
  TOGGLE   pid 127 rc=0             TOGGLE   pid 199 rc=0     total 4816ms
  suspend exit 0, 155s wall -> STATUS_SUSPENDED       resume 100s
  device back after 6s of polling
  UNTOGGLE pid 127 rc=0             UNTOGGLE pid 199 rc=0     total 4914ms
  VERDICT: IDENTICAL -- greedy output matches across the cycle
  VERDICT: long-prompt output also identical
```

Greedy decode, so byte-identical output is a check and not a coincidence. Both a
short prompt and a 40-sentence one match across the cycle.

`nvidia-smi` never listed pid 129, yet `cuda-checkpoint --get-state --pid 129`
returned `running` and the toggle succeeded. The launcher held driver state that
the tool used to look for driver state did not report.

Run 1's conclusion — that vLLM needs native support — **was wrong, and is
retracted.** It was an under-count, not a coordination problem. Single GPU means
no collectives, so this says nothing about tensor parallelism.

Checkpoint cost still scales with allocated VRAM: 465 ms for the 1024x1024
correctness fixture, ~4.1-4.8 s for vLLM at `gpu_memory_utilization=0.10`.

### 6f. Timing summary

All figures from ateom's structured log except *wall*, which is the
`kubectl-ate` command duration.

| phase | idle GPU (2 GB) | bare ctx, checkpointed (2 GB) | PyTorch, checkpointed (8 GB) |
|---|---|---|---|
| detach | 2.15 / 3.09 s | 3.18 s | 3.32 s |
| pause | 1.68 / 1.80 ms | 1.70 ms | 2.80 ms |
| snapshot | 1.42 / 1.44 s | 1.44 s | **10.11 s** |
| teardown | 4.51 / 4.36 s | 4.39 s | 7.97 s |
| memory restore mode | `Copy` (eager) | `Copy` | `Copy` |
| re-attach | 12.17 s | 12.21 s | 12.57 s |
| driver rebind | required | required | required |
| total restore | 33.23 s | 32.79 s | 37.47 s |
| suspend wall | 25.6 s | 25.9 s | **325.3 s** |
| resume wall | 42.6 s | 55.3 s | 65.7 s |

**The suspend/resume path is insensitive to the checkpoint.** Detach and
re-attach are within noise across all three columns.

### 6g. Snapshot size

`aws s3 ls --recursive s3://ate-snapshots/ate-poc-torch/`:

| object | bytes |
|---|---|
| golden (idle GPU, 8 GB guest) `memory-ranges.zstd` | 148,939,792 |
| PyTorch, checkpointed, `memory-ranges.zstd` | **1,132,129,674** |
| `state.json.zstd` (PyTorch) | 89,189 |
| `config.json.zstd` | 1,388 |

**7.6× growth.** A checkpoint drains device memory into guest RAM, and guest RAM
is what the snapshot captures. 512 MiB tensor + 568 MiB allocator footprint
lands in the memory image — and is read back **eagerly** on restore, because a
passthrough device forbids lazy restore (VFIO must pin guest memory to map it
into the IOMMU; userfaultfd-backed pages cannot be pinned).

**This is the number that decides whether the technique is practical.** A
workload holding 20 GB on the card needs a guest sized for it and a snapshot
roughly that much larger.

### 6h. Unexplained latency

PyTorch suspend wall was **325.3 s**, of which `CheckpointWorkload` was
**21.4 s**:

```
19:35:5x  suspend issued
19:39:59  ateom begins CheckpointWorkload      <- ~4 min gap
19:40:02  Detached passthrough devices
19:40:20  Actor checkpointed  (RPC elapsed 21.4s)
19:41:07  memory-ranges.zstd written to rustfs
```

Roughly four minutes elapsed in the control plane before ateom was asked to do
anything. **Not investigated.** The 2 GB fixture did not show it (25.6 s wall),
so it may scale with something — but that is a hypothesis, not a finding.

---

## 7. Defects found

### 7a. A failed suspend strands the actor and leaks its GPU

Severity: **reachable on merged code today**, by any GPU actor doing real work.

```
$ kubectl-ate resume actor -a poc poc-1
Error: FailedPrecondition: AssignWorker prerequisite not met for Actor: poc/poc-1
       (got: STATUS_SUSPENDING, want STATUS_SUSPENDED or STATUS_PAUSED)

$ kubectl-ate delete actor -a poc poc-1
Error: FailedPrecondition: Actor poc/poc-1 is not in a deletable status
       (status: STATUS_SUSPENDING)

$ kubectl-ate get workers -a poc
POOL           POD                             STATUS     ASSIGNED ACTOR
poc-cudackpt   poc-cudackpt-…-6zsnq            ASSIGNED   …/poc/poc-1
```

The guest is unharmed throughout — the CUDA context stayed alive with VRAM
intact for **three hours** while the actor was unreachable. On a single-GPU node
the card is unreclaimable.

Only recovery found: cycle the WorkerPool to `replicas: 0` and back, which moves
the actor to `STATUS_CRASHED` (from which it *is* deletable) — **and kills every
other actor on that pool**.

The design fails closed on **data integrity**: `errIfPassthroughSnapshot`
refuses to snapshot with a device attached, so no torn memory image is ever
produced. That guarantee holds. It does not fail cleanly on **state**: the
suspend error path never rolls the actor's status back.

*Caveat: the missing rollback is inferred from behaviour. The exact site in
ateapi's `ActorWorkflow` has not been read.*

### 7b. `ActorTemplate` spec is immutable

```
The ActorTemplate "poc-cudackpt" is invalid: spec: Invalid value: Spec is immutable
```

Correct behaviour — a changed template would invalidate its snapshots — but it
means every fixture revision needs a new template name. Eleven were created
during this work. `workerSelector` lets them share one WorkerPool.

---

## 8. What was not tested

- **In-flight work.** The device was quiesced before every checkpoint, which
  sidesteps precisely the restriction NVIDIA documents.
- **Multi-GPU**, IPC / peer access, CUDA graphs, NCCL. vLLM ran at TP=1, so no
  collectives existed to tear down; `vllm-project/vllm#37921` may still be
  required above TP=1.
- **Restoring onto a different physical card.** Actors resume on whichever
  worker is free; behaviour across GPU UUIDs is unknown, and this is a
  single-GPU node so it could not be tested.
- **Large VRAM footprints.** 568 MiB was moved. The interesting regime is tens
  of GB, where snapshot size and eager restore dominate.
- **Repeat runs.** Each figure is a single observation, not a distribution.

---

## 9. Cluster defects encountered

Neither is a GPU issue, but both cost time and one invalidated an earlier
finding, so they are recorded here.

### atenet-egress serves an expired certificate

Actor egress died mid-run. The symptom inside an actor was
`ConnectionResetError: [Errno 104]` on every outbound connection, and **the
egress gateway logged nothing at all** — the TLS handshake fails before any
request exists, so there is nothing to log.

```
atunnel: egress gateway TLS handshake: tls: failed to verify certificate:
  x509: certificate has expired or is not yet valid

atenet-egress serving cert:  notBefore Aug 19 11:54:17 2026
                             notAfter  Aug 20 11:54:17 2026
```

Rotation itself works — `api.ate-system.svc` and `atenet-router.ate-system.svc`
both held certs renewed that morning. The gateway simply never **reloads** its
rotated cert; it serves whatever it read at startup. `kubectl rollout restart
deploy/atenet-egress` produced a valid one immediately.

Effect: actor egress silently dies roughly 24 hours after the gateway pod
starts, and the failure surfaces far from its cause.

**This invalidated a finding.** "huggingface.co is reset from an actor while
github works" compared measurements taken either side of the expiry. Nothing was
special about HuggingFace.

### Model fetching, once egress was restored

`huggingface.co` is reachable from ordinary pod networking. The vLLM fixture
still fetches from an in-cluster `modelhost` Service over plain HTTP — no SigV4,
no TLS, no external dependency — which is more robust for a fixture and proves
in passing that an actor can reach a **ClusterIP**, consistent with the egress
path dialing IP:port from the CONNECT authority.
