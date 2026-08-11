# microVM GPU attach/detach Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make GPU micro-VM actors snapshottable — detach the GPU before every snapshot and re-attach it on resume — so golden snapshots, fast fork and suspend/resume all work.

**Architecture:** The GPU is attached only while an actor runs. `CheckpointWorkload` detaches (stop `nvidia-persistenced` → unbind in guest → `vm.remove-device` → verify the VFIO group fd is released) before pausing and snapshotting. `RestoreWorkload` re-attaches (`vm.add-device` → verify the driver re-bound). Boot and teardown are unchanged. The golden snapshot needs no special case: it is an ordinary suspend.

**Tech Stack:** Go; cloud-hypervisor REST API over a unix socket; kata debug console over vsock; Linux sysfs PCI bind/unbind.

**Spec:** `docs/superpowers/specs/2026-08-10-microvm-gpu-attach-detach-design.md`

## Global Constraints

- Guest-facing / sysfs-reading Go files carry `//go:build linux`. Tests for them run on Linux only — on darwin use `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=mod golang:1.26 go test ...`.
- Module path `github.com/agent-substrate/substrate`; ch package is `github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch`.
- Copy the Apache license header block from a sibling file into every new `.go` file.
- TDD: failing test → watch it fail → minimal implementation → watch it pass → commit.
- **Never trust an HTTP status as proof of device state.** `vm.remove-device` returns 204 meaning "eject requested". Verify by observed state, and fail loudly if the state cannot be observed (spec D5).
- **Never snapshot with a VFIO device attached.** `errIfPassthroughSnapshot` stays as the final assertion.
- Out of scope here: the guest-image bump (spec §12), `cuda-checkpoint` (spec §13), and the C5 container-injection fallback (only needed if §12 step 5 fails).

---

## File structure

| File | Responsibility |
|---|---|
| `cmd/ateom-microvm/internal/ch/device.go` *(new)* | `AddDevice` / `RemoveDevice` / `DeviceIDs` — the clh device API, plus the `putJSON` helper needed to capture `add-device`'s response body |
| `cmd/ateom-microvm/internal/ch/device_test.go` *(new)* | httptest-backed tests for the above |
| `cmd/ateom-microvm/internal/ch/eject.go` *(new)* | `WaitDeviceRemoved` — the completion oracle (device tree + VFIO group fd), with a deadline |
| `cmd/ateom-microvm/internal/ch/eject_test.go` *(new)* | oracle tests incl. the "cannot observe" case |
| `cmd/ateom-microvm/internal/kata/gpuguest.go` *(new)* | guest-side detach and attach-verify, driven over the debug console |
| `cmd/ateom-microvm/internal/kata/gpuguest_test.go` *(new)* | command-shape and parsing tests |
| `cmd/ateom-microvm/gpudetach.go` *(new)* | orchestration: `detachGPUsForSnapshot`, `attachGPUsAfterRestore` |
| `cmd/ateom-microvm/checkpoint.go` *(modify)* | call the detach before `Pause`/`Snapshot` |
| `cmd/ateom-microvm/restore.go` *(modify)* | call the attach after `Resume` |
| `cmd/ateom-microvm/run.go` *(modify)* | record attached device IDs on `runningActor` |

---

### Task 1: clh device API — add, remove, list

**Files:**
- Create: `cmd/ateom-microvm/internal/ch/device.go`
- Create: `cmd/ateom-microvm/internal/ch/device_test.go`
- Modify: `cmd/ateom-microvm/internal/ch/api.go` (add `putJSON` beside the existing `put`, ~line 99)

**Interfaces:**
- Consumes: existing `ch.Client`, `ch.DeviceConfig{Path string; Iommu bool}` (`internal/ch/createvm.go:49`), `apiClient.put` / `apiClient.getJSON` (`internal/ch/api.go`).
- Produces:
  - `func (c *Client) AddDevice(ctx context.Context, path string) (AddedDevice, error)`
  - `type AddedDevice struct { ID string \`json:"id"\`; BDF string \`json:"bdf"\` }`
  - `func (c *Client) RemoveDevice(ctx context.Context, id string) error`
  - `func (c *Client) DeviceIDs(ctx context.Context) ([]string, error)`

- [ ] **Step 1: Write the failing test** (`cmd/ateom-microvm/internal/ch/device_test.go`)

```go
// <Apache header copied from ch.go>

package ch

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// serveUnix starts an HTTP server on a unix socket and returns a Client for it.
func serveUnix(t *testing.T, h http.Handler) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "ch.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.Remove(sock) })
	return NewClient(sock)
}

func TestAddDeviceReturnsIDAndBDF(t *testing.T) {
	var gotPath string
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/vm.add-device" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotPath = body.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"_vfio1","bdf":"0000:00:02.0"}`))
	}))

	got, err := c.AddDevice(context.Background(), "/sys/bus/pci/devices/0000:3b:00.0/")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	if gotPath != "/sys/bus/pci/devices/0000:3b:00.0/" {
		t.Errorf("sent path = %q", gotPath)
	}
	if got.ID != "_vfio1" || got.BDF != "0000:00:02.0" {
		t.Errorf("got %+v, want id=_vfio1 bdf=0000:00:02.0", got)
	}
}

func TestRemoveDeviceSendsID(t *testing.T) {
	var gotID string
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotID = body.ID
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := c.RemoveDevice(context.Background(), "_vfio0"); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	if gotID != "_vfio0" {
		t.Errorf("sent id = %q, want _vfio0", gotID)
	}
}

