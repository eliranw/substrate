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

# Assemble the GPU-specific micro-VM guest assets (kernel + rootfs image) for a
# GPU SandboxConfig. The NVIDIA driver and its guest-side bring-up are baked into
# the rootfs, so a GPU actor's VM boots this instead of the stock kata guest.
#
# The assets come from the SAME kata-static tarball assemble.sh already downloads
# -- upstream kata ships the NVIDIA GPU guest alongside the stock one -- so this
# is a file-selection difference, not a separate supply chain. Only the two
# GPU-specific payloads are produced here; cloud-hypervisor, virtiofsd and
# configuration-clh.toml are guest-agnostic and shared with assemble.sh.
#
# Env:
#   ARCH      amd64 only (the NVIDIA GPU guest is x86_64); default amd64
#   KATA_VER  kata-containers release (default 4.0.0)
#   OUT       output dir (default bin/microvm-gpu-assets, under the gitignored bin/)
set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
ARCH="${ARCH:-amd64}"
KATA_VER="${KATA_VER:-4.0.0}"
OUT="${OUT:-${ROOT}/bin/microvm-gpu-assets}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if [[ "$ARCH" != "amd64" ]]; then
  echo "the NVIDIA GPU guest is published for amd64 only (got ARCH=$ARCH)" >&2
  exit 1
fi

mkdir -p "$OUT"
cd "$WORK"

echo ">> Downloading kata-static ${KATA_VER} (${ARCH})..."
curl -fSL -o kata-static.tar.zst \
  "https://github.com/kata-containers/kata-containers/releases/download/${KATA_VER}/kata-static-${KATA_VER}-${ARCH}.tar.zst"

# Extract only what a GPU guest needs. vmlinux-nvidia-gpu.container and
# kata-containers-nvidia-gpu.img are the stable symlink names upstream keeps
# pointing at the current driver/kernel build, so following them avoids pinning a
# version string that changes every release.
#
# The member glob needs --wildcards on GNU tar; bsdtar (the macOS default) globs
# by default and rejects the flag outright, so pass it only where it exists.
echo ">> Extracting the NVIDIA GPU guest..."
mkdir -p kata
wildcards=()
if tar --version 2>&1 | grep -qi 'gnu tar'; then
  wildcards=(--wildcards)
fi
tar --zstd -xf kata-static.tar.zst -C kata \
  "${wildcards[@]}" './opt/kata/share/kata-containers/*nvidia-gpu*' \
  './opt/kata/share/defaults/kata-containers/configuration-clh.toml' \
  './opt/kata/share/defaults/kata-containers/configuration-qemu-nvidia-gpu.toml'

KROOT="kata/opt/kata/share/kata-containers"
kernel="$(readlink -f "${KROOT}/vmlinux-nvidia-gpu.container")"
image="$(readlink -f "${KROOT}/kata-containers-nvidia-gpu.img")"
if [[ ! -f "$kernel" || ! -f "$image" ]]; then
  echo "!! could not resolve the GPU kernel/image symlinks in ${KROOT}:" >&2
  ls -l "$KROOT" >&2
  exit 1
fi

cp -f "$kernel" "${OUT}/vmlinux-gpu"
cp -f "$image" "${OUT}/rootfs-gpu.img"

# Upstream publishes no clh flavour of the NVIDIA config -- kata's own GPU support
# is QEMU-only -- so derive one. ateom reads exactly four keys from this file
# (kata.ParseConfig), and every other key is ignored, so emitting just those is
# equivalent to a merged 34KB toml and cannot drift with upstream's other keys.
#
# The two GPU-specific values are lifted from upstream's own GPU config rather
# than written here, so they track the release:
#   rootfs_type   the guest image's filesystem (erofs, vs ext4 for the stock
#                 guest) -- it decides root=/rootflags=/rootfstype=.
#   kernel_params pci=realloc pci=nocrs pci=assign-busses, which is BAR
#                 reallocation and bus reassignment for the passed-through GPU.
# Sizing comes from the clh config, since that is hypervisor state, not guest.
DEFAULTS="kata/opt/kata/share/defaults/kata-containers"
tomlval() { sed -n "s/^[[:space:]]*$2[[:space:]]*=[[:space:]]*//p" "$1" | head -1; }

gpu_rootfs_type="$(tomlval "${DEFAULTS}/configuration-qemu-nvidia-gpu.toml" rootfs_type)"
gpu_kernel_params="$(tomlval "${DEFAULTS}/configuration-qemu-nvidia-gpu.toml" kernel_params)"

# Drop pci=nocrs. It tells Linux to ignore the ACPI _CRS host-bridge windows --
# the address ranges the platform declares it actually routes -- and fall back to
# assuming MMIO starts just above top-of-RAM. That is a sensible workaround for
# bare-metal BIOSes that publish wrong windows, which is why NVIDIA ships it.
#
# cloud-hypervisor publishes correct windows, so ignoring them replaces good
# information with a guess, and the guess is wrong: the guest picks an address
# clh does not route, clh refuses ("Failed moving device BAR ... keeping old
# BAR"), and the guest then drives MMIO where the device is not. Verified on a
# T4: with nocrs the GPU is unusable after a hot-plug and the kernel demands
# 0x80000000 every time (exactly the top of a 2048MiB guest); without it the
# kernel picks 0xc0000000, inside clh's aperture, and the GPU works.
#
# Cold boot never noticed because clh lays the BARs out and the guest only reads
# them. Only hot-plug makes the guest ASSIGN an address, which is what
# re-attaching a device after a suspend does.
gpu_kernel_params="${gpu_kernel_params//pci=nocrs/}"
clh_memory="$(tomlval "${DEFAULTS}/configuration-clh.toml" default_memory)"
clh_vcpus="$(tomlval "${DEFAULTS}/configuration-clh.toml" default_vcpus)"
for v in gpu_rootfs_type gpu_kernel_params clh_memory clh_vcpus; do
  if [[ -z "${!v}" ]]; then
    echo "!! could not read ${v} from the kata configs in ${DEFAULTS}" >&2
    exit 1
  fi
done

cat > "${OUT}/configuration-clh-gpu.toml" <<EOF
# Generated by hack/microvm-assets/assemble-gpu.sh from kata-static ${KATA_VER}.
# Do not edit: rootfs_type and kernel_params are copied from upstream's
# configuration-qemu-nvidia-gpu.toml so they track the guest image they describe.
[hypervisor.clh]
rootfs_type = ${gpu_rootfs_type}
kernel_params = ${gpu_kernel_params}
default_memory = ${clh_memory}
default_vcpus = ${clh_vcpus}
EOF

echo ">> done. GPU guest assets in ${OUT}:"
cd "$OUT"
ls -lh vmlinux-gpu rootfs-gpu.img configuration-clh-gpu.toml
echo
echo ">> generated kata-config:"
sed 's/^/     /' configuration-clh-gpu.toml
echo
echo ">> sha256 -- paste these into the GPU SandboxConfig"
echo "   (kata-kernel -> vmlinux-gpu, kata-image -> rootfs-gpu.img,"
echo "    kata-config -> configuration-clh-gpu.toml):"
sha256sum vmlinux-gpu rootfs-gpu.img configuration-clh-gpu.toml
