//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Handing a passthrough GPU to the actor CONTAINER is a separate problem from
// handing it to the VM (devices.go). The VM gets it over VFIO; the container
// needs device nodes, the driver libraries and a device-cgroup entry, all with
// numbers that only exist inside the guest.
//
// The guest's kata-agent does that work, and it is driven entirely by an
// annotation. At CreateContainer it scans the OCI spec's annotations for the
// "cdi.k8s.io/" prefix and, for every CDI device named there, adds the device
// nodes, the library mounts and the matching device-cgroup entries. That is why
// nothing here writes /dev/nvidia* or widens a cgroup by hand — doing either
// would duplicate what the agent already does, with host numbers that are wrong
// inside the guest.
//
// The spec it resolves against is generated INSIDE the guest at boot: NVRC runs
// `nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml` and blocks on it
// before forking the agent, so the file is always complete before the first
// CreateContainer. Guest-generated also means the major/minor numbers are
// already guest-native, so no host-to-guest translation is involved.
const (
	// guestCDIAnnotation asks the agent to inject the devices named in its value.
	// The agent matches on the "cdi.k8s.io/" prefix alone, so the suffix is free
	// and only identifies who set it.
	guestCDIAnnotation = "cdi.k8s.io/ate-passthrough"

	// guestCDIDevices is the CDI device reference to inject. NVRC generates a
	// single kind, nvidia.com/gpu, carrying an "all" device that expands to every
	// GPU the guest can see — which is exactly the pod's allocation, since the VM
	// is given precisely the devices the pod was granted. Naming "all" rather than
	// indices keeps this correct for a multi-GPU worker without counting.
	//
	// This is the one device-kind-specific value in the passthrough path. A
	// non-GPU VFIO device would need its own kind, derived from the resource name
	// rather than fixed.
	guestCDIDevices = "nvidia.com/gpu=all"

	// cdiAnnotationPrefix is what the agent scans for.
	cdiAnnotationPrefix = "cdi.k8s.io/"
)

// isNVIDIAGPU reports whether a passthrough device is an NVIDIA display-class
// device, read from the host sysfs directory ateom already hands to
// cloud-hypervisor.
//
// The device resolver deliberately matches the bare PCI_RESOURCE_ prefix, which
// is the convention EVERY Kubernetes PCI device plugin follows -- SR-IOV NICs,
// RDMA adapters and FPGAs all set it. That is right for the VFIO plumbing, which
// genuinely does not care what the device is, and wrong here: annotating a
// non-GPU actor with nvidia.com/gpu=all names a CDI device the guest cannot
// resolve, and an unresolvable CDI device FAILS CreateContainer rather than
// being ignored. An actor holding only an SR-IOV NIC would not merely take a
// slower path; it would not start.
//
// Class 0x03* is the display controllers (0x0300 VGA, 0x0302 3D -- a T4 reports
// 3D), which also excludes the HDMI-audio function that arrives in the same
// IOMMU group.
func isNVIDIAGPU(sysfsPath string) bool {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(sysfsPath, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return read("vendor") == "0x10de" && strings.HasPrefix(read("class"), "0x03")
}

// withGuestCDIDevices returns spec with the annotation that makes the guest
// agent inject this worker's passthrough GPU(s) into the container, or spec
// unchanged when the worker was granted none.
//
// It drops any inherited cdi.k8s.io/ annotation first. Those name HOST-side CDI
// devices — the NVIDIA sandbox device plugin advertises kind nvidia.com/pgpu on
// the host — which do not exist in the guest's spec. An unresolvable CDI device
// is not ignored: it fails CreateContainer, so an inherited annotation would
// break the container rather than merely be useless. (Kata's own shim clears
// them for the same reason before forwarding a spec into a guest.)
//
// The spec is copied rather than mutated: the caller's spec is the bundle's
// prepared config.json, shared with the carrier container, which must not see
// these annotations — its rootfs is the read-only lower and it has no use for
// the device.
func withGuestCDIDevices(spec *specs.Spec) (*specs.Spec, error) {
	devs, err := resolveWorkerDevices()
	if err != nil {
		return nil, fmt.Errorf("while resolving worker passthrough devices: %w", err)
	}
	gpus := 0
	for _, d := range devs {
		if isNVIDIAGPU(d.Path) {
			gpus++
		}
	}
	inherited := false
	for k := range spec.Annotations {
		if strings.HasPrefix(k, cdiAnnotationPrefix) {
			inherited = true
			break
		}
	}
	if gpus == 0 && !inherited {
		return spec, nil
	}

	out := *spec
	out.Annotations = make(map[string]string, len(spec.Annotations)+1)
	for k, v := range spec.Annotations {
		if !strings.HasPrefix(k, cdiAnnotationPrefix) {
			out.Annotations[k] = v
		}
	}
	if gpus > 0 {
		out.Annotations[guestCDIAnnotation] = guestCDIDevices
	}
	return &out, nil
}
