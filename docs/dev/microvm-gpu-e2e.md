# Running a GPU micro-VM actor end to end

Brings up a micro-VM actor with a passthrough GPU, suspends it, and resumes it.
Needs a real GPU host: everything below turns on behaviour of the NVIDIA driver,
NVRC and the guest kernel, none of which can be exercised off hardware.

Validated against a Tesla T4 host (`s2029gp-tr-0139`), kata-static 4.0.0,
cloud-hypervisor v52.

## What this is actually testing

Two claims in the design are **inferences that have never been observed**, and
this runbook exists mainly to settle them. Both are called out at the step that
decides them:

| | Claim | Decided at |
|---|---|---|
| A | NVRC boots with a non-dm-verity root | step 4 |
| B | A container's `/dev/nvidia*` still work after detach and re-attach | step 8 |

If either fails, the design section it belongs to (§4.4, §4.3) names the
fallback. Neither failure means the approach is wrong.

A third thing worth stating: the workload is a CUDA base image on purpose. An
actor that never opens the device reaches Ready, suspends and resumes cleanly on
a worker whose GPU is completely unusable — so `nvidia-smi` inside the actor,
not actor status, is the assertion throughout.

## 0. Host prerequisites

```bash
# IOMMU must be on, or VFIO cannot isolate the device for passthrough.
ls /sys/kernel/iommu_groups | wc -l          # must be > 0

# The GPU Operator in vm-passthrough mode binds the GPU to vfio-pci.
lspci -d 10de: -k | grep -A2 .               # "Kernel driver in use: vfio-pci"
ls /dev/vfio/                                # the group node, plus "vfio"
```

`Kernel driver in use:` is the state line. `Kernel modules:` is a list of
*candidates* and says nothing about what is bound — misreading one for the other
is easy and has cost time before.

```bash
# The device plugin advertises a MODEL-SPECIFIC resource in passthrough mode.
kubectl describe node <gpu-node> | grep -i nvidia.com
```

Note the exact resource name; `nvidia.com/gpu` is the *time-slicing* name and
will not be present. Put whatever you see into the WorkerPool.

## 1. Build and stage the guest assets

```bash
hack/microvm-assets/assemble-gpu.sh          # ~2GB download
```

Produces `vmlinux-gpu`, `rootfs-gpu.img` and `configuration-clh-gpu.toml` in
`bin/microvm-gpu-assets/`, and prints their sha256 sums. Sanity-check the
generated config before staging — it is what makes the guest bootable:

```bash
cat bin/microvm-gpu-assets/configuration-clh-gpu.toml
# rootfs_type must be "erofs" (the stock guest is ext4)
# kernel_params must carry pci=realloc pci=nocrs pci=assign-busses
```

Stage all three into `gs://${BUCKET_NAME}/kata-gpu-assets/`, then apply the
sandbox config with the printed sums:

```bash
export GPU_KERNEL_SHA256=... GPU_ROOTFS_SHA256=... GPU_CONFIG_SHA256=...
envsubst < manifests/microvm/sandboxconfig-microvm-gpu.yaml.tmpl | kubectl apply -f -
```

## 2. Deploy ateom and the fixture

```bash
export GPU_WORKLOAD_IMAGE=nvcr.io/nvidia/cuda:12.6.2-base-ubuntu24.04
envsubst < demos/gpu/gpu-microvm.yaml.tmpl | ko apply -f -
kubectl -n ate-demo-gpu-microvm get workerpool,pods -w
```

The worker pod must reach Running with the GPU resource granted. If it stays
Pending, the resource name in the WorkerPool does not match what the node
advertises (step 0).

## 3. Confirm ateom sees the allocation

```bash
kubectl -n ate-demo-gpu-microvm exec <worker-pod> -c ateom -- env | grep PCI_RESOURCE_
```

Expect `PCI_RESOURCE_NVIDIA_COM_<MODEL>=<BDF>`. ateom matches the bare
`PCI_RESOURCE_` prefix, so any vendor's plugin works, but the value must be a
host BDF. **No output means the actor's VM will boot with no device** — every
later step would then pass vacuously.

## 4. Boot an actor — decides claim A

```bash
kubectl-ate create actor --template gpu-microvm -n ate-demo-gpu-microvm gpu-1
kubectl -n ate-demo-gpu-microvm logs <worker-pod> -c ateom | tail -40
```

If the guest does not boot, read the serial log — the guest kernel writes its
root-mount failure there and nothing else will explain it:

```bash
kubectl -n ate-demo-gpu-microvm exec <worker-pod> -c ateom -- \
  cat /run/ateom/vm/<actor-uid>/serial.log | tail -60
```

