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
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveUnix starts an HTTP server on a unix socket and returns a Client for it.
//
// The directory comes from os.MkdirTemp, not t.TempDir: t.TempDir embeds the
// test's name in the path, and under darwin's long $TMPDIR a descriptively
// named test pushes the socket past the 104-byte sun_path cap, so Listen fails
// with EINVAL for that test alone.
func serveUnix(t *testing.T, h http.Handler) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "ch.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.RemoveAll(dir) })
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

func TestAddDeviceRejectionIsAnError(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`Error from device manager: no free PCI slot`))
	}))
	_, err := c.AddDevice(context.Background(), "/sys/bus/pci/devices/0000:3b:00.0/")
	if err == nil {
		t.Fatal("expected an error when cloud-hypervisor refuses the device")
	}
	if !strings.Contains(err.Error(), "no free PCI slot") {
		t.Errorf("error %q drops the reason cloud-hypervisor gave", err)
	}
}

// A 2xx with no id is the dangerous shape: without this guard AddDevice reports
// success, and the missing handle only bites at eject time.
func TestAddDeviceWithoutAnIDIsAnError(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty body", ``},
		{"null body", `null`},
		{"no id field", `{"bdf":"0000:00:02.0"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			if _, err := c.AddDevice(context.Background(), "/sys/bus/pci/devices/0000:3b:00.0/"); err == nil {
				t.Fatal("expected an error when the reply carries no device id")
			}
		})
	}
}

func TestRemoveDeviceSendsID(t *testing.T) {
	var gotID string
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/vm.remove-device" {
			t.Errorf("path = %q", r.URL.Path)
		}
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

// "nothing attached" and "could not look" must not answer the same, or a caller
// reads a malformed vm.info as proof the device is gone and snapshots with it live.
func TestDeviceIDsRequiresADeviceTree(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"absent", `{"state":"Running"}`},
		{"null", `{"state":"Running","device_tree":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			if _, err := c.DeviceIDs(context.Background()); err == nil {
				t.Fatal("expected an error: a reply with no device_tree says nothing about attached devices")
			}
		})
	}
}

func TestDeviceIDsEmptyTreeIsNotAnError(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"Running","device_tree":{}}`))
	}))
	ids, err := c.DeviceIDs(context.Background())
	if err != nil {
		t.Fatalf("DeviceIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want none", ids)
	}
}

// The device tree lists every device the VM has, so the passthrough ones have to
// be picked out of the virtio noise. Cold-plugged devices are named by the same
// allocator as hot-plugged ones, which is what makes reading them back possible
// at all -- we never see an add-device reply for a device passed at vm.create.
func TestVFIOPassthroughIDsFiltersTheDeviceTree(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// _vfio_user0 is a DIFFERENT device class that shares the prefix, and the
		// ids are not _vfio0/_vfio1: the name counter is global across every
		// auto-named device, so virtio devices consume the low numbers first.
		_, _ = w.Write([]byte(`{"device_tree":{"_net0":{},"_fs1":{},"_vfio3":{},"_vfio2":{},"_vfio_user0":{},"__rng":{}}}`))
	}))
	got, err := c.VFIOPassthroughIDs(context.Background())
	if err != nil {
		t.Fatalf("VFIOPassthroughIDs: %v", err)
	}
	if len(got) != 2 || got[0] != "_vfio2" || got[1] != "_vfio3" {
		t.Fatalf("got %v, want [_vfio2 _vfio3] (sorted; virtio and vfio-user excluded)", got)
	}
}

// vfio-user shares the "_vfio" prefix but is a different device class with a
// different teardown. Ejecting one as if it were a passthrough device would be
// wrong, and counting one would wedge the snapshot gate closed forever.
func TestVFIOPassthroughIDsExcludesVfioUser(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_tree":{"_vfio_user0":{},"_vfio_user12":{}}}`))
	}))
	got, err := c.VFIOPassthroughIDs(context.Background())
	if err != nil {
		t.Fatalf("VFIOPassthroughIDs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want none -- vfio-user is not VFIO passthrough", got)
	}
}

func TestVFIOPassthroughIDsEmptyWhenNoneAttached(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_tree":{"_net0":{},"_fs1":{},"__rng":{}}}`))
	}))
	got, err := c.VFIOPassthroughIDs(context.Background())
	if err != nil {
		t.Fatalf("VFIOPassthroughIDs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

// An unreadable tree must propagate as an error, never as "none attached" --
// the snapshot gate reads this to decide it is safe to freeze the guest.
func TestVFIOPassthroughIDsPropagatesAnUnreadableTree(t *testing.T) {
	c := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{}}`))
	}))
	if _, err := c.VFIOPassthroughIDs(context.Background()); err == nil {
		t.Fatal("a missing device_tree must be an error, not an empty list")
	}
}