func TestRemoveDeviceUnknownIDIsAnError(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`Error from device manager: UnknownDeviceId("_device0")`))
	}))
	if err := c.RemoveDevice(context.Background(), "_device0"); err == nil {
		t.Fatal("expected an error for an unknown device id")
	}
}

func TestDeviceIDsListsOnlyDeviceTreeKeys(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"Running","device_tree":{"_vfio0":{},"__rng":{},"__serial":{}}}`))
	}))
	ids, err := c.DeviceIDs(context.Background())
	if err != nil {
		t.Fatalf("DeviceIDs: %v", err)
	}
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["_vfio0"] || !found["__rng"] || len(ids) != 3 {
		t.Fatalf("ids = %v, want the three device_tree keys", ids)
	}
}
```

- [ ] **Step 2: Run the tests, verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=mod golang:1.26 go test ./cmd/ateom-microvm/internal/ch/ -run 'TestAddDevice|TestRemoveDevice|TestDeviceIDs' -v`
Expected: FAIL — `c.AddDevice`, `c.RemoveDevice`, `c.DeviceIDs` undefined.

- [ ] **Step 3: Add the `putJSON` helper** to `cmd/ateom-microvm/internal/ch/api.go`, directly after the existing `put` method

```go
// putJSON issues a PUT with a JSON body and decodes the 2xx JSON response into
// out. Unlike put, it keeps the response body: vm.add-device answers 200 with
// the id cloud-hypervisor assigned, which the caller needs in order to eject the
// device later.
func (c *apiClient) putJSON(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, apiBase+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(raw))
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
```

- [ ] **Step 4: Create** `cmd/ateom-microvm/internal/ch/device.go`

```go
// <Apache header copied from ch.go>

package ch

import (
	"context"
	"fmt"
)

// AddedDevice is cloud-hypervisor's answer to vm.add-device. The ID is assigned
// by CH and CHANGES between attachments of the same physical GPU (_vfio0 ->
// _vfio1 across a detach/re-attach cycle), so it must be captured here rather
// than reconstructed: it is the handle required to eject the device later.
type AddedDevice struct {
	ID  string `json:"id"`
	BDF string `json:"bdf"`
}

// AddDevice hot-plugs a VFIO PCI device into a running VM. path is the host
// sysfs directory of the device, e.g. /sys/bus/pci/devices/0000:3b:00.0/.
func (c *Client) AddDevice(ctx context.Context, path string) (AddedDevice, error) {
	var out AddedDevice
	body := struct {
		Path string `json:"path"`
	}{Path: path}
	if err := c.api.putJSON(ctx, "/api/v1/vm.add-device", body, &out); err != nil {
		return AddedDevice{}, fmt.Errorf("vm.add-device %s: %w", path, err)
	}
	return out, nil
}

// RemoveDevice REQUESTS ejection of a device by the id CH assigned at add time.
// A nil error means the request was accepted, NOT that the device is gone: the
// real teardown runs when the guest executes the ACPI _EJ0 method. Callers must
// confirm with WaitDeviceRemoved before assuming the device is released.
func (c *Client) RemoveDevice(ctx context.Context, id string) error {
	body := struct {
		ID string `json:"id"`
	}{ID: id}
	if err := c.api.put(ctx, "/api/v1/vm.remove-device", body); err != nil {
		return fmt.Errorf("vm.remove-device %s: %w", id, err)
	}
	return nil
}

// DeviceIDs returns the ids present in the VM's device tree. This is the
// authoritative view of what the VMM still has attached; VmInfo.config is
// cleared as soon as an eject is requested and will report a device as gone
// while its teardown is still in flight.
func (c *Client) DeviceIDs(ctx context.Context) ([]string, error) {
	var info struct {
		DeviceTree map[string]any `json:"device_tree"`
	}
	if err := c.api.getJSON(ctx, "/api/v1/vm.info", &info); err != nil {
		return nil, fmt.Errorf("vm.info: %w", err)
	}
	ids := make([]string, 0, len(info.DeviceTree))
	for id := range info.DeviceTree {
		ids = append(ids, id)
	}
	return ids, nil
}
```

- [ ] **Step 5: Run the tests, verify they pass**

Run: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=mod golang:1.26 go test ./cmd/ateom-microvm/internal/ch/ -run 'TestAddDevice|TestRemoveDevice|TestDeviceIDs' -v`
Expected: PASS (4 tests).

- [ ] **Step 6: Commit**

```bash
git add cmd/ateom-microvm/internal/ch/device.go cmd/ateom-microvm/internal/ch/device_test.go cmd/ateom-microvm/internal/ch/api.go
git commit -m "feat(ch): vm.add-device / vm.remove-device / device-tree listing

add-device answers 200 with the id CH assigned, which changes between
attachments of the same GPU and is the handle needed to eject it later, so
capture it via a new putJSON helper rather than discarding the body."
```

---

### Task 2: eject completion oracle

`vm.remove-device` returning 204 means "requested". This task builds the only trustworthy completion signal, and makes an unobservable state a loud failure rather than a silent success — during hardware validation a naive poll reported success because both of its checks were vacuous (spec D5).

**Files:**
- Create: `cmd/ateom-microvm/internal/ch/eject.go`
- Create: `cmd/ateom-microvm/internal/ch/eject_test.go`

**Interfaces:**
- Consumes: `Client.DeviceIDs` (Task 1).
- Produces: `func (c *Client) WaitDeviceRemoved(ctx context.Context, id string, vmmPID int, deadline time.Duration) error`
- Produces: package-level `var procFDDir = "/proc"` (overridable in tests).

- [ ] **Step 1: Write the failing test** (`cmd/ateom-microvm/internal/ch/eject_test.go`)

