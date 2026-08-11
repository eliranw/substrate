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
  "${wildcards[@]}" './opt/kata/share/kata-containers/*nvidia-gpu*'

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

echo ">> done. GPU guest assets in ${OUT}:"
cd "$OUT"
ls -lh vmlinux-gpu rootfs-gpu.img
echo
echo ">> sha256 -- paste these into the GPU SandboxConfig"
echo "   (kata-kernel -> vmlinux-gpu, kata-image -> rootfs-gpu.img):"
sha256sum vmlinux-gpu rootfs-gpu.img
