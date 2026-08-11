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
)

func TestSnapshotRejectedWithPassthroughDevice(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
	err := errIfPassthroughSnapshot()
	if err == nil {
		t.Fatal("expected snapshot of an actor holding a passthrough device to be rejected")
	}
	if !strings.Contains(err.Error(), "passthrough device") {
		t.Errorf("error should name the blocking condition, got %q", err.Error())
	}
}

func TestSnapshotAllowedWithoutPassthroughDevice(t *testing.T) {
	// No PCI_RESOURCE_* env -> no passthrough device -> snapshots allowed.
	if err := errIfPassthroughSnapshot(); err != nil {
		t.Fatalf("a worker with no passthrough device should be snapshottable, got %v", err)
	}
}

// The gate must not depend on per-actor in-memory state. Two holes existed when
// it keyed off per-actor state: RestoreWorkload never set it (so every restored
// actor sailed through), and a nil runningActor after an ateom restart was
// treated as device-free. Keying off the worker's own allocation closes both --
// these cases are indistinguishable from a fresh actor here, by construction.
func TestSnapshotGateIndependentOfActorState(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
	// No runningActor exists at all (restored actor / post-restart ateom).
	if err := errIfPassthroughSnapshot(); err == nil {
		t.Fatal("gate must reject even with no tracked actor state")
	}
}

func TestSnapshotGateRejectsMultipleDevices(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:61:00.0,0000:da:00.0")
	err := errIfPassthroughSnapshot()
	if err == nil {
		t.Fatal("expected rejection when several devices are allocated")
	}
	if !strings.Contains(err.Error(), "2 passthrough device(s)") {
		t.Errorf("error should report both devices, got %q", err.Error())
	}
}