```go
// <Apache header copied from ch.go>

package ch

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProcFD builds <root>/<pid>/fd containing the given symlink targets.
func fakeProcFD(t *testing.T, pid string, targets ...string) string {
	t.Helper()
	root := t.TempDir()
	fdDir := filepath.Join(root, pid, "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, tgt := range targets {
		real := filepath.Join(root, "target"+string(rune('a'+i)))
		if err := os.WriteFile(real, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		// name the symlink after the fd number; point it at a path containing tgt
		dst := filepath.Join(root, tgt)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dst, filepath.Join(fdDir, string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestWaitDeviceRemovedSucceedsWhenTreeAndGroupFDAreGone(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"__rng":{}}}`))
	}))
	procFDDir = fakeProcFD(t, "42", "dev/vfio/vfio") // container node only, no group
	t.Cleanup(func() { procFDDir = "/proc" })

	if err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 3*time.Second); err != nil {
		t.Fatalf("WaitDeviceRemoved: %v", err)
	}
}

func TestWaitDeviceRemovedFailsWhileGroupFDIsHeld(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"__rng":{}}}`)) // tree already clear...
	}))
	procFDDir = fakeProcFD(t, "42", "dev/vfio/66") // ...but the GROUP fd is still open
	t.Cleanup(func() { procFDDir = "/proc" })

	err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected failure while the vfio group fd is still held")
	}
	if !strings.Contains(err.Error(), "vfio") {
		t.Errorf("error should mention the vfio fd, got %q", err.Error())
	}
}

func TestWaitDeviceRemovedFailsWhileStillInDeviceTree(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_tree":{"_vfio0":{},"__rng":{}}}`))
	}))
	procFDDir = fakeProcFD(t, "42") // no fds at all
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
	procFDDir = t.TempDir() // no <pid>/fd inside
	t.Cleanup(func() { procFDDir = "/proc" })

	err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 4242, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected failure when the fd directory cannot be read")
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
	procFDDir = fakeProcFD(t, "42")
	t.Cleanup(func() { procFDDir = "/proc" })

	if err := c.WaitDeviceRemoved(context.Background(), "_vfio0", 42, 5*time.Second); err != nil {
		t.Fatalf("WaitDeviceRemoved: %v", err)
	}
	if calls.Load() < 3 {
		t.Errorf("expected polling, got %d calls", calls.Load())
	}
}
```

- [ ] **Step 2: Run the tests, verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=mod golang:1.26 go test ./cmd/ateom-microvm/internal/ch/ -run TestWaitDeviceRemoved -v`
Expected: FAIL — `WaitDeviceRemoved` and `procFDDir` undefined.

- [ ] **Step 3: Create** `cmd/ateom-microvm/internal/ch/eject.go`

```go
// <Apache header copied from ch.go>

package ch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// procFDDir is the /proc root used to inspect the VMM's open file descriptors.
// A var so tests can point it at a fixture.
var procFDDir = "/proc"

// vfioGroupFD matches a VFIO *group* fd, e.g. /dev/vfio/66. It deliberately does
// NOT match /dev/vfio/vfio (the container control node) or the anon_inode
// kvm-vfio entries: cloud-hypervisor retains those after an eject, and they do
// not pin the device. The group fd is the one that does.
var vfioGroupFD = regexp.MustCompile(`/dev/vfio/[0-9]+$`)

// WaitDeviceRemoved blocks until an ejection actually completed, or the deadline
// expires.
//
// This is necessary because vm.remove-device returns 204 meaning "eject
// REQUESTED": VmConfig is edited and an ACPI notify is fired immediately, but
// the real teardown only runs when the guest executes _EJ0. Snapshotting in that
// window produces a corrupt image, and vm_snapshot performs no VFIO check of its
// own.
//
// Two independent signals must both clear:
//   - the id disappears from the device tree (NOT from VmInfo.config, which is
//     cleared up front and would report success immediately), and
//   - the VFIO group fd leaves the VMM's /proc/<pid>/fd.
//
// The fd check is the stronger of the two: a partial eject failure can remove
// the tree node while the group is still held. If the fds cannot be READ, that
// is a failure, not a pass — an unobservable state must never look like success.
func (c *Client) WaitDeviceRemoved(ctx context.Context, id string, vmmPID int, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	var lastErr error
	for {
		inTree, err := c.deviceInTree(ctx, id)
		if err != nil {
			lastErr = err
		} else if !inTree {
			held, err := vfioGroupFDsHeld(vmmPID)
			switch {
			case err != nil:
				lastErr = fmt.Errorf("cannot observe VMM fds: %w", err)
			case held == 0:
				return nil
			default:
				lastErr = fmt.Errorf("VMM still holds %d vfio group fd(s)", held)
			}
		} else {
			lastErr = fmt.Errorf("device %s still in the device tree", id)
		}
		if time.Now().After(end) {
			return fmt.Errorf("device %s not ejected within %s: %w", id, deadline, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (c *Client) deviceInTree(ctx context.Context, id string) (bool, error) {
	ids, err := c.DeviceIDs(ctx)
	if err != nil {
		return false, err
	}
	for _, got := range ids {
		if got == id {
			return true, nil
		}
	}
	return false, nil
}

// vfioGroupFDsHeld counts VFIO group fds open by the VMM. An error means the fds
// could not be observed, which callers must treat as "not verified".
func vfioGroupFDsHeld(pid int) (int, error) {
	dir := filepath.Join(procFDDir, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", dir, err)
	}
	n := 0
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // fd closed between listing and reading; not our device
		}
		if vfioGroupFD.MatchString(target) {
			n++
		}
	}
	return n, nil
}
```

