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

### The podCertificate feature gate

Substrate's components authenticate to each other with certificates delivered by
a `podCertificate` projected volume. That API is alpha, and **a disabled alpha
field is pruned at admission rather than rejected** — the projection vanishes
from the pod spec, no signing request is ever filed, and every component that
reads a credential bundle exits on a file that was never written. Enabling it on
the kubelet alone does nothing; the API server does the pruning.

```bash
# Must list PodCertificateRequest=true.
kubectl -n kube-system get pod -l component=kube-apiserver \
  -o jsonpath='{.items[0].spec.containers[0].command}' | tr ',' '\n' | grep feature-gates
```

On kubeadm, add it to `--feature-gates` in
`/etc/kubernetes/manifests/kube-apiserver.yaml` (back the file up first — a bad
flag leaves you with no API server) and the static pod restarts itself.
`hack/create-kind-cluster.sh:80` does the equivalent for kind. This is a
cluster-level setting and survives reinstalling substrate.

The decisive check is whether the field survives a round trip, not whether the
flag is spelled right:

```bash
kubectl apply --dry-run=server -o json -f - <<'EOF' | grep -c podCertificate  # want > 0
apiVersion: v1
kind: Pod
metadata: {name: pcr-probe, namespace: default}
spec:
  containers: [{name: c, image: registry.k8s.io/pause:3.10,
                volumeMounts: [{name: id, mountPath: /run/id}]}]
  volumes:
  - name: id
    projected:
      sources:
      - podCertificate:
          signerName: podidentity.podcert.ate.dev/identity
          keyType: ECDSAP256
          credentialBundlePath: credential-bundle.pem
EOF
```

### Installing substrate

Install rather than upgrade. A cluster installed from an older checkout fails in
a way that is expensive to debug: each component starts, gets further than the
last, and stops on a different missing input — a ClusterRole verb, a CRD, a
serialized proto, a CA root. controller-runtime turns two of those into an
indefinite cache-sync wait with no error line, which reads exactly like "the
code doesn't do that yet." See the appendix for the specific ones.

```bash
export KUBECONFIG=/etc/kubernetes/admin.conf     # kubeadm control plane
cd <repo>

ATE_INSTALL_KIND=true KO_DOCKER_REPO=localhost:5001 \
  ./hack/install-ate.sh --delete-ate-system

ATE_INSTALL_KIND=true KO_DOCKER_REPO=localhost:5001 \
  ./hack/install-ate.sh --deploy-ate-system
```

`ATE_INSTALL_KIND=true` is load-bearing on any off-GCP cluster, not just kind:
it selects `manifests/ate-install/kind`, which is where rustfs and atelet's
`ATE_STORAGE_BACKEND=s3` / `AWS_ENDPOINT_URL` env live. Without it the base
manifests install an atelet that cannot fetch any asset.

`KO_DOCKER_REPO=localhost:5001` means `ko` must run **on the node** — the
registry is host-local and not reachable from a laptop.

Note what the delete takes with it: `rustfs.yaml` in that overlay owns the
`rustfs-data` PVC, so **every staged asset is destroyed** and step 2 is not
optional. Nothing else in this runbook survives either — SandboxConfigs are
cluster-scoped CRs and the CRDs are deleted.

## 1. Build and stage the guest assets

Two sets: the stock micro-VM assets that every actor needs, and the GPU guest.

```bash
# Stock. ARCH defaults to arm64 -- on an x86 host that silently produces
# binaries the node cannot execute.
ARCH=amd64 hack/microvm-assets/assemble.sh
OUT=bin/microvm-assets/amd64 hack/microvm-assets/stage-to-rustfs.sh

export BUCKET_NAME=ate-snapshots
export VIRTIOFSD_SHA256=$(sha256sum bin/microvm-assets/amd64/virtiofsd | cut -d' ' -f1)
envsubst < manifests/microvm/sandboxconfig-microvm.yaml.tmpl | kubectl apply -f -
```

```bash
hack/microvm-assets/assemble-gpu.sh          # ~2GB download
```

Produces `vmlinux-gpu`, `rootfs-gpu.img` and `configuration-clh-gpu.toml` in
`bin/microvm-gpu-assets/`, and prints their sha256 sums. Sanity-check the
generated config before staging — it is what makes the guest bootable:

```bash
cat bin/microvm-gpu-assets/configuration-clh-gpu.toml
# rootfs_type must be "erofs" (the stock guest is ext4)
# kernel_params must NOT contain pci=nocrs -- assemble-gpu.sh strips it, because
# it makes the guest ignore clh's real MMIO windows and break hot-plug
```

Stage all three and apply the sandbox config with the printed sums:

```bash
hack/microvm-assets/stage-gpu-assets.sh      # uploads, then prints the exports
export GPU_KERNEL_SHA256=... GPU_ROOTFS_SHA256=... GPU_CONFIG_SHA256=...
envsubst < manifests/microvm/sandboxconfig-microvm-gpu.yaml.tmpl | kubectl apply -f -
```

## 2. Deploy ateom and the fixture

```bash
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

There is no `exec` into an actor, so the workload reports on itself: it prints
its device nodes and `nvidia-smi` every five seconds, and ateom forwards that
into the worker pod's log.

```bash
kubectl-ate logs actors -n ate-demo-gpu-microvm gpu-1 --follow
```

Expect the T4 listed with a driver version. This is the step the whole change
exists for, and the one a non-GPU workload would have skipped. Leave the follow
running through steps 6–8: the log going quiet at suspend and resuming after is
the same evidence, seen once.

If `nvidia-smi` is missing, the image lacks the driver userspace. If it runs but
reports **no devices**, the CDI injection did not happen — check the annotation
reached the agent:

```bash
kubectl -n ate-demo-gpu-microvm exec <worker-pod> -c ateom -- \
  cat /run/ateom/vm/<actor-uid>/serial.log | grep -i cdi
