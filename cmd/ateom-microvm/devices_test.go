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
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
)

func paths(devs []ch.DeviceConfig) []string {
	var p []string
	for _, d := range devs {
		p = append(p, d.Path)
	}
	return p
}

func TestResolveWorkerGPUs_SingleAllocation(t *testing.T) {
	// The device plugin advertises the one GPU it allocated to this pod.
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
	got, err := resolveWorkerDevices()
	if err != nil {
		t.Fatalf("resolveWorkerDevices: %v", err)
	}
	want := []string{"/sys/bus/pci/devices/0000:da:00.0/"}
	if !equalStrings(paths(got), want) {
		t.Fatalf("got %v, want %v", paths(got), want)
	}
	if got[0].Iommu {
		t.Errorf("Iommu = true, want false (direct passthrough)")
	}
}

func TestResolveWorkerGPUs_MultiAllocation(t *testing.T) {
	// A 2-GPU worker: the plugin lists both BDFs, comma-separated. Deduped + sorted.
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:61:00.0,0000:da:00.0,0000:61:00.0")
	got, err := resolveWorkerDevices()
	if err != nil {
		t.Fatalf("resolveWorkerDevices: %v", err)
	}
	want := []string{
		"/sys/bus/pci/devices/0000:61:00.0/",
		"/sys/bus/pci/devices/0000:da:00.0/",
	}
	if !equalStrings(paths(got), want) {
		t.Fatalf("got %v, want %v", paths(got), want)
	}
}

func TestResolveWorkerGPUs_NoAllocation(t *testing.T) {
	// No PCI_RESOURCE_NVIDIA_COM_* env -> non-GPU worker -> nil.
	got, err := resolveWorkerDevices()
	if err != nil {
		t.Fatalf("resolveWorkerDevices: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil on a non-GPU worker, got %v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildVMConfigIncludesPassthroughDevices(t *testing.T) {
	devs := []ch.DeviceConfig{{Path: "/sys/bus/pci/devices/0000:01:00.0/"}}
	cfg := buildVMConfig("uid", "vmlinux", "rootfs.img", "", "/tmp/serial.log", 2048, 2, false, devs)
	if len(cfg.Devices) != 1 || cfg.Devices[0].Path != devs[0].Path {
		t.Fatalf("buildVMConfig Devices = %v, want %v", cfg.Devices, devs)
	}
}

func TestBuildVMConfigDiskBoot(t *testing.T) {
	cfg := buildVMConfig("uid", "vmlinux", "rootfs.img", "", "/tmp/serial.log", 2048, 2, false, nil)
	if len(cfg.Disks) != 1 || cfg.Disks[0].Path != "rootfs.img" {
		t.Errorf("disk boot Disks = %v, want the rootfs.img disk", cfg.Disks)
	}
	if !strings.Contains(cfg.Payload.Cmdline, "root=/dev/vda1") {
		t.Errorf("disk boot cmdline missing root=/dev/vda1: %q", cfg.Payload.Cmdline)
	}
}