- [ ] **Step 4: Run the tests, verify they pass**

Run: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=mod golang:1.26 go test ./cmd/ateom-microvm/internal/ch/ -run TestWaitDeviceRemoved -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/ateom-microvm/internal/ch/eject.go cmd/ateom-microvm/internal/ch/eject_test.go
git commit -m "feat(ch): completion oracle for VFIO device ejection

remove-device's 204 means 'requested', so verify by observed state: the id must
leave the device tree AND the vfio GROUP fd must leave the VMM's /proc/<pid>/fd.
Matching only /dev/vfio/<N> is deliberate -- CH retains /dev/vfio/vfio and
anon_inode:kvm-vfio after an eject and neither pins the device. An unreadable fd
directory fails rather than passing: a check that cannot tell 'absent' from
'could not look' is not a check."
```

---

### Task 3: guest-side detach and attach-verify

**Files:**
- Create: `cmd/ateom-microvm/internal/kata/gpuguest.go`
- Create: `cmd/ateom-microvm/internal/kata/gpuguest_test.go`

**Interfaces:**
- Consumes: `kata.DebugConsoleDump(ctx context.Context, vsockPath, cmd string) string` (`internal/kata/agentclient.go:44`) — runs a shell command in the guest over vsock and returns its combined output.
- Produces:
  - `func GuestGPUBDFs(ctx context.Context, vsockPath string) ([]string, error)`
  - `func GuestDetachGPU(ctx context.Context, vsockPath, bdf string) error`
  - `func GuestVerifyGPUBound(ctx context.Context, vsockPath, bdf string, deadline time.Duration) error`
- Produces (test seam): `var guestExec = DebugConsoleDump`

- [ ] **Step 1: Write the failing test** (`cmd/ateom-microvm/internal/kata/gpuguest_test.go`)

```go
//go:build linux

// <Apache header copied from agentclient.go>

package kata

import (
	"context"
	"strings"
	"testing"
	"time"
)

// stubGuest replaces guestExec with a scripted responder and records commands.
func stubGuest(t *testing.T, reply func(cmd string) string) *[]string {
	t.Helper()
	var seen []string
	orig := guestExec
	guestExec = func(ctx context.Context, vsockPath, cmd string) string {
		seen = append(seen, cmd)
		return reply(cmd)
	}
	t.Cleanup(func() { guestExec = orig })
	return &seen
}

func TestGuestGPUBDFsParsesLspci(t *testing.T) {
	stubGuest(t, func(cmd string) string {
		return "0000:00:02.0\n"
	})
	got, err := GuestGPUBDFs(context.Background(), "/tmp/vsock")
	if err != nil {
		t.Fatalf("GuestGPUBDFs: %v", err)
	}
	if len(got) != 1 || got[0] != "0000:00:02.0" {
		t.Fatalf("got %v, want [0000:00:02.0]", got)
	}
}

func TestGuestGPUBDFsNormalisesShortForm(t *testing.T) {
	// lspci prints 00:02.0 without the domain; sysfs paths need 0000:00:02.0.
	stubGuest(t, func(cmd string) string { return "00:02.0\n" })
	got, _ := GuestGPUBDFs(context.Background(), "/tmp/vsock")
	if len(got) != 1 || got[0] != "0000:00:02.0" {
		t.Fatalf("got %v, want [0000:00:02.0]", got)
	}
}

func TestGuestDetachStopsPersistencedThenUnbinds(t *testing.T) {
	seen := stubGuest(t, func(cmd string) string {
		if strings.Contains(cmd, "test -e") {
			return "absent\n" // driver symlink gone => unbound
		}
		return ""
	})
	if err := GuestDetachGPU(context.Background(), "/tmp/vsock", "0000:00:02.0"); err != nil {
		t.Fatalf("GuestDetachGPU: %v", err)
	}
	all := strings.Join(*seen, "\n")
	if !strings.Contains(all, "nvidia-persistenced") {
		t.Error("must stop nvidia-persistenced before unbinding")
	}
	if !strings.Contains(all, "unbind") {
		t.Error("must write to the driver unbind file")
	}
	pi := strings.Index(all, "nvidia-persistenced")
	ui := strings.Index(all, "unbind")
	if pi > ui {
		t.Error("persistenced must be stopped BEFORE the unbind")
	}
}

// The NVIDIA driver's remove path returns EIO to the sysfs write even when the
// unbind succeeded, so success is the ABSENCE of the driver symlink, never the
// exit status.
func TestGuestDetachIgnoresEIOAndChecksSymlink(t *testing.T) {
	stubGuest(t, func(cmd string) string {
		if strings.Contains(cmd, "test -e") {
			return "absent\n"
		}
		return "sh: echo: I/O error\n"
	})
	if err := GuestDetachGPU(context.Background(), "/tmp/vsock", "0000:00:02.0"); err != nil {
		t.Fatalf("EIO on the unbind write must not fail the detach: %v", err)
	}
}

func TestGuestDetachFailsIfStillBound(t *testing.T) {
	stubGuest(t, func(cmd string) string {
		if strings.Contains(cmd, "test -e") {
			return "present\n"
		}
		return ""
	})
	if err := GuestDetachGPU(context.Background(), "/tmp/vsock", "0000:00:02.0"); err == nil {
		t.Fatal("expected an error while the driver symlink is still present")
	}
}

func TestGuestVerifyBoundSucceedsWhenSymlinkAppears(t *testing.T) {
	stubGuest(t, func(cmd string) string { return "present\n" })
	if err := GuestVerifyGPUBound(context.Background(), "/tmp/vsock", "0000:00:02.0", time.Second); err != nil {
		t.Fatalf("GuestVerifyGPUBound: %v", err)
	}
}

