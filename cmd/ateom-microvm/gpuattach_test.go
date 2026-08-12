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
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
)

// recordingCH is a stand-in cloud-hypervisor that serves a device tree and
// records the calls made against it, so a test can assert what happened and in
// what order relative to the guest-side steps.
type recordingCH struct {
	mu   sync.Mutex
	log  *[]string
	tree string
}

func startRecordingCH(t *testing.T, log *[]string, deviceTree string) *ch.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "ch.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	f := &recordingCH{log: log, tree: deviceTree}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "vm.info"):
			// vm.info is polled; recording it would drown the ordering signal.
		case strings.HasSuffix(r.URL.Path, "vm.remove-device"):
			*f.log = append(*f.log, "remove:"+extractField(string(body), "id"))
		case strings.HasSuffix(r.URL.Path, "vm.add-device"):
			*f.log = append(*f.log, "add:"+extractField(string(body), "path"))
		}
		f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "vm.info") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.tree))
			return
		}
		if strings.HasSuffix(r.URL.Path, "vm.add-device") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"_vfio9","bdf":"0000:00:05.0"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ch.NewClient(sock)
}

// extractField pulls a string field out of a tiny JSON body without a decoder.
func extractField(body, key string) string {
	i := strings.Index(body, `"`+key+`":"`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(key)+4:]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j]
	}
	return ""
}

// stubEject records the eject confirmations instead of inspecting a live VMM.
func stubEject(t *testing.T, log *[]string) {
	t.Helper()
	orig := waitDeviceGone
	waitDeviceGone = func(c *ch.Client, ctx context.Context, id string, pid int, d time.Duration) error {
		*log = append(*log, "wait:"+id)
		return nil
	}
	t.Cleanup(func() { waitDeviceGone = orig })
}

// liveActor returns a runningActor whose chCmd is a real, running process, so
// vmmPID reports a usable pid.
func liveActor(t *testing.T) *runningActor {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return &runningActor{chCmd: cmd}
}

// The eject is the unbind: clh raises an ACPI eject and the guest kernel calls
// the bound driver's .remove(). Nothing is asked of the guest, which could not
// answer anyway -- NVIDIA's rootfs has no shell.
func TestDetachEjectsWithoutAskingTheGuest(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{"_vfio2":{},"_net0":{}}}`)
	stubEject(t, &log)

	s := &AteomService{}
	if err := s.detachPassthrough(context.Background(), c, liveActor(t), "actor-1"); err != nil {
		t.Fatalf("detachPassthrough: %v", err)
	}
	want := []string{"remove:_vfio2", "wait:_vfio2"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("call order = %v, want %v", log, want)
	}
}

// Every eject is REQUESTED before any is awaited. Interleaving them would
// serialise ejects the guest can run concurrently, turning one slow device into
// a timeout for all of them.
func TestDetachRequestsAllEjectsBeforeWaiting(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{"_vfio2":{},"_vfio3":{}}}`)
	stubEject(t, &log)

	s := &AteomService{}
	if err := s.detachPassthrough(context.Background(), c, liveActor(t), "actor-1"); err != nil {
		t.Fatalf("detachPassthrough: %v", err)
	}
	lastRemove, firstWait := -1, len(log)
	for i, e := range log {
		if strings.HasPrefix(e, "remove:") {
			lastRemove = i
		}
		if strings.HasPrefix(e, "wait:") && i < firstWait {
			firstWait = i
		}
	}
	if lastRemove > firstWait {
		t.Errorf("a wait ran before the last eject was requested: %v", log)
	}
}

// A VM with nothing attached must not touch the guest at all -- detach is called
// unconditionally, including for every ordinary actor on a non-GPU worker.
func TestDetachIsANoOpWithoutPassthrough(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{"_net0":{},"__rng":{}}}`)
	stubEject(t, &log)

	s := &AteomService{}
	if err := s.detachPassthrough(context.Background(), c, nil, "actor-1"); err != nil {
		t.Fatalf("detachPassthrough on a device-free VM: %v", err)
	}
	if len(log) != 0 {
		t.Errorf("expected no VMM calls, got %v", log)
	}
}

// Without the VMM pid the eject cannot be confirmed, and an unconfirmed eject is
// the torn-snapshot bug. It must refuse BEFORE requesting anything, so the
// device is left in a known state rather than mid-eject.
func TestDetachRefusesWhenTheEjectCannotBeConfirmed(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{"_vfio2":{}}}`)
	stubEject(t, &log)

	s := &AteomService{}
	err := s.detachPassthrough(context.Background(), c, nil, "actor-1") // nil ra -> pid 0
	if err == nil {
		t.Fatal("expected a refusal when the cloud-hypervisor pid is unknown")
	}
	for _, e := range log {
		if strings.HasPrefix(e, "remove:") {
			t.Errorf("must not request an eject it cannot confirm: %v", log)
		}
	}
}

func TestAttachAddsEachAllocatedDevice(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{"_vfio9":{}}}`)

	s := &AteomService{}
	if err := s.attachPassthrough(context.Background(), c, "actor-1"); err != nil {
		t.Fatalf("attachPassthrough: %v", err)
	}
	want := []string{"add:/sys/bus/pci/devices/0000:da:00.0/"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("call order = %v, want %v", log, want)
	}
}

// Attach must not report success on a device the VMM never took: the actor
// would come back believing it has a GPU it cannot use.
//
// Exercised through waitDevicesAttached rather than attachPassthrough so the
// deadline can be short -- going through attachPassthrough would burn the real
// 30s settle timeout to assert a timeout.
func TestWaitDevicesAttachedFailsWhenTheDeviceDoesNotComeBack(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{"_net0":{}}}`) // no _vfio entry

	err := waitDevicesAttached(context.Background(), c, 1, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the device never appears in the device tree")
	}
	if !strings.Contains(err.Error(), "0 of 1") {
		t.Errorf("error should report the shortfall, got %q", err)
	}
}

// An unreadable device tree must not read as "it came back".
func TestWaitDevicesAttachedFailsOnAnUnreadableTree(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"config":{}}`) // no device_tree

	err := waitDevicesAttached(context.Background(), c, 1, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the device tree cannot be read")
	}
	if !strings.Contains(err.Error(), "could not confirm") {
		t.Errorf("error should name the unreadable tree, got %q", err)
	}
}

// An ordinary actor restores through this path too.
func TestAttachIsANoOpWithoutAnAllocation(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{}}`)

	s := &AteomService{}
	if err := s.attachPassthrough(context.Background(), c, "actor-1"); err != nil {
		t.Fatalf("attachPassthrough with no allocation: %v", err)
	}
	if len(log) != 0 {
		t.Errorf("expected no calls, got %v", log)
	}
}
