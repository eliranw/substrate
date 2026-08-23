#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# The executable form of docs/dev/gpu-node-bootstrap.md: takes a bare Ubuntu
# 22.04 GPU box to a single-node cluster substrate can run micro-VM GPU actors
# on. Read that document for why each step is here; this only performs them.
#
# Two phases, because the kernel and IOMMU changes need a reboot and nothing
# after them works until it happens:
#
#   sudo ./gpu-node-bootstrap.sh phase1   # kernel + IOMMU, then reboot
#   sudo reboot
#   ./gpu-node-bootstrap.sh phase2        # everything else (NOT as root)
#
# Every step is idempotent, so a failed run can be re-run after fixing the cause
# rather than restarted from a fresh machine.
set -o errexit -o nounset -o pipefail

K8S_MINOR="${K8S_MINOR:-v1.36}"
POD_SUBNET="${POD_SUBNET:-192.168.32.0/22}"
SERVICE_SUBNET="${SERVICE_SUBNET:-10.96.0.0/12}"
CALICO_VER="${CALICO_VER:-v3.32.0}"
LOCAL_PATH_VER="${LOCAL_PATH_VER:-v0.0.30}"
GPU_OPERATOR_VER="${GPU_OPERATOR_VER:-v26.3.2}"
GO_VER="${GO_VER:-1.26.3}"

say() { printf '\n>> %s\n' "$*"; }

need_root() {
  [[ $EUID -eq 0 ]] || { echo "!! run phase1 with sudo" >&2; exit 1; }
}
refuse_root() {
  [[ $EUID -ne 0 ]] || {
    echo "!! run phase2 as your normal user -- it writes ~/.kube and ~/go" >&2
    exit 1
  }
}

