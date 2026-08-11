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

package ch

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProcFD builds a fixture /proc: <root>/<pid>/cmdline holding a VMM argv
// that serves apiSocket, and <root>/<pid>/fd holding one symlink per target,
// each named after its fd number and pointing at <root>/<target>.
//
// A target of "dev/vfio/66" therefore produces a readable link ending in
// /dev/vfio/66 — the shape vfioGroupFD must recognise — and "dev/vfio/vfio" one
// it must not, so neither outcome can be reached by the fixture silently
// building nothing.
func fakeProcFD(t *testing.T, pid, apiSocket string, targets ...string) string {
	t.Helper()
	root := t.TempDir()
	fdDir := filepath.Join(root, pid, "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// /proc renders argv NUL-separated, as `cloud-hypervisor --api-socket <path>`.
	argv := strings.Join([]string{"cloud-hypervisor", "--api-socket", apiSocket}, "\x00") + "\x00"
	if err := os.WriteFile(filepath.Join(root, pid, "cmdline"), []byte(argv), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, tgt := range targets {
		dst := filepath.Join(root, tgt)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dst, filepath.Join(fdDir, strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestWaitDeviceRemovedSucceedsWhenTreeAndGroupFDAreGone(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"__rng":{}}}`))
	}))
	// The container control node, which CH keeps after an eject, and a path that
	// merely begins like a group node. Neither pins the device, so the match has
	// to be anchored at the end of the path rather than merely contained in it.
	procFDDir = fakeProcFD(t, "42", c.apiSocket, "dev/vfio/vfio", "dev/vfio/66.bak")
	t.Cleanup(func() { procFDDir = "/proc" })

	if err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 3*time.Second); err != nil {
		t.Fatalf("WaitDeviceRemoved: %v", err)
	}
}

func TestWaitDeviceRemovedFailsWhileGroupFDIsHeld(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"__rng":{}}}`)) // tree already clear...
	}))
	procFDDir = fakeProcFD(t, "42", c.apiSocket, "dev/vfio/66") // ...but the GROUP fd is still open
	t.Cleanup(func() { procFDDir = "/proc" })

	err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected failure while the vfio group fd is still held")
	}
	if !strings.Contains(err.Error(), "vfio") {
		t.Errorf("error should mention the vfio fd, got %q", err.Error())
	}
}

// The kernel appends " (deleted)" to a /proc/<pid>/fd target whose backing
// dentry was unlinked. Such an fd pins the device exactly as firmly, so a match
// that insists on a bare path reports success with the group wide open.
func TestWaitDeviceRemovedCountsAnUnlinkedGroupNode(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"__rng":{}}}`))
	}))
	procFDDir = fakeProcFD(t, "42", c.apiSocket, "dev/vfio/66 (deleted)")
	t.Cleanup(func() { procFDDir = "/proc" })

	err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected failure: an unlinked group node still pins the device")
	}
	if !strings.Contains(err.Error(), "vfio") {
		t.Errorf("error should mention the vfio fd, got %q", err.Error())
	}
}

func TestWaitDeviceRemovedFailsWhileStillInDeviceTree(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"_vfio0":{},"__rng":{}}}`))
	}))
	procFDDir = fakeProcFD(t, "42", c.apiSocket) // no fds at all
	t.Cleanup(func() { procFDDir = "/proc" })

	if err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 500*time.Millisecond); err == nil {
		t.Fatal("expected failure while the device is still in the device tree")
	}
}

// The failure that matters most: if we cannot READ the fd directory we must not
// report success. A check that cannot distinguish "absent" from "could not look"
// is not a check.
func TestWaitDeviceRemovedFailsWhenFDsCannotBeObserved(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"__rng":{}}}`))
	}))
	// The process is plainly our VMM; only its fd directory is unreadable.
	root := fakeProcFD(t, "42", c.apiSocket)
	if err := os.RemoveAll(filepath.Join(root, "42", "fd")); err != nil {
		t.Fatal(err)
	}
	procFDDir = root
	t.Cleanup(func() { procFDDir = "/proc" })

	err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected failure when the fd directory cannot be read")
	}
}

// The same rule one level down: a readlink that fails for any reason other than
// "the fd closed under us" leaves that fd's target unknown, and an unknown fd
// could be the group node. Counting it as "not a vfio fd" would be a guess.
func TestWaitDeviceRemovedFailsWhenAnFDTargetCannotBeRead(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"__rng":{}}}`))
	}))
	root := fakeProcFD(t, "42", c.apiSocket)
	// A regular file where /proc would have a symlink: readlink fails EINVAL,
	// which is "could not look", not "nothing here".
	if err := os.WriteFile(filepath.Join(root, "42", "fd", "0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	procFDDir = root
	t.Cleanup(func() { procFDDir = "/proc" })

	if err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 500*time.Millisecond); err == nil {
		t.Fatal("expected failure when an fd's target cannot be read")
	}
}

