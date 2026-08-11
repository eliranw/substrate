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

// stubGuestSeams replaces the guest-side and eject steps with recorders.
func stubGuestSeams(t *testing.T, log *[]string, bdfs []string) {
	t.Helper()
	oG, oD, oV, oW := guestGPUBDFs, guestDetachGPU, guestVerifyBound, waitDeviceGone
	guestGPUBDFs = func(ctx context.Context, vsock string) ([]string, error) {
		*log = append(*log, "scan")
		return bdfs, nil
	}
	guestDetachGPU = func(ctx context.Context, vsock, bdf string) error {
		*log = append(*log, "unbind:"+bdf)
		return nil
	}
	guestVerifyBound = func(ctx context.Context, vsock, bdf string, d time.Duration) error {
		*log = append(*log, "verify:"+bdf)
		return nil
	}
	waitDeviceGone = func(c *ch.Client, ctx context.Context, id string, pid int, d time.Duration) error {
		*log = append(*log, "wait:"+id)
		return nil
	}
	t.Cleanup(func() {
		guestGPUBDFs, guestDetachGPU, guestVerifyBound, waitDeviceGone = oG, oD, oV, oW
	})
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

// The ordering is the correctness argument: the guest must release the device
// before the VMM ejects it, because nothing quiesces a bus-mastering device and
// ejecting one the driver still owns leaves it free to DMA into guest RAM while
// the memory image is written.
func TestDetachUnbindsInGuestBeforeEjecting(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{"_vfio2":{},"_net0":{}}}`)
	stubGuestSeams(t, &log, []string{"0000:00:02.0"})

	s := &AteomService{}
	if err := s.detachPassthrough(context.Background(), c, liveActor(t), "actor-1"); err != nil {
		t.Fatalf("detachPassthrough: %v", err)
	}
	want := []string{"scan", "unbind:0000:00:02.0", "remove:_vfio2", "wait:_vfio2"}
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
	stubGuestSeams(t, &log, []string{"0000:00:02.0", "0000:00:03.0"})

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
	stubGuestSeams(t, &log, []string{"0000:00:02.0"})

	s := &AteomService{}
	if err := s.detachPassthrough(context.Background(), c, nil, "actor-1"); err != nil {
		t.Fatalf("detachPassthrough on a device-free VM: %v", err)
	}
	if len(log) != 0 {
		t.Errorf("expected no guest or VMM calls, got %v", log)
	}
}

// Without the VMM pid the eject cannot be confirmed, and an unconfirmed eject is
// the torn-snapshot bug. It must refuse BEFORE requesting anything, so the
// device is left in a known state rather than mid-eject.
func TestDetachRefusesWhenTheEjectCannotBeConfirmed(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{"_vfio2":{}}}`)
	stubGuestSeams(t, &log, []string{"0000:00:02.0"})

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

// A guest that will not release the device must stop the detach before the
// eject, not after: ejecting a device the driver still owns is the exact state
// the guest step exists to prevent.
func TestDetachStopsWhenTheGuestWillNotRelease(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{"_vfio2":{}}}`)
	stubGuestSeams(t, &log, []string{"0000:00:02.0"})
	guestDetachGPU = func(ctx context.Context, vsock, bdf string) error {
		log = append(log, "unbind-failed")
		return fmt.Errorf("still bound")
	}

	s := &AteomService{}
	if err := s.detachPassthrough(context.Background(), c, liveActor(t), "actor-1"); err == nil {
		t.Fatal("expected the detach to fail when the guest will not release")
	}
	for _, e := range log {
		if strings.HasPrefix(e, "remove:") {
			t.Errorf("must not eject a device the guest still owns: %v", log)
		}
	}
}

func TestAttachAddsThenVerifiesEachDeviceBound(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{}}`)
	stubGuestSeams(t, &log, []string{"0000:00:02.0"})

	s := &AteomService{}
	if err := s.attachPassthrough(context.Background(), c, "actor-1"); err != nil {
		t.Fatalf("attachPassthrough: %v", err)
	}
	want := []string{"add:/sys/bus/pci/devices/0000:da:00.0/", "scan", "verify:0000:00:02.0"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("call order = %v, want %v", log, want)
	}
}

// An ordinary actor restores through this path too.
func TestAttachIsANoOpWithoutAnAllocation(t *testing.T) {
	var log []string
	c := startRecordingCH(t, &log, `{"device_tree":{}}`)
	stubGuestSeams(t, &log, []string{"0000:00:02.0"})

	s := &AteomService{}
	if err := s.attachPassthrough(context.Background(), c, "actor-1"); err != nil {
		t.Fatalf("attachPassthrough with no allocation: %v", err)
	}
	if len(log) != 0 {
		t.Errorf("expected no calls, got %v", log)
	}
}

// Hot-plug is asynchronous, so a short first read is expected rather than a
// failure -- binding only the devices that appeared first would silently leave
// the rest unusable.
func TestWaitGuestGPUsPollsUntilAllAppear(t *testing.T) {
	orig := guestGPUBDFs
	t.Cleanup(func() { guestGPUBDFs = orig })
	calls := 0
	guestGPUBDFs = func(ctx context.Context, vsock string) ([]string, error) {
		calls++
		switch calls {
		case 1:
			return nil, nil // guest has not processed the hot-plug yet
		case 2:
			return []string{"0000:00:02.0"}, nil // only one of two so far
		}
		return []string{"0000:00:02.0", "0000:00:03.0"}, nil
	}
	got, err := waitGuestGPUs(context.Background(), "/tmp/vsock", 2, 3*time.Second)
	if err != nil {
		t.Fatalf("waitGuestGPUs: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want both devices", got)
	}
}

func TestWaitGuestGPUsReportsAShortfallOnTimeout(t *testing.T) {
	orig := guestGPUBDFs
	t.Cleanup(func() { guestGPUBDFs = orig })
	guestGPUBDFs = func(ctx context.Context, vsock string) ([]string, error) {
		return []string{"0000:00:02.0"}, nil
	}
	_, err := waitGuestGPUs(context.Background(), "/tmp/vsock", 2, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout when only one of two devices appears")
	}
	if !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("error should report the shortfall, got %q", err)
	}
}