phase1() {
  need_root

  # A boot menu FIRST, before anything changes how this machine boots. Ubuntu
  # ships GRUB_TIMEOUT=0 with a hidden menu, so a kernel that fails to come up
  # leaves no way to choose the previous one -- on a remote box that is a brick
  # recoverable only from the BMC console. Everything below is reversible; this
  # is what makes it so.
  say "boot menu: giving the machine a way back if the new kernel fails"
  cp -a /etc/default/grub "/etc/default/grub.bak.$(date +%s)"
  sed -i 's/^GRUB_TIMEOUT=.*/GRUB_TIMEOUT=10/' /etc/default/grub
  sed -i 's/^GRUB_TIMEOUT_STYLE=.*/GRUB_TIMEOUT_STYLE=menu/' /etc/default/grub
  grep -q '^GRUB_TIMEOUT=' /etc/default/grub || echo 'GRUB_TIMEOUT=10' >> /etc/default/grub
  grep -q '^GRUB_TIMEOUT_STYLE=' /etc/default/grub || echo 'GRUB_TIMEOUT_STYLE=menu' >> /etc/default/grub

  # VFIO cannot isolate a device without an IOMMU, and passthrough then fails at
  # the first step with no useful error.
  #
  # The enable flag is vendor-specific, and the kernel ignores the other vendor's
  # without complaint -- so hardcoding one boots the wrong host with no IOMMU
  # while every step here still reports success. The group count checked after
  # the reboot is what finally catches it, a whole reboot later.
  local iommu_flag
  if grep -q '^vendor_id.*AuthenticAMD' /proc/cpuinfo; then
    iommu_flag="amd_iommu=on"
  else
    iommu_flag="intel_iommu=on"
  fi
  say "IOMMU: adding $iommu_flag iommu=pt to the kernel cmdline"
  if grep -q "$iommu_flag" /etc/default/grub; then
    echo "   already present"
  else
    sed -i "s/GRUB_CMDLINE_LINUX=\"\(.*\)\"/GRUB_CMDLINE_LINUX=\"\1 $iommu_flag iommu=pt\"/" \
      /etc/default/grub
    # Assert the substitution matched. A silent no-op here means booting without
    # an IOMMU and discovering it much later, at the first passthrough attempt.
    grep -q "GRUB_CMDLINE_LINUX=\".*$iommu_flag" /etc/default/grub || {
      echo "!! the GRUB_CMDLINE_LINUX substitution did not match. /etc/default/grub is:" >&2
      grep GRUB_CMDLINE /etc/default/grub >&2
      echo "!! add '$iommu_flag iommu=pt' by hand, then re-run" >&2
      exit 1
    }
  fi

  # Validate before committing. grub-mkconfig to a scratch file exercises the
  # same generator update-grub uses, so a config it rejects is caught here
  # rather than at the next boot.
  say "validating the generated grub config before writing it"
  grub-mkconfig -o /tmp/grub.cfg.check >/dev/null 2>&1 || {
    echo "!! grub-mkconfig failed -- NOT touching the live config" >&2
    exit 1
  }
  update-grub

  # imagecache mounts container rootfs overlays with one lowerdir+ per layer,
  # which overlayfs only understands from 6.5. On 5.15 every actor fails with a
  # bare "invalid argument" that names neither overlayfs nor a version.
  say "kernel: installing the HWE stack (need >= 6.5, 22.04 ships 5.15)"
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq linux-generic-hwe-22.04

  # Confirm a >= 6.5 kernel actually landed and has an initramfs. Rebooting into
  # a kernel that was never fully installed is the failure this catches.
  say "verifying the new kernel is installed and bootable"
  local newest; newest="$(ls -1 /boot/vmlinuz-* 2>/dev/null | sed 's/.*vmlinuz-//' | sort -V | tail -1)"
  echo "   newest kernel on /boot: ${newest:-NONE}"
  case "${newest:-0}" in
    [0-5].*|6.[0-4].*|NONE|0)
      echo "!! no kernel >= 6.5 in /boot -- the HWE install did not take." >&2
      echo "!! DO NOT REBOOT. Fix the install first." >&2
      exit 1 ;;
  esac
  [[ -f "/boot/initrd.img-${newest}" ]] || {
    echo "!! /boot/initrd.img-${newest} is missing -- that kernel will not boot." >&2
    echo "!! DO NOT REBOOT. Try: update-initramfs -c -k ${newest}" >&2
    exit 1
  }

  say "phase1 done -- kernel ${newest} installed, boot menu enabled (10s)."
  echo "   If it does not come back, pick the previous kernel from the GRUB menu"
  echo "   on the BMC console rather than power-cycling."
  echo "   Then: sudo reboot && ./gpu-node-bootstrap.sh phase2"
}

