# Building a GPU node from a fresh VM

Takes a bare Ubuntu VM with NVIDIA GPUs to a single-node Kubernetes cluster that
substrate can run micro-VM GPU actors on. Nothing here is substrate itself —
reinstalling substrate never touches any of it. Continue with
`docs/dev/microvm-gpu-e2e.md` once the checklist at the end passes.

Every version below was read off a working node (`s2029gp-tr-0139`), so this
reproduces a known-good combination rather than a plausible one. Newer versions
are usually fine; the ones marked **required** are not free choices.

| | |
|---|---|
| OS | Ubuntu 22.04.5 LTS |
| Kernel | 6.8.0 (HWE) — **required: ≥ 6.5** |
| Container runtime | containerd 2.3.1 |
| Kubernetes | v1.36.2, kubeadm, single node |
| CNI | Calico v3.32.0 |
| Storage | local-path-provisioner v0.0.30 |
| GPU Operator | v26.3.2, `vm-passthrough` |
| GPUs | 6× Tesla T4 |

---

## 1. Kernel

**Linux 6.5 or newer is required.** atelet hands ateom a list of image layers and
ateom mounts the container rootfs as an overlay through the new mount API,
appending one `lowerdir+` per layer to sidestep `mount(2)`'s single-page option
limit (`internal/imagecache/bundle_linux.go`). Overlayfs only gained that option
in 6.5.

On an older kernel `fsopen("overlay")` *succeeds*, every `fsconfig` call
*succeeds*, and the mount fails at the very end with a bare `invalid argument`
that names neither overlayfs nor a version. Ubuntu 22.04 ships 5.15, so it needs
the HWE stack:

```bash
uname -r                                      # want >= 6.5
sudo apt update
sudo apt install -y linux-generic-hwe-22.04   # 22.04 HWE is 6.8
sudo reboot
```

## 2. IOMMU

VFIO cannot isolate a device without it, and passthrough fails at the first step
with no useful error.

```bash
sudo sed -i 's/GRUB_CMDLINE_LINUX="\(.*\)"/GRUB_CMDLINE_LINUX="\1 intel_iommu=on iommu=pt"/' \
  /etc/default/grub
sudo update-grub && sudo reboot
```

`iommu=pt` (passthrough) leaves DMA unremapped for devices not assigned to a VM,
so host-attached hardware keeps normal performance.

**Verify:**
```bash
ls /sys/kernel/iommu_groups | wc -l           # must be > 0
```

## 3. Host prerequisites for kubelet

```bash
sudo swapoff -a
sudo sed -i '/[[:space:]]swap[[:space:]]/s/^/#/' /etc/fstab

cat <<'EOF' | sudo tee /etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF
sudo modprobe overlay && sudo modprobe br_netfilter

cat <<'EOF' | sudo tee /etc/sysctl.d/k8s.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sudo sysctl --system
```

## 4. containerd

```bash
sudo apt install -y containerd
sudo mkdir -p /etc/containerd
containerd config default | sudo tee /etc/containerd/config.toml >/dev/null
sudo sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
sudo systemctl restart containerd
```

`SystemdCgroup = true` is **required** — kubelet uses the systemd cgroup driver
by default, and a mismatch produces pods that fail to start with cgroup errors
that don't mention cgroups.

## 5. Kubernetes packages

```bash
sudo apt install -y apt-transport-https ca-certificates curl gpg
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.36/deb/Release.key \
  | sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.36/deb/ /' \
  | sudo tee /etc/apt/sources.list.d/kubernetes.list
sudo apt update && sudo apt install -y kubelet kubeadm kubectl
sudo apt-mark hold kubelet kubeadm kubectl
```

## 6. kubeadm init

The feature gates are the part that is **not** a free choice. Substrate's
components authenticate to each other with certificates delivered by a
`podCertificate` projected volume, and that API is alpha: a disabled alpha field
is **pruned at admission rather than rejected**, so the volume silently
disappears from pod specs and every component dies reading a file nobody wrote.
`ClusterTrustBundle` is how they learn which CA to trust.

