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
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// fakePCIDevice writes a sysfs device directory and points the resolver at it,
// so the vendor/class check has something real to read.
func fakePCIDevice(t *testing.T, bdf, vendor, class string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, bdf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, val := range map[string]string{"vendor": vendor, "class": class} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(val+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := pciSysfsRoot
	pciSysfsRoot = root
	t.Cleanup(func() { pciSysfsRoot = orig })
}

func TestGuestCDIAnnotationSetOnPassthroughWorker(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
	fakePCIDevice(t, "0000:da:00.0", "0x10de", "0x030200")
	got, err := withGuestCDIDevices(&specs.Spec{})
	if err != nil {
		t.Fatalf("withGuestCDIDevices: %v", err)
	}
	if got.Annotations[guestCDIAnnotation] != guestCDIDevices {
		t.Errorf("annotation %s = %q, want %q",
			guestCDIAnnotation, got.Annotations[guestCDIAnnotation], guestCDIDevices)
	}
}

// A worker with no allocation must not ask for CDI injection. The agent fails
// CreateContainer when a named CDI device cannot be resolved, so annotating
// unconditionally would break every ordinary actor.
func TestGuestCDIAnnotationAbsentWithoutPassthrough(t *testing.T) {
	spec := &specs.Spec{Annotations: map[string]string{"keep": "me"}}
	got, err := withGuestCDIDevices(spec)
	if err != nil {
		t.Fatalf("withGuestCDIDevices: %v", err)
	}
	if _, ok := got.Annotations[guestCDIAnnotation]; ok {
		t.Error("no passthrough device allocated, but a CDI annotation was set")
	}
	if got != spec {
		t.Error("spec with nothing to change should be returned as-is, not copied")
	}
}

// Host-side CDI annotations name devices that do not exist in the guest's spec
// (the sandbox device plugin advertises nvidia.com/pgpu on the host). An
// unresolvable CDI device fails CreateContainer rather than being ignored, so
// carrying one through would break the container.
func TestGuestCDIAnnotationDropsInheritedHostAnnotations(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
	fakePCIDevice(t, "0000:da:00.0", "0x10de", "0x030200")
	got, err := withGuestCDIDevices(&specs.Spec{Annotations: map[string]string{
		"cdi.k8s.io/gpu": "nvidia.com/pgpu=0",
		"unrelated":      "kept",
	}})
	if err != nil {
		t.Fatalf("withGuestCDIDevices: %v", err)
	}
	if v, ok := got.Annotations["cdi.k8s.io/gpu"]; ok {
		t.Errorf("inherited host CDI annotation survived: cdi.k8s.io/gpu = %q", v)
	}
	if got.Annotations["unrelated"] != "kept" {
		t.Error("non-CDI annotations must be preserved")
	}
	if got.Annotations[guestCDIAnnotation] != guestCDIDevices {
		t.Error("our own guest CDI annotation should still be set")
	}
}

// The inherited annotation is dangerous on its own: a worker with no passthrough
// device must still have it stripped, or the container fails to create.
//
// This is also the only case that reaches the rewrite with no allocation — the
// no-annotation case returns early — so it is where "annotate only when a device
// was actually granted" is decided. Assert BOTH halves: stripping theirs and not
// adding ours. Checking only the strip lets an unconditional annotate through,
// which would name a CDI device no guest without a GPU can resolve.
func TestGuestCDIAnnotationStrippedEvenWithoutPassthrough(t *testing.T) {
	got, err := withGuestCDIDevices(&specs.Spec{Annotations: map[string]string{
		"cdi.k8s.io/gpu": "nvidia.com/pgpu=0",
	}})
	if err != nil {
		t.Fatalf("withGuestCDIDevices: %v", err)
	}
	if _, ok := got.Annotations["cdi.k8s.io/gpu"]; ok {
		t.Error("inherited host CDI annotation must be stripped even with no allocation")
	}
	if v, ok := got.Annotations[guestCDIAnnotation]; ok {
		t.Errorf("no device allocated, but %s was set to %q", guestCDIAnnotation, v)
	}
}

// The carrier container shares the caller's spec and must not see these
// annotations — its rootfs is the read-only lower and it has no use for the
// device, so mutating in place would inject into it too.
func TestGuestCDIAnnotationDoesNotMutateCaller(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
	fakePCIDevice(t, "0000:da:00.0", "0x10de", "0x030200")
	spec := &specs.Spec{Annotations: map[string]string{"unrelated": "kept"}}
	if _, err := withGuestCDIDevices(spec); err != nil {
		t.Fatalf("withGuestCDIDevices: %v", err)
	}
	if _, ok := spec.Annotations[guestCDIAnnotation]; ok {
		t.Error("caller's spec was mutated; the carrier would inherit the annotation")
	}
}

// The device resolver matches the bare PCI_RESOURCE_ prefix, which every
// Kubernetes PCI plugin sets -- SR-IOV NICs, RDMA adapters, FPGAs. Annotating
// one of those with nvidia.com/gpu=all names a CDI device the guest cannot
// resolve, and an unresolvable CDI device FAILS CreateContainer rather than
// being ignored, so a non-GPU passthrough actor would not start at all.
func TestGuestCDIAnnotationSkipsNonGPUPassthrough(t *testing.T) {
	dir := t.TempDir()
	// An SR-IOV NIC: right prefix, wrong device.
	if err := os.WriteFile(filepath.Join(dir, "vendor"), []byte("0x8086\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "class"), []byte("0x020000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isNVIDIAGPU(dir) {
		t.Error("an Intel network device must not be treated as an NVIDIA GPU")
	}

	gpu := t.TempDir()
	if err := os.WriteFile(filepath.Join(gpu, "vendor"), []byte("0x10de\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gpu, "class"), []byte("0x030200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isNVIDIAGPU(gpu) {
		t.Error("a 3D-controller NVIDIA device is a GPU")
	}

	// The audio function arrives in the same IOMMU group and is not a GPU.
	audio := t.TempDir()
	if err := os.WriteFile(filepath.Join(audio, "vendor"), []byte("0x10de\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(audio, "class"), []byte("0x040300\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isNVIDIAGPU(audio) {
		t.Error("the card's HDMI-audio function must not be treated as a GPU")
	}
}

// The whole path, not just the predicate: a worker allocated an SR-IOV NIC
// through the same PCI_RESOURCE_ convention gets no CDI annotation, so its
// container still starts.
func TestGuestCDIAnnotationAbsentForAnSRIOVWorker(t *testing.T) {
	t.Setenv("PCI_RESOURCE_INTEL_COM_SRIOV_NETDEVICE", "0000:3b:02.1")
	fakePCIDevice(t, "0000:3b:02.1", "0x8086", "0x020000")
	got, err := withGuestCDIDevices(&specs.Spec{})
	if err != nil {
		t.Fatalf("withGuestCDIDevices: %v", err)
	}
	if v, ok := got.Annotations[guestCDIAnnotation]; ok {
		t.Errorf("an SR-IOV NIC was annotated %s=%q; the guest cannot resolve that "+
			"CDI device and CreateContainer would fail", guestCDIAnnotation, v)
	}
}
