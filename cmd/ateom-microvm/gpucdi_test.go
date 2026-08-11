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
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestGuestCDIAnnotationSetOnPassthroughWorker(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
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
	spec := &specs.Spec{Annotations: map[string]string{"unrelated": "kept"}}
	if _, err := withGuestCDIDevices(spec); err != nil {
		t.Fatalf("withGuestCDIDevices: %v", err)
	}
	if _, ok := spec.Annotations[guestCDIAnnotation]; ok {
		t.Error("caller's spec was mutated; the carrier would inherit the annotation")
	}
}