They must be set on the API server, controller-manager **and** scheduler, and
`certificates.k8s.io/v1beta1` must also be in `runtime-config` or the
`PodCertificateRequest` resource is not served even with the gate on.

```yaml
# kubeadm-config.yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.2
networking:
  podSubnet: 192.168.32.0/22
  serviceSubnet: 10.96.0.0/12
apiServer:
  extraArgs:
  - name: feature-gates
    value: DynamicResourceAllocation=true,ClusterTrustBundle=true,ClusterTrustBundleProjection=true,PodCertificateRequest=true
  - name: runtime-config
    value: resource.k8s.io/v1beta1=true
  - name: runtime-config
    value: resource.k8s.io/v1beta2=true
  - name: runtime-config
    value: certificates.k8s.io/v1beta1=true
controllerManager:
  extraArgs:
  - name: feature-gates
    value: DynamicResourceAllocation=true,ClusterTrustBundle=true,ClusterTrustBundleProjection=true,PodCertificateRequest=true
scheduler:
  extraArgs:
  - name: feature-gates
    value: DynamicResourceAllocation=true,ClusterTrustBundle=true,ClusterTrustBundleProjection=true,PodCertificateRequest=true
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
featureGates:
  ClusterTrustBundle: true
  ClusterTrustBundleProjection: true
  DynamicResourceAllocation: true
  KubeletPodResourcesGet: true
  PodCertificateRequest: true
  RuntimeClassInImageCriApi: true
```

```bash
sudo kubeadm init --config kubeadm-config.yaml

mkdir -p ~/.kube && sudo cp /etc/kubernetes/admin.conf ~/.kube/config \
  && sudo chown "$(id -u):$(id -g)" ~/.kube/config

# Single node: it has to run workloads.
kubectl taint node --all node-role.kubernetes.io/control-plane-
```

Put gates in this config, not in `/etc/kubernetes/manifests/kube-apiserver.yaml`.
Editing the manifest works and restarts the static pod, but the change is
invisible to the `kubeadm-config` ConfigMap and a later `kubeadm upgrade`
regenerates the manifest from it.

**Verify the gate took** — the round trip is the only check that cannot mislead.
Do not grep the API server's flags: the value is a comma-separated list, and
splitting on commas shows you the first gate and hides the rest.

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

An empty `kubectl get podcertificaterequests -A` is **not** a symptom: the
built-in cleanup controller deletes them once issued, so no objects is the
steady state.

## 7. CNI and storage

```bash
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.32.0/manifests/calico.yaml

kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.30/deploy/local-path-storage.yaml
kubectl patch storageclass local-path \
  -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
```

Calico's pod CIDR must agree with `podSubnet` above. A **default StorageClass is
required** — rustfs claims a PVC, and without one it stays Pending and every
asset fetch fails downstream in a way that looks like a staging problem.

**Verify:**
```bash
kubectl get node                # Ready
kubectl get sc                  # local-path (default)
```

## 8. In-cluster registry

`ko` pushes here and the node pulls from it, over plain HTTP on `localhost:5001`.
It is a `hostPort`, which is why **`ko` must run on the node** — the registry is
not reachable from a laptop.

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: registry
  namespace: kube-system
  labels: {app: registry}
spec:
  replicas: 1
  selector: {matchLabels: {app: registry}}
  template:
    metadata:
      labels: {app: registry}
    spec:
      containers:
      - name: registry
        image: registry:2
        ports:
        - containerPort: 5000
          hostPort: 5001
EOF
```

containerd must accept it as insecure (plain HTTP):

```bash
sudo mkdir -p /etc/containerd/certs.d/localhost:5001
cat <<'EOF' | sudo tee /etc/containerd/certs.d/localhost:5001/hosts.toml
server = "http://localhost:5001"
[host."http://localhost:5001"]
  capabilities = ["pull", "resolve"]