// The hardware false success: a miscomputed pid that happens to name a live
// process reads cleanly and holds no VFIO fds, which is indistinguishable from a
// completed eject unless the pid itself is proven to be this VM's VMM.
func TestWaitDeviceRemovedFailsWhenThePIDIsNotThisVMM(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"__rng":{}}}`))
	}))
	// A live cloud-hypervisor with a perfectly readable, entirely clean fd
	// directory — it just serves a different actor's VM.
	procFDDir = fakeProcFD(t, "42", "/run/vc/vm/some-other-actor/clh-api.sock")
	t.Cleanup(func() { procFDDir = "/proc" })

	err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected failure when the pid does not serve this VM's api-socket")
	}
	if !strings.Contains(err.Error(), "api-socket") {
		t.Errorf("error should name the api-socket mismatch, got %q", err.Error())
	}
}

// A vm.info carrying no device_tree says nothing about what is attached, and
// DeviceIDs reports that as an error rather than an empty list. Swallowing it
// here would turn "could not look" into "the device is gone" and clear the way to
// snapshot with the device still live.
func TestWaitDeviceRemovedFailsWhenTheDeviceTreeCannotBeRead(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"Running"}`))
	}))
	procFDDir = fakeProcFD(t, "42", c.apiSocket) // fds are clean, so only the tree read can fail
	t.Cleanup(func() { procFDDir = "/proc" })

	if err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 500*time.Millisecond); err == nil {
		t.Fatal("expected failure when vm.info carries no device_tree")
	}
}

// The deadline has to bound the whole wait, not just the sleeps between polls:
// cloud-hypervisor can be swapped out and leave a request outstanding, and a
// checkpoint that hangs forever is worse than one that fails.
func TestWaitDeviceRemovedDeadlineBoundsAStalledRequest(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	procFDDir = fakeProcFD(t, "42", c.apiSocket)
	t.Cleanup(func() { procFDDir = "/proc" })

	done := make(chan error, 1)
	go func() {
		done <- c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 200*time.Millisecond)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected failure when vm.info never answers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDeviceRemoved ignored its deadline and is still blocked on vm.info")
	}
}

// The deadline bounds the wait by cancelling an in-flight vm.info, but that
// cancellation must not become the reported reason: an eject stuck behind a slow
// VMM is exactly the failure that most needs its state named. Here the deadline
// deliberately lands mid-request rather than during a sleep.
func TestWaitDeviceRemovedTimeoutNamesTheBlockingState(t *testing.T) {
	var calls atomic.Int32
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first poll answers at once, so a state is observed; every later
		// poll stalls past the deadline.
		if calls.Add(1) > 1 {
			select {
			case <-time.After(300 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write([]byte(`{"device_tree":{"__rng":{}}}`))
	}))
	procFDDir = fakeProcFD(t, "42", c.apiSocket, "dev/vfio/66")
	t.Cleanup(func() { procFDDir = "/proc" })

	err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected failure while the vfio group fd is still held")
	}
	if !strings.Contains(err.Error(), "vfio group fd") {
		t.Errorf("timeout should name the state that blocked it, got %q", err.Error())
	}
}

// An empty api-socket would make the cmdline match vacuously true, which is the
// one answer this check must never give.
func TestAssertIsVMMRejectsAClientWithNoAPISocket(t *testing.T) {
	procFDDir = fakeProcFD(t, "42", "/run/vc/vm/some-actor/clh-api.sock")
	t.Cleanup(func() { procFDDir = "/proc" })

	if err := (&Client{}).assertIsVMM(42); err == nil {
		t.Fatal("expected an error when the client has no api-socket to match against")
	}
}

func TestWaitDeviceRemovedHonoursContextCancellation(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"_vfio0":{}}}`)) // never clears
	}))
	procFDDir = fakeProcFD(t, "42", c.apiSocket)
	t.Cleanup(func() { procFDDir = "/proc" })

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	defer cancel()

	// A deadline far longer than the cancellation, so only cancellation can end
	// this wait.
	err := c.WaitDeviceRemoved(ctx, "_vfio0", 42, 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestWaitDeviceRemovedPollsUntilClear(t *testing.T) {
	var calls atomic.Int32
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			_, _ = w.Write([]byte(`{"device_tree":{"_vfio0":{}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"device_tree":{}}`))
	}))
	procFDDir = fakeProcFD(t, "42", c.apiSocket)
	t.Cleanup(func() { procFDDir = "/proc" })

	if err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 5*time.Second); err != nil {
		t.Fatalf("WaitDeviceRemoved: %v", err)
	}
	if calls.Load() < 3 {
		t.Errorf("expected polling, got %d calls", calls.Load())
	}
}