phase2() {
  refuse_root

  say "verifying phase1 actually took"
  local kver; kver="$(uname -r)"
  case "$kver" in
    [0-5].*|6.[0-4].*)
      echo "!! kernel is $kver, need >= 6.5 -- did phase1 run and the box reboot?" >&2
      exit 1 ;;
  esac
  echo "   kernel $kver"
  local groups; groups="$(find /sys/kernel/iommu_groups -maxdepth 1 -mindepth 1 | wc -l)"
  [[ "$groups" -gt 0 ]] || { echo "!! no IOMMU groups -- phase1 GRUB change did not take" >&2; exit 1; }
  echo "   $groups IOMMU groups"

  say "kubelet prerequisites: swap off, modules, sysctls"
  sudo swapoff -a
  sudo sed -i '/[[:space:]]swap[[:space:]]/s/^\([^#]\)/#\1/' /etc/fstab
  printf 'overlay\nbr_netfilter\n' | sudo tee /etc/modules-load.d/k8s.conf >/dev/null
  sudo modprobe overlay || true
  sudo modprobe br_netfilter || true
  printf 'net.bridge.bridge-nf-call-iptables  = 1\nnet.bridge.bridge-nf-call-ip6tables = 1\nnet.ipv4.ip_forward                 = 1\n' \
    | sudo tee /etc/sysctl.d/k8s.conf >/dev/null
  sudo sysctl --system >/dev/null

  # SystemdCgroup must match kubelet's driver or pods fail to start with cgroup
  # errors that do not mention cgroups.
  say "containerd"
  sudo apt-get install -y -qq containerd
  sudo mkdir -p /etc/containerd
  containerd config default | sudo tee /etc/containerd/config.toml >/dev/null
  sudo sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
  sudo mkdir -p "/etc/containerd/certs.d/localhost:5001"
  printf 'server = "http://localhost:5001"\n[host."http://localhost:5001"]\n  capabilities = ["pull", "resolve"]\n' \
    | sudo tee "/etc/containerd/certs.d/localhost:5001/hosts.toml" >/dev/null
  sudo systemctl restart containerd

  say "kubernetes packages (${K8S_MINOR})"
  sudo apt-get install -y -qq apt-transport-https ca-certificates curl gpg
  sudo mkdir -p /etc/apt/keyrings
  if [[ ! -f /etc/apt/keyrings/kubernetes-apt-keyring.gpg ]]; then
    curl -fsSL "https://pkgs.k8s.io/core:/stable:/${K8S_MINOR}/deb/Release.key" \
      | sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  fi
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/${K8S_MINOR}/deb/ /" \
    | sudo tee /etc/apt/sources.list.d/kubernetes.list >/dev/null
  sudo apt-get update -qq
  sudo apt-get install -y -qq kubelet kubeadm kubectl
  sudo apt-mark hold kubelet kubeadm kubectl >/dev/null

  # The feature gates are the one thing here that is not a free choice. A
  # disabled alpha field is PRUNED at admission rather than rejected, so without
  # PodCertificateRequest the podCertificate volume silently vanishes from pod
  # specs and every substrate component dies reading a file nobody wrote.
  say "kubeadm init"
  if sudo test -f /etc/kubernetes/admin.conf; then
    echo "   control plane already initialised, skipping"
  else
    local gates="DynamicResourceAllocation=true,ClusterTrustBundle=true,ClusterTrustBundleProjection=true,PodCertificateRequest=true"
    cat >/tmp/kubeadm-config.yaml <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
networking:
  podSubnet: ${POD_SUBNET}
  serviceSubnet: ${SERVICE_SUBNET}
apiServer:
  extraArgs:
  - name: feature-gates
    value: ${gates}
  - name: runtime-config
    value: resource.k8s.io/v1beta1=true
  - name: runtime-config
    value: resource.k8s.io/v1beta2=true
  - name: runtime-config
    value: certificates.k8s.io/v1beta1=true
controllerManager:
  extraArgs:
  - name: feature-gates
    value: ${gates}