EOF
sudo systemctl restart containerd
```

**Verify:**
```bash
curl -s http://localhost:5001/v2/_catalog     # {"repositories":[]}
```

## 9. GPU Operator, vm-passthrough

This is the mode that matters. The default binds GPUs to the `nvidia` driver for
containers; **vm-passthrough binds them to `vfio-pci`** so a VM can own the
device outright. There is no NVIDIA driver on the host at all — the driver lives
inside the guest image.

```bash
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm repo update

helm install --wait gpu-operator nvidia/gpu-operator \
  --version v26.3.2 \
  -n nvidia-gpu-operator --create-namespace \
  --set sandboxWorkloads.enabled=true \
  --set sandboxWorkloads.defaultWorkload=vm-passthrough
```

That deploys `nvidia-vfio-manager` (binds to `vfio-pci`),
`nvidia-sandbox-device-plugin` (advertises the resource),
`nvidia-sandbox-validator` and node-feature-discovery. The ClusterPolicy will
show `driver.enabled: true` and `toolkit.enabled: true` — that is not a
contradiction. With `sandboxWorkloads.enabled=true` those flags declare what is
*available*, and the node label decides what is deployed; `vm-passthrough` stamps
the node with `nvidia.com/gpu.deploy.vfio-manager` and friends and omits the
container-workload equivalents.

**Verify:**
```bash
lspci -d 10de: -k | grep -A2 .        # "Kernel driver in use: vfio-pci"
ls /dev/vfio/                          # numbered group nodes, plus "vfio"
kubectl describe node | grep -i nvidia.com
```

`Kernel driver in use:` is the state line. `Kernel modules:` lists *candidates*
and says nothing about what is bound — confusing the two has cost hours.

The advertised resource is **model-specific**: `nvidia.com/TU104GL_TESLA_T4`, not
`nvidia.com/gpu`, which is the time-slicing name and never appears in this mode.
It differs on other hardware. Put whatever the node reports into the WorkerPool.

## 10. Node label

ateom's worker pods are scheduled by it, and nothing else sets it:

```bash
kubectl label node "$(hostname)" ate.dev/sandboxClass=microvm
```

## 11. Build tooling

All of this runs **on the node**, because `ko` pushes to a host-local registry
and the hardware tests need `/dev/vfio` and a real GPU.

```bash
sudo apt install -y git gettext-base awscli    # envsubst, aws

# Go must satisfy go.mod (currently 1.26.3)
curl -fsSL https://go.dev/dl/go1.26.3.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.bashrc && source ~/.bashrc

go install github.com/google/ko@latest
git clone https://github.com/agent-substrate/substrate.git ~/surat && cd ~/surat
```

Optional, for the GPU hardware tests — they want a real image rootfs so
`nvidia-smi` can run inside the probe container:

```bash
sudo ctr -n k8s.io images pull docker.io/library/ubuntu:24.04
sudo mkdir -p /mnt/ubuntu
sudo ctr -n k8s.io images mount --rw docker.io/library/ubuntu:24.04 /mnt/ubuntu
```

## Checklist before moving on

```bash
uname -r                                            # >= 6.5
ls /sys/kernel/iommu_groups | wc -l                 # > 0
kubectl get node                                    # Ready, untainted
kubectl get sc                                      # local-path (default)
curl -s http://localhost:5001/v2/_catalog           # responds
lspci -d 10de: -k | grep "in use"                   # vfio-pci
kubectl describe node | grep nvidia.com/            # model-specific resource, count > 0
kubectl get node -o jsonpath='{.items[0].metadata.labels}' | grep sandboxClass
go version && ko version                            # toolchain present
```

Then continue with `docs/dev/microvm-gpu-e2e.md`, from "Installing substrate".

## Notes for a `sudo` shell

`sudo` resets `PATH` via `secure_path`, so `sudo -E` alone will not find `go`.
Every command that needs root and Go takes this shape:

```bash
sudo -E env "PATH=$PATH" go test ...
```