func TestGuestVerifyBoundBindsExplicitlyIfNeeded(t *testing.T) {
	calls := 0
	seen := stubGuest(t, func(cmd string) string {
		if strings.Contains(cmd, "test -e") {
			calls++
			if calls == 1 {
				return "absent\n" // not auto-bound on the first look
			}
			return "present\n" // bound after we asked explicitly
		}
		return ""
	})
	if err := GuestVerifyGPUBound(context.Background(), "/tmp/vsock", "0000:00:02.0", 3*time.Second); err != nil {
		t.Fatalf("GuestVerifyGPUBound: %v", err)
	}
	if !strings.Contains(strings.Join(*seen, "\n"), "drivers/nvidia/bind") {
		t.Error("expected an explicit bind after the driver did not auto-bind")
	}
}
```

- [ ] **Step 2: Run the tests, verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=mod golang:1.26 go test ./cmd/ateom-microvm/internal/kata/ -run 'TestGuest' -v`
Expected: FAIL — `guestExec`, `GuestGPUBDFs`, `GuestDetachGPU`, `GuestVerifyGPUBound` undefined.

- [ ] **Step 3: Create** `cmd/ateom-microvm/internal/kata/gpuguest.go`

```go
//go:build linux

// <Apache header copied from agentclient.go>

package kata

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// guestExec runs a shell command inside the guest. A var so tests can stub it.
var guestExec = DebugConsoleDump

// GuestGPUBDFs lists the NVIDIA PCI addresses the guest can see, normalised to
// the full domain form sysfs uses (lspci omits the domain).
func GuestGPUBDFs(ctx context.Context, vsockPath string) ([]string, error) {
	out := guestExec(ctx, vsockPath, `lspci -d 10de: | awk '{print $1}'`)
	var bdfs []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.TrimSpace(line)
		if f == "" || strings.Contains(f, " ") {
			continue
		}
		if strings.Count(f, ":") == 1 { // 00:02.0 -> 0000:00:02.0
			f = "0000:" + f
		}
		if strings.Count(f, ":") == 2 {
			bdfs = append(bdfs, f)
		}
	}
	if len(bdfs) == 0 {
		return nil, fmt.Errorf("no NVIDIA devices visible in the guest: %s", strings.TrimSpace(out))
	}
	return bdfs, nil
}

// GuestDetachGPU releases the GPU inside the guest so the VMM can eject it.
//
// Two things make this subtle:
//   - nvidia-persistenced holds the device open even with no workload running,
//     and it is the only systematic holder; with it stopped an idle actor has
//     none, so the unbind returns immediately instead of spinning on a non-zero
//     usage count.
//   - the NVIDIA driver's remove path returns EIO to the sysfs write AFTER
//     device_release_driver() has completed, so the write's exit status is not a
//     success signal. The absence of /sys/bus/pci/devices/<bdf>/driver is.
func GuestDetachGPU(ctx context.Context, vsockPath, bdf string) error {
	guestExec(ctx, vsockPath, `pkill nvidia-persistenced 2>/dev/null; sleep 1`)
	guestExec(ctx, vsockPath, fmt.Sprintf(`echo %s > /sys/bus/pci/devices/%s/driver/unbind 2>/dev/null`, bdf, bdf))
	if bound, err := guestDriverBound(ctx, vsockPath, bdf); err != nil {
		return err
	} else if bound {
		return fmt.Errorf("guest GPU %s is still bound to a driver after unbind", bdf)
	}
	return nil
}

// GuestVerifyGPUBound waits for the driver to claim a freshly attached GPU. The
// PCI core normally auto-binds it against the resident nvidia driver, but that
// is an inference rather than a guarantee, so poll and bind explicitly if it has
// not happened.
func GuestVerifyGPUBound(ctx context.Context, vsockPath, bdf string, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	askedExplicitly := false
	for {
		bound, err := guestDriverBound(ctx, vsockPath, bdf)
		if err != nil {
			return err
		}
		if bound {
			return nil
		}
		if !askedExplicitly {
			guestExec(ctx, vsockPath, fmt.Sprintf(`echo %s > /sys/bus/pci/drivers/nvidia/bind 2>/dev/null`, bdf))
			askedExplicitly = true
		}
		if time.Now().After(end) {
			return fmt.Errorf("guest GPU %s did not bind to the nvidia driver within %s", bdf, deadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// guestDriverBound reports whether a driver currently owns the device. Checking
// the sysfs symlink is deliberate: `lspci -k` prints BOTH a "Kernel modules:"
// candidate list (always present) and a "Kernel driver in use:" state line, and
// conflating them is easy. The symlink exists if and only if a driver is bound.
func guestDriverBound(ctx context.Context, vsockPath, bdf string) (bool, error) {
	out := guestExec(ctx, vsockPath,
		fmt.Sprintf(`test -e /sys/bus/pci/devices/%s/driver && echo present || echo absent`, bdf))
	switch {
	case strings.Contains(out, "present"):
		return true, nil
	case strings.Contains(out, "absent"):
		return false, nil
	default:
		return false, fmt.Errorf("could not determine driver state for %s in the guest: %s", bdf, strings.TrimSpace(out))
	}
}
```

- [ ] **Step 4: Run the tests, verify they pass**

