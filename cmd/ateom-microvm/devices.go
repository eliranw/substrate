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
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
)

// pciResourceEnvPrefix is the convention a Kubernetes PCI device plugin follows
// when it hands a passthrough device to a pod: the resource name, uppercased with
// "/" and "." replaced by "_", carrying the allocated PCI addresses.
//
//	PCI_RESOURCE_<VENDOR>_<MODEL>=<BDF>[,<BDF>...]
//	e.g. PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4=0000:da:00.0
//
// Matching the bare prefix rather than one vendor's keeps this device-agnostic:
// any VFIO device a plugin allocates to the worker is passed through the same
// way. Nothing downstream of here inspects what KIND of device it is.
const pciResourceEnvPrefix = "PCI_RESOURCE_"

// resolveWorkerDevices returns the cloud-hypervisor VFIO passthrough devices a
// device plugin allocated to THIS worker pod. Because one actor runs per worker,
// its VM gets exactly the pod's allocation. Results are deduped by PCI address
// and sorted for a stable vm.create body.
//
// We deliberately do NOT enumerate /dev/vfio: the ateom worker runs privileged
// and therefore sees EVERY host VFIO group, not just the one(s) allocated to it
// -- enumerating it would over-assign the whole node's devices to a single actor.
// The env is the authoritative per-pod allocation and carries the host BDF
// directly, which is exactly what cloud-hypervisor's --device wants.
//
// Returns (nil, nil) when nothing was allocated (an ordinary worker).
func resolveWorkerDevices() ([]ch.DeviceConfig, error) {
	seen := map[string]bool{}
	var addrs []string
	for _, e := range os.Environ() {
		k, v, ok := strings.Cut(e, "=")
		if !ok || v == "" || !strings.HasPrefix(k, pciResourceEnvPrefix) {
			continue
		}
		for _, bdf := range strings.Split(v, ",") {
			bdf = strings.TrimSpace(bdf)
			if bdf != "" && !seen[bdf] {
				seen[bdf] = true
				addrs = append(addrs, bdf)
			}
		}
	}
	if len(addrs) == 0 {
		return nil, nil // nothing allocated -> ordinary worker
	}
	sort.Strings(addrs)
	devs := make([]ch.DeviceConfig, 0, len(addrs))
	for _, a := range addrs {
		devs = append(devs, ch.DeviceConfig{Path: filepath.Join("/sys/bus/pci/devices", a) + "/"})
	}
	return devs, nil
}