scheduler:
  extraArgs:
  - name: feature-gates
    value: ${gates}
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
EOF
    sudo kubeadm init --config /tmp/kubeadm-config.yaml
  fi

  mkdir -p ~/.kube
  sudo cp -f /etc/kubernetes/admin.conf ~/.kube/config
  sudo chown "$(id -u):$(id -g)" ~/.kube/config
  # Single node: it has to run workloads.
  kubectl taint node --all node-role.kubernetes.io/control-plane- 2>/dev/null || true

  say "CNI and storage"
  kubectl apply -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VER}/manifests/calico.yaml"
  kubectl apply -f "https://raw.githubusercontent.com/rancher/local-path-provisioner/${LOCAL_PATH_VER}/deploy/local-path-storage.yaml"
  kubectl patch storageclass local-path \
    -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'

  # ko pushes here and the node pulls from it, over plain HTTP on a hostPort --
  # which is why ko must run ON this box, not from a laptop.
  say "in-cluster registry on localhost:5001"
  kubectl apply -f - <<'EOF'
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

  # vm-passthrough binds each GPU to vfio-pci so a VM can own it outright. There
  # is no NVIDIA driver on the host at all; the driver lives in the guest image.
  say "GPU Operator in vm-passthrough mode"
  if ! command -v helm >/dev/null; then
    curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
  fi
  helm repo add nvidia https://helm.ngc.nvidia.com/nvidia >/dev/null 2>&1 || true
  helm repo update >/dev/null
  helm upgrade --install --wait gpu-operator nvidia/gpu-operator \
    --version "${GPU_OPERATOR_VER}" \
    -n nvidia-gpu-operator --create-namespace \
    --set sandboxWorkloads.enabled=true \
    --set sandboxWorkloads.defaultWorkload=vm-passthrough

  say "node label (nothing else sets it; ateom's workers schedule on it)"
  kubectl label node "$(hostname)" ate.dev/sandboxClass=microvm --overwrite

  say "build tooling"
  sudo apt-get install -y -qq git gettext-base awscli
  if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VER}"; then
    curl -fsSL "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz" | sudo tar -C /usr/local -xz
  fi
  export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin"
  grep -q '/usr/local/go/bin' ~/.bashrc || \
    echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.bashrc
  command -v ko >/dev/null || go install github.com/google/ko@latest

  # The GPU hardware tests want a real image rootfs so nvidia-smi can run in the
  # probe container. This mount does NOT survive a reboot -- see the runbook.
  say "probe rootfs at /mnt/ubuntu (re-run this line after every reboot)"
  sudo ctr -n k8s.io images pull docker.io/library/ubuntu:24.04 >/dev/null
  sudo mkdir -p /mnt/ubuntu
  sudo ctr -n k8s.io images mount --rw docker.io/library/ubuntu:24.04 /mnt/ubuntu >/dev/null 2>&1 || true

  say "phase2 done -- checks below"
  checks
}

checks() {
  export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin"
  echo
  printf 'kernel >= 6.5          : %s\n' "$(uname -r)"
  printf 'IOMMU groups           : %s\n' "$(find /sys/kernel/iommu_groups -maxdepth 1 -mindepth 1 | wc -l)"
  printf 'node                   : %s\n' "$(kubectl get node --no-headers 2>/dev/null | awk '{print $1, $2}')"
  printf 'default StorageClass   : %s\n' "$(kubectl get sc --no-headers 2>/dev/null | grep default | awk '{print $1}')"
  printf 'registry               : %s\n' "$(curl -fsS http://localhost:5001/v2/_catalog 2>/dev/null || echo UNREACHABLE)"
  printf 'GPUs bound to vfio-pci : %s\n' "$(lspci -d 10de: -k 2>/dev/null | grep -c 'in use: vfio-pci' || echo 0)"
  printf 'advertised GPU resource: %s\n' "$(kubectl get node -o jsonpath='{.items[0].status.allocatable}' 2>/dev/null | tr ',' '\n' | grep -i nvidia | tr -d '"{}' || echo NONE)"
  printf 'sandboxClass label     : %s\n' "$(kubectl get node -o jsonpath='{.items[0].metadata.labels.ate\.dev/sandboxClass}' 2>/dev/null)"
  printf 'go / ko                : %s / %s\n' "$(go version 2>/dev/null | awk '{print $3}')" "$(ko version 2>/dev/null)"
  printf 'probe rootfs           : %s\n' "$(ls /mnt/ubuntu/lib/x86_64-linux-gnu >/dev/null 2>&1 && echo ok || echo 'EMPTY -- re-run the ctr mount')"
  echo
  echo "podCertificate round-trip (the check that cannot mislead):"
  kubectl apply --dry-run=server -o json -f - <<'EOF' 2>/dev/null | grep -c podCertificate || echo 0
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
  echo "  (must be > 0; 0 means the feature gate did not take)"
}

case "${1:-}" in
  phase1) phase1 ;;
  phase2) phase2 ;;
  checks) checks ;;
  *) echo "usage: $0 phase1|phase2|checks" >&2; exit 1 ;;
esac