Run: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=mod golang:1.26 go test ./cmd/ateom-microvm/internal/kata/ -run 'TestGuest' -v`
Expected: PASS (7 tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/ateom-microvm/internal/kata/gpuguest.go cmd/ateom-microvm/internal/kata/gpuguest_test.go
git commit -m "feat(kata): guest-side GPU detach and bind verification

Stops nvidia-persistenced (the only systematic holder; without it an idle actor
has none) then unbinds. The driver returns EIO to the unbind write even on
success, so the result is judged by the ABSENCE of
/sys/bus/pci/devices/<bdf>/driver -- never by exit status, and never by parsing
lspci, whose 'Kernel modules:' candidate list is easy to mistake for the
'Kernel driver in use:' state line."
```

---

### Task 4: detach before snapshot, attach after restore

**Files:**
- Create: `cmd/ateom-microvm/gpudetach.go`
- Modify: `cmd/ateom-microvm/run.go` (add `passthroughIDs` to `runningActor`, set it after `CreateVM`)
- Modify: `cmd/ateom-microvm/checkpoint.go` (call the detach before `Pause`)
- Modify: `cmd/ateom-microvm/restore.go` (call the attach after `Resume`)
- Create: `cmd/ateom-microvm/gpudetach_test.go`

**Interfaces:**
- Consumes: `resolveWorkerDevices() ([]ch.DeviceConfig, error)` (`devices.go`); `ch.Client.AddDevice/RemoveDevice/WaitDeviceRemoved` (Tasks 1–2); `kata.GuestGPUBDFs/GuestDetachGPU/GuestVerifyGPUBound` (Task 3); `kata.VsockSocketPath(id string) string`.
- Produces:
  - `func detachGPUsForSnapshot(ctx context.Context, client *ch.Client, ra *runningActor, actorUID string) error`
  - `func attachGPUsAfterRestore(ctx context.Context, client *ch.Client, ra *runningActor, actorUID string) error`
  - `runningActor.passthroughIDs []string`

- [ ] **Step 1: Write the failing test** (`cmd/ateom-microvm/gpudetach_test.go`)

```go
//go:build linux

// <Apache header copied from gpu.go>

package main

import "testing"

// The detach path must be a no-op on a worker with no GPU: ordinary actors must
// not pay for, or be broken by, GPU sequencing.
func TestDetachIsNoOpWithoutGPUs(t *testing.T) {
	// no PCI_RESOURCE_NVIDIA_COM_* in the environment
	if gpuWorkerHasGPUs(t) {
		t.Skip("test environment unexpectedly advertises a GPU")
	}
	ra := &runningActor{}
	if err := detachGPUsForSnapshot(t.Context(), nil, ra, "uid"); err != nil {
		t.Fatalf("detach on a non-GPU worker must be a no-op, got %v", err)
	}
	if err := attachGPUsAfterRestore(t.Context(), nil, ra, "uid"); err != nil {
		t.Fatalf("attach on a non-GPU worker must be a no-op, got %v", err)
	}
}

func gpuWorkerHasGPUs(t *testing.T) bool {
	t.Helper()
	devs, err := resolveWorkerDevices()
	if err != nil {
		return false
	}
	return len(devs) > 0
}

// A GPU worker must never reach Pause/Snapshot with devices still attached.
func TestDetachRequiresDeviceIDsWhenGPUsPresent(t *testing.T) {
	t.Setenv("PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4", "0000:da:00.0")
	ra := &runningActor{} // no recorded device ids -> we cannot eject
	if err := detachGPUsForSnapshot(t.Context(), nil, ra, "uid"); err == nil {
		t.Fatal("expected an error when the worker has GPUs but no device ids were recorded")
	}
}
```

- [ ] **Step 2: Run the tests, verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=mod golang:1.26 go test ./cmd/ateom-microvm/ -run 'TestDetach' -v`
Expected: FAIL — `detachGPUsForSnapshot`, `attachGPUsAfterRestore`, `runningActor.passthroughIDs` undefined.

- [ ] **Step 3: Add the field to `runningActor`** in `cmd/ateom-microvm/run.go`, after `apiSocket`

```go
	// passthroughIDs are the cloud-hypervisor device ids of the VFIO GPUs attached
	// to this VM. CH assigns them and they CHANGE across a detach/re-attach cycle
	// (_vfio0 -> _vfio1), so they are recorded rather than reconstructed: they
	// are the handles required to eject the devices before a snapshot.
	passthroughIDs []string
```

- [ ] **Step 4: Record the ids after `CreateVM`** in `cmd/ateom-microvm/run.go`, immediately after the existing `client.CreateVM(ctx, vmCfg)` error check

```go
	// Cold-plugged GPUs get their ids from the device tree: unlike hot-plug,
	// vm.create does not report them.
	if len(gpuDevices) > 0 {
		ids, err := client.DeviceIDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("while listing VM devices: %w", err)
		}
		var gpuIDs []string
		for _, id := range ids {
			if strings.HasPrefix(id, "_vfio") {
				gpuIDs = append(gpuIDs, id)
			}
		}
		if len(gpuIDs) != len(gpuDevices) {
			return nil, fmt.Errorf("expected %d VFIO device ids after vm.create, found %d %v",
				len(gpuDevices), len(gpuIDs), gpuIDs)
		}
		passthroughIDs = gpuIDs
	}
```

Declare `var passthroughIDs []string` next to the existing `gpuDevices`, and add `passthroughIDs: passthroughIDs` to the `ra := &runningActor{...}` literal. Add `"strings"` to the imports if absent.

- [ ] **Step 5: Create** `cmd/ateom-microvm/gpudetach.go`

```go
//go:build linux

// <Apache header copied from gpu.go>

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
)

