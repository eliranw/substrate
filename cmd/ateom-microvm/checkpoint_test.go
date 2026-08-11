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
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
)

// fakeVMInfo serves one canned /api/v1/vm.info body on a unix socket and
// returns a client pointed at it.
func fakeVMInfo(t *testing.T, body string) *ch.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "ch.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ch.NewClient(sock)
}

// The device tree always carries the VM's ordinary virtio devices, so "no
// passthrough attached" means no _vfio entries -- not an empty tree.
func TestSnapshotAllowedWhenNoPassthroughAttached(t *testing.T) {
	c := fakeVMInfo(t, `{"device_tree":{"__virtio-net0":{},"__virtio-blk0":{},"__fs0":{}}}`)
	if err := errIfPassthroughSnapshot(context.Background(), c); err != nil {
		t.Fatalf("a VM with only virtio devices should be snapshottable, got %v", err)
	}
}

func TestSnapshotRejectedWhilePassthroughAttached(t *testing.T) {
	c := fakeVMInfo(t, `{"device_tree":{"__virtio-net0":{},"_vfio0":{}}}`)
	err := errIfPassthroughSnapshot(context.Background(), c)
	if err == nil {
		t.Fatal("expected a refusal while a passthrough device is still attached")
	}
	// The id is the operator's only handle on which device did not eject.
	if !strings.Contains(err.Error(), "_vfio0") {
		t.Errorf("error should name the attached device, got %q", err)
	}
}

func TestSnapshotRejectionCountsEveryAttachedDevice(t *testing.T) {
	c := fakeVMInfo(t, `{"device_tree":{"_vfio0":{},"_vfio1":{}}}`)
	err := errIfPassthroughSnapshot(context.Background(), c)
	if err == nil {
		t.Fatal("expected a refusal on a multi-device worker")
	}
	if !strings.Contains(err.Error(), "2 passthrough device(s)") {
		t.Errorf("error should report both devices, got %q", err)
	}
}

// An unreadable device tree must not read as "nothing attached". This is the
// last check before the memory image is written, and treating an unanswerable
// VMM as clear is exactly how a torn snapshot gets produced.
func TestSnapshotRejectedWhenTheDeviceTreeCannotBeRead(t *testing.T) {
	c := fakeVMInfo(t, `{"config":{"devices":[]}}`) // no device_tree at all
	if err := errIfPassthroughSnapshot(context.Background(), c); err == nil {
		t.Fatal("an unreadable device tree must fail the gate, not pass it")
	}
}

// vmmPID gates the eject confirmation: without the pid, WaitDeviceRemoved cannot
// check that the VMM dropped its /dev/vfio group fd, so the detach must refuse
// rather than proceed on an unconfirmed eject.
func TestVMMPIDUnknownWithoutATrackedProcess(t *testing.T) {
	if got := vmmPID(nil); got != 0 {
		t.Errorf("vmmPID(nil) = %d, want 0", got)
	}
	if got := vmmPID(&runningActor{}); got != 0 {
		t.Errorf("vmmPID with no process = %d, want 0", got)
	}
}