```

The guest cannot be inspected directly to confirm NVRC wrote a spec: its rootfs
has `/bin/busybox` and no `/bin/sh`, so kata-agent's debug console cannot spawn a
shell and no command runs there. The container is the only vantage point, which
is why every check in this runbook is made from inside the actor.

`Permission denied` opening `/dev/nvidia0` rather than "no devices" would mean
the device cgroup was not widened — CDI does that itself, so that would be a
kata-agent-side surprise worth capturing.

## 6. Suspend — the detach path

```bash
kubectl-ate suspend actor -n ate-demo-gpu-microvm gpu-1
kubectl -n ate-demo-gpu-microvm logs <worker-pod> -c ateom | grep -i detach
```

Expect `Detached passthrough devices for snapshot` with a device count and a
duration. ateom asks the guest nothing here: `vm.remove-device` raises an ACPI
eject and the guest kernel's hotplug path calls the driver's `.remove()` itself
(verified on a T4). Failure modes, in the order they are checked:

- `while confirming eject` — the VMM never dropped its `/dev/vfio` group fd
  within 30s. The eject was requested and the guest did not complete it. The
  likeliest cause is a live CUDA context in the workload pinning the device,
  which is why suspend expects an idle GPU. The fixture's `nvidia-smi` holds the
  device for well under a second between five-second sleeps, so a suspend that
  lands on one still completes inside the timeout.
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

`only 0 of 1 passthrough device(s) came back` means the VMM never took the
device. If it 500s with `VfioDmaMap(IommuDmaMap(Error(14)))`, the actor was
restored with `OnDemand` memory: VFIO must pin guest memory to map it into the
IOMMU and userfaultfd-backed pages cannot be pinned, so `restoreFullScope`
selects `Copy` whenever the worker holds a passthrough device.

ateom can only confirm the device returned to the VMM. Whether the guest driver
rebound is not observable from the host on this image — step 8 is what settles
it.

## 8. The GPU works again — decides claim B

The same log stream answers this: after resume, the loop's lines come back.

```bash
kubectl-ate logs actors -n ate-demo-gpu-microvm gpu-1 | tail -20
```

**Claim B fails here** if the loop prints `nvidia-smi FAILED` *while* step 7
logged a successful bind. That combination is the specific signal: the driver
owns the device, but the container's pre-existing `/dev/nvidia*` nodes no longer
resolve to it.

The fallback is design §4.3 / C5: after re-attach, create the nodes into
`/proc/<pid>/root/dev/` from the guest and widen the cgroup via
`UpdateContainer`, driven by the guest CDI spec.

Compare the `major:minor` pairs the loop prints against the ones from before the
cycle. If they changed, that is the mechanism: the design argues `nvidia-uvm`'s
dynamic major is stable *because the modules stay resident*, and a changed major
would falsify that.

No output at all after resume means the forwarding did not re-attach rather than
the GPU failing — `restore.go` reopens `ReadStdout`/`ReadStderr` per container,
so check the worker log for that before suspecting the device.

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

## Appendix: what a partially-upgraded cluster looks like

Deploying new binaries onto a cluster installed from an older checkout produced
six failures in a row, each only visible once the previous was fixed. They are
recorded because none of them names its own cause, and four of the six read as a
bug in the component being deployed.

| Symptom | Actual cause |
|---|---|
| ateom exits: `reading credential bundle: no such file or directory` | API server missing `PodCertificateRequest=true`; the projection was pruned |
| Controller Running, but the WorkerPool Deployment never changes | Its NetworkPolicy informer is `403 forbidden`, so the manager never finishes cache sync. No error, no crash — it just never reconciles. The old pod stays Running and serving |
| api-server CrashLoop, `/readyz` refused | Same shape: an informer for `csidriverconfigs.ate.dev`, a CRD the cluster never had. `ensure_crds` short-circuits on the three CRDs that do exist |
| Client: `cannot parse invalid wire-format data` | #737 wrapped the flat `ateom_pod_*` fields into a `WorkerAssignment` message; field 5 arrives as a string where a message is expected |
| api-server: `unknown field "ateomPodNamespace"` | Actor records in Valkey serialized before #737. `kubectl-ate admin debug-flush-redis`, then restart the workers so no VM is orphaned holding a GPU |
| atelet: `credential bundle has no Pod identity` | Its cert predates the signer emitting the `oidPodIdentity` extension. Delete the pod to force a fresh `PodCertificateRequest` |

Two immutable-field errors also block re-apply, and both want delete-then-create:
`svc/api` (gains `clusterIP: None`) and `job/valkey-cluster-init` (its pod
template gains the podCertificate projection that used to be pruned).

The one that is a genuine bug rather than skew: `valkey-ca-certs` must contain
**two** roots — servicedns to verify peers' server certs, podidentity to verify
clients — and an older `install-ate.sh` built it with a single `printf` whose
substitutions `errexit` could not see fail, silently dropping one.

```bash
kubectl -n ate-system exec valkey-cluster-0 -- grep -c BEGIN /etc/valkey-ca/ca.crt   # must be 2
```

It hides for as long as nothing uses the missing root: the api-server connects
with its *servicedns* cert, and only `valkey-cluster-init` authenticates with
*podidentity*. So the cluster runs fine until the pods roll and it has to
re-form. Regenerate with `--create-valkey-ca-certs-secret`; if the nodes have
already been initialized, `FLUSHALL` + `CLUSTER RESET HARD` each of the six
before re-running the init Job, or it stops with `Node ... is not empty`.