const (
	// gpuEjectDeadline bounds how long we wait for the guest to execute ACPI
	// _EJ0 and for CH to release the VFIO group.
	gpuEjectDeadline = 30 * time.Second
	// gpuBindDeadline bounds how long we wait for the guest driver to claim a
	// re-attached GPU.
	gpuBindDeadline = 30 * time.Second
)

// detachGPUsForSnapshot releases every GPU attached to this actor's VM so the
// guest memory image can be captured safely.
//
// This is required for correctness, not tidiness: cloud-hypervisor does not
// refuse to snapshot a VM holding a VFIO device. VfioPciDevice's Pausable impl
// is empty, so vm.pause never quiesces a bus-mastering GPU -- it keeps DMA-ing
// into guest RAM while the memory ranges are written out, yielding a TORN image
// rather than merely one missing device state.
//
// Any failure here aborts the suspend and leaves the actor running: a partially
// detached actor must never be snapshotted.
func detachGPUsForSnapshot(ctx context.Context, client *ch.Client, ra *runningActor, actorUID string) error {
	devs, err := resolveWorkerDevices()
	if err != nil {
		return fmt.Errorf("while checking for GPUs: %w", err)
	}
	if len(devs) == 0 {
		return nil // ordinary worker: nothing to do
	}
	if ra == nil || len(ra.passthroughIDs) == 0 {
		return fmt.Errorf("worker has %d GPU(s) but no device ids were recorded; cannot eject", len(devs))
	}

	vsock := kata.VsockSocketPath(actorUID)
	bdfs, err := kata.GuestGPUBDFs(ctx, vsock)
	if err != nil {
		return fmt.Errorf("while listing guest GPUs: %w", err)
	}
	for _, bdf := range bdfs {
		if err := kata.GuestDetachGPU(ctx, vsock, bdf); err != nil {
			return fmt.Errorf("while releasing guest GPU %s: %w", bdf, err)
		}
	}

	// Request every eject BEFORE waiting on any of them. WaitDeviceRemoved's fd
	// check counts ALL vfio group fds the VMM holds, not just one device's, so on
	// a multi-GPU actor a per-device wait inside this loop could never reach zero
	// and would time out with the other GPUs still legitimately attached.
	for _, id := range ra.passthroughIDs {
		if err := client.RemoveDevice(ctx, id); err != nil {
			return fmt.Errorf("while requesting eject of %s: %w", id, err)
		}
	}
	for _, id := range ra.passthroughIDs {
		// A 204 only means "requested"; confirm the VMM actually let go.
		if err := client.WaitDeviceRemoved(ctx, id, vmmPID(ra), gpuEjectDeadline); err != nil {
			return fmt.Errorf("while confirming eject of %s: %w", id, err)
		}
	}
	slog.InfoContext(ctx, "Detached GPUs for snapshot",
		slog.Int("count", len(ra.passthroughIDs)), slog.String("id", actorUID))
	ra.passthroughIDs = nil
	return nil
}

// attachGPUsAfterRestore re-attaches this worker's GPUs to a freshly restored VM
// and waits for the guest driver to claim them. The snapshot was taken with no
// device present, so a restored VM always comes back GPU-less: the attach is
// explicit, never automatic.
func attachGPUsAfterRestore(ctx context.Context, client *ch.Client, ra *runningActor, actorUID string) error {
	devs, err := resolveWorkerDevices()
	if err != nil {
		return fmt.Errorf("while checking for GPUs: %w", err)
	}
	if len(devs) == 0 {
		return nil
	}
	var ids []string
	for _, d := range devs {
		added, err := client.AddDevice(ctx, d.Path)
		if err != nil {
			return fmt.Errorf("while attaching GPU %s: %w", d.Path, err)
		}
		ids = append(ids, added.ID)
	}
	if ra != nil {
		ra.passthroughIDs = ids
	}

	vsock := kata.VsockSocketPath(actorUID)
	bdfs, err := kata.GuestGPUBDFs(ctx, vsock)
	if err != nil {
		return fmt.Errorf("while listing guest GPUs after attach: %w", err)
	}
	for _, bdf := range bdfs {
		if err := kata.GuestVerifyGPUBound(ctx, vsock, bdf, gpuBindDeadline); err != nil {
			return fmt.Errorf("while binding guest GPU %s: %w", bdf, err)
		}
	}
	slog.InfoContext(ctx, "Re-attached GPUs after restore",
		slog.Int("count", len(ids)), slog.String("id", actorUID))
	return nil
}

// vmmPID returns the pid of the cloud-hypervisor process owning this actor's VM,
// used to observe whether the VFIO group fd was released.
func vmmPID(ra *runningActor) int {
	if ra == nil || ra.chCmd == nil || ra.chCmd.Process == nil {
		return 0
	}
	return ra.chCmd.Process.Pid
}
```

- [ ] **Step 6: Wire the detach into `checkpoint.go`** — replace the existing gate call so the detach runs first and the gate becomes the final assertion

```go
	if err := detachGPUsForSnapshot(ctx, client, ra, actorUID); err != nil {
		return nil, fmt.Errorf("while detaching GPUs before snapshot: %w", err)
	}
	// Final assertion: after a successful detach the worker's GPUs are gone from
	// this VM, so this must now pass. If it fires, the detach above is broken.
	if err := errIfPassthroughSnapshot(); err != nil {
		return nil, err
	}
