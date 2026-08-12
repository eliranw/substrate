# Rebuilding the GPU node

Brings a bare GPU host back to the state substrate expects, so that
`docs/dev/microvm-gpu-e2e.md` can start at its step 0. Nothing here is
substrate: reinstalling substrate never touches any of it, and this is only
needed when the machine itself is replaced.

Transcribed from `s2029gp-tr-0139` — Ubuntu 22.04.5, kernel 5.15.0-177,
containerd 2.3.1, Kubernetes v1.36.2 (kubeadm), single node, 6× Tesla T4.
Substitute versions freely; the parts that are *not* free choices are called
out.

## 1. Host: IOMMU

VFIO cannot isolate a device without it, so passthrough fails at the first step
with no useful error.

```bash
sudo sed -i 's/GRUB_CMDLINE_LINUX="\(.*\)"/GRUB_CMDLINE_LINUX="\1 intel_iommu=on iommu=pt"/' \
  /etc/default/grub
sudo update-grub && sudo reboot
```

```bash
ls /sys/kernel/iommu_groups | wc -l          # must be > 0 after the reboot
```

`iommu=pt` (passthrough) leaves DMA unremapped for devices not assigned to a
VM, so host-attached hardware keeps its normal performance.

## 2. kubeadm init

The feature gates are the one thing here that is not a free choice — substrate
does not work without them, and the failure mode is silent. `PodCertificateRequest`
is how every component gets the certificate it authenticates with; a disabled
alpha field is **pruned at admission**, so the volume simply disappears from pod
specs and each component dies reading a file nobody wrote. `ClusterTrustBundle`
is how they learn which CA to trust.

They must be set on the API server, controller-manager **and** scheduler.
`certificates.k8s.io/v1beta1` must be in `runtime-config` as well, or the
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

Editing `/etc/kubernetes/manifests/kube-apiserver.yaml` afterwards works and
restarts the static pod, but the change is invisible to `kubeadm-config` and a
later `kubeadm upgrade` regenerates the manifest from that ConfigMap — so put
gates in the config, not the manifest.

## 3. CNI and storage

```bash
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml
kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.28/deploy/local-path-storage.yaml
kubectl patch storageclass local-path \
  -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
```

Calico's manifest carries its own pod CIDR; it must match `podSubnet` above.
A default StorageClass is required — rustfs claims a PVC, and without one it
stays Pending and every asset fetch fails downstream.

## 4. In-cluster registry

`ko` pushes here, and the node pulls from it. It must be reachable as
`localhost:5001` **on the node**, which is why `ko` has to run there rather than
on a laptop.

```bash
kubectl -n kube-system create deployment registry --image=registry:2
kubectl -n kube-system expose deployment registry --port=5000
# then map it to localhost:5001 on the host (hostPort, NodePort, or a socat unit)
```

containerd must accept it as an insecure (plain HTTP) registry:

```toml
# /etc/containerd/certs.d/localhost:5001/hosts.toml
server = "http://localhost:5001"
[host."http://localhost:5001"]
  capabilities = ["pull", "resolve"]
```

## 5. GPU Operator in vm-passthrough mode

This is the mode that matters. The default binds GPUs to the `nvidia` driver for
containers; **vm-passthrough binds them to `vfio-pci`** so a VM can own the
device outright, which is what ateom hands to cloud-hypervisor.

```bash
helm install --wait gpu-operator nvidia/gpu-operator \
  -n nvidia-gpu-operator --create-namespace \
  --set sandboxWorkloads.enabled=true \
  --set sandboxWorkloads.defaultWorkload=vm-passthrough
```

```bash
lspci -d 10de: -k | grep -A2 .     # "Kernel driver in use: vfio-pci"
ls /dev/vfio/                      # a numbered group node, plus "vfio"
kubectl describe node | grep -i nvidia.com
```

The advertised resource is **model-specific** — `nvidia.com/TU104GL_TESLA_T4`,
not `nvidia.com/gpu`, which is the time-slicing name and will not appear. Put
whatever the node reports into the WorkerPool.

`Kernel driver in use:` is the state line. `Kernel modules:` lists *candidates*
and says nothing about what is bound; confusing the two has cost time before.

## 6. Node label

ateom's worker pods are scheduled by it, and nothing else sets it:

```bash
kubectl label node <gpu-node> ate.dev/sandboxClass=microvm
```

## Then

`docs/dev/microvm-gpu-e2e.md`, from "Installing substrate".
