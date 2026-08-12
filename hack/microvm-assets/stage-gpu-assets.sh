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

# Upload the GPU guest assets into the cluster's object store and print a
# SandboxConfig with their hashes filled in.
#
# atelet fetches runtime assets from whatever object store it was configured
# with (ATE_STORAGE_BACKEND on the atelet DaemonSet). This reads that config
# rather than taking endpoint and credentials as arguments, so it cannot drift
# from what the cluster actually uses.
#
# The asset URLs keep the gs:// scheme even on an S3 backend: only the bucket
# and key are used, and the existing SandboxConfigs are written that way.
#
# Run hack/microvm-assets/assemble-gpu.sh first.
set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
ASSETS="${ASSETS:-${ROOT}/bin/microvm-gpu-assets}"
BUCKET="${BUCKET:-ate-snapshots}"
PREFIX="${PREFIX:-kata-gpu-assets}"
NS="${NS:-ate-system}"

for f in vmlinux-gpu rootfs-gpu.img configuration-clh-gpu.toml; do
  [[ -f "${ASSETS}/${f}" ]] || {
    echo "!! missing ${ASSETS}/${f} -- run hack/microvm-assets/assemble-gpu.sh" >&2
    exit 1
  }
done

# The generated config is what makes the guest bootable AND hot-pluggable, so
# check it before uploading rather than debugging a guest that will not boot.
cfg="${ASSETS}/configuration-clh-gpu.toml"
grep -q 'rootfs_type = "erofs"' "$cfg" || {
  echo "!! ${cfg} does not say rootfs_type = \"erofs\"; the guest will not mount its root" >&2
  exit 1
}
if grep -q 'pci=nocrs' "$cfg"; then
  echo "!! ${cfg} still contains pci=nocrs. That makes the guest ignore the VMM's" >&2
  echo "   real MMIO windows, so a re-attached GPU is unusable. assemble-gpu.sh" >&2
  echo "   strips it -- this config predates that fix; regenerate it." >&2
  exit 1
fi

env_of() {
  kubectl -n "$NS" get ds atelet \
    -o jsonpath="{range .spec.template.spec.containers[0].env[?(@.name=='$1')]}{.value}{end}"
}
ENDPOINT="$(env_of AWS_ENDPOINT_URL)"
REGION="$(env_of AWS_REGION)"
KEY_ID="$(env_of AWS_ACCESS_KEY_ID)"
SECRET="$(env_of AWS_SECRET_ACCESS_KEY)"
[[ -n "$ENDPOINT" ]] || { echo "!! atelet has no AWS_ENDPOINT_URL; is this an S3-backed cluster?" >&2; exit 1; }

# The endpoint is a cluster-internal Service, so reach it through a port-forward
# rather than requiring the object store to be exposed on the node.
port="${PORT:-19000}"
svc="$(sed -E 's#^https?://([^.]+).*#\1#' <<<"$ENDPOINT")"
svcport="$(sed -E 's#.*:([0-9]+).*#\1#' <<<"$ENDPOINT")"
echo ">> port-forwarding svc/${svc}:${svcport} -> localhost:${port}"
kubectl -n "$NS" port-forward "svc/${svc}" "${port}:${svcport}" >/dev/null 2>&1 &
pf=$!
trap 'kill "$pf" 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  curl -fsS "http://localhost:${port}" >/dev/null 2>&1 && break
  sleep 0.5
done

upload() {
  AWS_ACCESS_KEY_ID="$KEY_ID" AWS_SECRET_ACCESS_KEY="$SECRET" AWS_REGION="$REGION" \
    aws --endpoint-url "http://localhost:${port}" s3 cp "$1" "s3://${BUCKET}/${PREFIX}/$(basename "$1")"
}
command -v aws >/dev/null || {
  echo "!! the aws CLI is required (pip install awscli, or apt install awscli)" >&2
  exit 1
}

for f in vmlinux-gpu rootfs-gpu.img configuration-clh-gpu.toml; do
  echo ">> uploading ${f}..."
  upload "${ASSETS}/${f}"
done

sha() { sha256sum "${ASSETS}/$1" | cut -d' ' -f1; }
echo
echo ">> uploaded. Apply the SandboxConfig with:"
echo
echo "   export BUCKET_NAME=${BUCKET}"
echo "   export GPU_KERNEL_SHA256=$(sha vmlinux-gpu)"
echo "   export GPU_ROOTFS_SHA256=$(sha rootfs-gpu.img)"
echo "   export GPU_CONFIG_SHA256=$(sha configuration-clh-gpu.toml)"
echo "   envsubst < manifests/microvm/sandboxconfig-microvm-gpu.yaml.tmpl | kubectl apply -f -"