```

Note `errIfPassthroughSnapshot` reads the *worker's* allocation, so it still reports GPUs after a successful detach. Change it to take the actor's remaining attachment instead:

```go
func errIfPassthroughSnapshot(ra *runningActor) error {
	if ra != nil && len(ra.passthroughIDs) > 0 {
		return status.Errorf(codes.FailedPrecondition,
			"cannot snapshot: %d GPU(s) still attached to this VM", len(ra.passthroughIDs))
	}
	return nil
}
```

and call it as `errIfPassthroughSnapshot(ra)`. Update `checkpoint_test.go` accordingly: replace the four `errIfPassthroughSnapshot()` tests with `errIfPassthroughSnapshot(&runningActor{passthroughIDs: []string{"_vfio0"}})` (expect error) and `errIfPassthroughSnapshot(&runningActor{})` / `errIfPassthroughSnapshot(nil)` (expect nil).

- [ ] **Step 7: Wire the attach into `restore.go`** — after the existing `client.Resume(ctx)` succeeds

```go
	if err := attachGPUsAfterRestore(ctx, client, ra, actorUID); err != nil {
		return nil, fmt.Errorf("while re-attaching GPUs after restore: %w", err)
	}
```

Place it after `ra` is constructed so the new device ids are recorded on it.

- [ ] **Step 8: Build and run the whole package**

Run: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=mod golang:1.26 sh -c "go build ./cmd/ateom-microvm/... && go test ./cmd/ateom-microvm/... "`
Expected: build clean; all tests pass.

- [ ] **Step 9: Commit**

```bash
git add cmd/ateom-microvm/gpudetach.go cmd/ateom-microvm/gpudetach_test.go cmd/ateom-microvm/run.go cmd/ateom-microvm/checkpoint.go cmd/ateom-microvm/restore.go cmd/ateom-microvm/checkpoint_test.go
git commit -m "feat(ateom-microvm): detach GPUs before snapshot, re-attach after restore

CheckpointWorkload now releases the GPU in the guest, ejects it from the VM and
confirms the VFIO group fd was freed before pausing; RestoreWorkload re-attaches
and waits for the driver to rebind. The golden snapshot needs no special case --
it is an ordinary suspend. errIfPassthroughSnapshot now keys off the actor's remaining
attachments so it asserts the detach worked, rather than the worker's static
allocation."
```

---

### Task 5: end-to-end runbook on the T4 host

**Files:**
- Create: `docs/superpowers/runbooks/microvm-gpu-suspend-resume-e2e.md`

- [ ] **Step 1: Write the runbook**

Document, as ordered shell steps for host `s2029gp-tr-0139`: deploy the rebuilt `ateom-microvm`; create a GPU `ActorTemplate` from the fixture added in this task (`demos/gpu/gpu-microvm.yaml.tmpl` — SandboxConfig pinning the NVIDIA Kata guest assets + a WorkerPool requesting the model-specific `nvidia.com/<PCI_MODEL>` resource + an ActorTemplate whose container is a **CUDA base image**, so `nvidia-smi` and the driver userspace are present; a workload that never opens the device would pass this runbook while the GPU is unusable); wait for the template to reach `Ready` (this exercises the golden snapshot, which is now an ordinary suspend); create an actor; `nvidia-smi` inside it; suspend it; confirm the worker is freed; resume it; `nvidia-smi` again.

Record expected output for each step, and these three failure signatures with their meaning:
- `device ... still in the device tree` → the guest never executed `_EJ0`; check the guest console.
- `VMM still holds N vfio group fd(s)` → partial eject; the VM must be destroyed, not retried.
- `guest GPU ... did not bind` → the driver did not claim the re-attached device; check that the modules are still resident.

- [ ] **Step 2: Run it and paste real output**

Execute on the GPU host. The acceptance criteria are: the template reaches `Ready`, an actor runs `nvidia-smi`, and the actor survives a suspend/resume with `nvidia-smi` still working afterwards.

- [ ] **Step 3: Confirm the §4.3 inference**

From inside a container that existed *before* the suspend, verify its pre-existing `/dev/nvidia*` still work after resume. If they do not, implement spec component C5 (create the nodes into `/proc/<pid>/root/dev/` from the guest and widen the device cgroup via `UpdateContainer`, both driven by the guest CDI spec) and re-run.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/runbooks/microvm-gpu-suspend-resume-e2e.md
git commit -m "docs: end-to-end runbook for GPU actor suspend/resume"
```

---

## Self-Review

**Spec coverage.** §4.1 lifecycle → Tasks 4 (suspend/resume) and existing branch code (run/teardown). §4.2 invariants → Task 4 step 6 (gate as assertion) and the detach ordering. §4.3 no restore-time injection → Task 5 step 3 verifies the inference and names the fallback. D1/D2 → Task 4. D3 (no golden special case) → nothing to implement, which is the point. D5 → Task 2. D6 → Task 4 step 6 rewrites the gate to key off remaining attachments. D9 → Task 3. D10 → Task 3 (`GuestVerifyGPUBound`). §7 error handling → the abort-the-suspend semantics in `detachGPUsForSnapshot` and the three failure signatures in Task 5. §12 and §13 are explicitly out of scope per Global Constraints.

**Placeholder scan.** No TBDs; every code step carries complete code; the runbook task specifies exactly which outputs to record rather than saying "document it".

**Type consistency.** `AddedDevice{ID,BDF}` is produced in Task 1 and consumed in Task 4. `WaitDeviceRemoved(ctx, id, vmmPID, deadline)` produced in Task 2, called in Task 4 with `vmmPID(ra)`. `GuestGPUBDFs`/`GuestDetachGPU`/`GuestVerifyGPUBound` produced in Task 3, all called in Task 4. `runningActor.passthroughIDs` added in Task 4 step 3 and used in steps 5–7. `errIfPassthroughSnapshot` changes signature in Task 4 step 6, and that step also updates its existing tests — the only breaking change to committed code.