**Claim A fails here** if the log shows the root device mounting but NVRC then
refusing to continue, or demanding a verity device. The fix is in design §4.4:
switch to the `dm-mod.create=...` cmdline and read `root_hash`/`salt`/
`data_blocks` from the `configuration-qemu-nvidia-gpu.toml` shipped with *that*
image build — they change on every rebuild, so they cannot be hardcoded.

A root-filesystem error instead (`unknown filesystem type`, `unable to mount
root fs`) means `rootfs_type` did not reach `buildVMConfig`; check the staged
config actually says `erofs`.

## 5. The GPU is usable in the actor — the real assertion

```bash
kubectl-ate exec -n ate-demo-gpu-microvm gpu-1 -- nvidia-smi
```

Expect the T4 listed with a driver version. This is the step the whole change
exists for, and the one a non-GPU workload would have skipped.

If `nvidia-smi` is missing, the image lacks the driver userspace. If it runs but
reports **no devices**, the CDI injection did not happen — check the annotation
reached the agent:

```bash
kubectl -n ate-demo-gpu-microvm exec <worker-pod> -c ateom -- \
  cat /run/ateom/vm/<actor-uid>/serial.log | grep -i cdi
```

and confirm the guest generated a spec at all (NVRC writes it before forking the
agent, so an empty file means NVRC's GPU mode did not run):

```bash
# via the guest debug console
ls -l /var/run/cdi/
```

`Permission denied` opening `/dev/nvidia0` rather than "no devices" would mean
the device cgroup was not widened — CDI does that itself, so that would be a
kata-agent-side surprise worth capturing.

## 6. Suspend — the detach path

```bash
kubectl-ate suspend actor -n ate-demo-gpu-microvm gpu-1
kubectl -n ate-demo-gpu-microvm logs <worker-pod> -c ateom | grep -i detach
```

Expect `Detached passthrough devices for snapshot` with the guest BDFs and a
duration. Failure modes, in the order they are checked:

- `still bound to a driver after unbind` — the guest would not release it.
  Usually something still holds the device; `nvidia-persistenced` is stopped
  first, but a live CUDA context in the workload would also do it.
- `while confirming eject` — the VMM never dropped its `/dev/vfio` group fd
  within 30s. The eject was requested but the guest did not complete it.
- `refusing to snapshot: N passthrough device(s) still attached` — the detach
  reported success without finishing. This is the assertion that stops a torn
  snapshot; it should be unreachable.

Confirm the worker went back to free, which is the actual product claim — the
GPU is available to another actor while this one is suspended:

```bash
kubectl -n ate-demo-gpu-microvm get workers
```

## 7. Resume — the attach path

```bash
kubectl-ate resume actor -n ate-demo-gpu-microvm gpu-1
kubectl -n ate-demo-gpu-microvm logs <worker-pod> -c ateom | grep -i "Re-attached"
```

`guest enumerated 0 of 1` means the hot-plugged device never appeared on the
guest PCI bus — the guest did not process the hot-plug event. `did not bind to
the nvidia driver` means it appeared but the driver would not claim it, even
after the explicit bind.

## 8. The GPU works again — decides claim B

```bash
kubectl-ate exec -n ate-demo-gpu-microvm gpu-1 -- nvidia-smi
```

**Claim B fails here** if `nvidia-smi` reports no devices *while* step 7 logged
a successful bind. That combination is the specific signal: the driver owns the
device, but the container's pre-existing `/dev/nvidia*` nodes no longer resolve
to it.

The fallback is design §4.3 / C5: after re-attach, create the nodes into
`/proc/<pid>/root/dev/` from the guest and widen the cgroup via
`UpdateContainer`, driven by the guest CDI spec. Capture the evidence first —
inside the actor, and via the debug console:

```bash
ls -l /dev/nvidia*                    # in the actor: do the nodes still exist?
cat /proc/devices | grep nvidia       # in the guest: what major does the driver hold now?
```

If the major changed across the cycle, that is the mechanism, and it is worth
recording: the design argues `nvidia-uvm`'s dynamic major is stable *because the
modules stay resident*, and a changed major would falsify that.

## 9. Repeat the cycle

Suspend and resume once more. A second cycle is not ceremony: the first resume
re-attaches into a VM whose device tree was serialized without the device, and
the second checks the state after a *restored* actor is snapshotted again —
which is a different code path (`restoreSourceDir` is set, so the snapshot is a
diff that gets merged).

## Cleanup

```bash
kubectl-ate delete actor -n ate-demo-gpu-microvm gpu-1
kubectl delete ns ate-demo-gpu-microvm
```
