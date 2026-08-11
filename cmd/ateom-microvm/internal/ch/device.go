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
	"fmt"
	"sort"
	"strings"
)

// AddedDevice is cloud-hypervisor's answer to vm.add-device. The ID is assigned
// by CH and CHANGES between attachments of the same physical device (_vfio0 ->
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
	// A 2xx carrying no id is a failed attach, not a successful one: the id is
	// the only handle for the later eject. Reporting success here would surface
	// at suspend as RemoveDevice(""), which means "do not retry, destroy the
	// VM" — so refuse now, while the caller can still act on it.
	if out.ID == "" {
		return AddedDevice{}, fmt.Errorf("vm.add-device %s: reply carried no device id", path)
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

// vfioIDPrefix is how cloud-hypervisor names VFIO passthrough devices:
// VFIO_DEVICE_NAME_PREFIX plus a counter. Devices passed at vm.create and
// devices hot-plugged later run through the same allocator, so a cold-plugged
// device carries an id of this shape even though we never saw an add-device
// reply for it.
//
// Reading the id back matters because cold-plug is not optional here: the guest
// builds its CDI spec once at boot, before the agent starts, so a device added
// afterwards would be invisible to the container. There is no add-device reply
// to remember, and this is the only handle for the later eject.
//
// The counter is GLOBAL across every auto-named device, not per-prefix, so the
// first passthrough device is not necessarily _vfio0 -- auto-named virtio
// devices are created first and consume lower numbers. Never assume an index.
const vfioIDPrefix = "_vfio"

// isVFIOID reports whether a device-tree key names a VFIO passthrough device.
//
// The digit check is load-bearing rather than tidiness: cloud-hypervisor also
// allocates ids under the prefix "_vfio_user" for vfio-user devices, which a
// plain prefix match would sweep up. Those are a different device class with a
// different teardown, and ejecting one as if it were a passthrough device would
// be wrong. Requiring the suffix to be all digits separates them exactly.
func isVFIOID(id string) bool {
	rest, ok := strings.CutPrefix(id, vfioIDPrefix)
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// VFIOPassthroughIDs returns the ids of the VFIO passthrough devices the VMM
// currently holds, filtered out of the full device tree (which also lists
// virtio-net, virtio-blk, virtio-fs, vsock and friends).
//
// It reads device_tree rather than config.devices, even though config.devices is
// by definition the passthrough list and needs no name matching. config is the
// wrong source for this question: cloud-hypervisor drops a device from config
// the moment an eject is REQUESTED, while the device is still being torn down,
// so config would report a device gone during exactly the window this exists to
// detect. device_tree only loses the entry once the guest has completed the
// eject.
//
// This is observed VMM state, not our own bookkeeping, which is what makes it
// usable as a safety assertion: it stays correct for an actor restored by a
// different ateom process, and it reports what the VMM actually still has
// rather than what we believe we detached.
func (c *Client) VFIOPassthroughIDs(ctx context.Context) ([]string, error) {
	ids, err := c.DeviceIDs(ctx)
	if err != nil {
		return nil, err
	}
	var vfio []string
	for _, id := range ids {
		if isVFIOID(id) {
			vfio = append(vfio, id)
		}
	}
	sort.Strings(vfio)
	return vfio, nil
}

// DeviceIDs returns the ids present in the VM's device tree. This is the
// authoritative view of what the VMM still has attached; VmInfo.config is
// cleared as soon as an eject is requested and will report a device as gone
// while its teardown is still in flight.
func (c *Client) DeviceIDs(ctx context.Context) ([]string, error) {
	// CH models device_tree as Option<HashMap<..>>, so it can be absent or null.
	// The pointer keeps that apart from a present-but-empty tree: an empty tree
	// proves nothing is attached, whereas a missing one proves nothing at all.
	// Collapsing the two would let a caller read a malformed vm.info as "the
	// device is gone" and clear the way to snapshot with it still attached.
	var info struct {
		DeviceTree *map[string]any `json:"device_tree"`
	}
	if err := c.api.getJSON(ctx, "/api/v1/vm.info", &info); err != nil {
		return nil, fmt.Errorf("vm.info: %w", err)
	}
	if info.DeviceTree == nil {
		return nil, fmt.Errorf("vm.info: reply carried no device_tree, so the attached devices cannot be read")
	}
	ids := make([]string, 0, len(*info.DeviceTree))
	for id := range *info.DeviceTree {
		ids = append(ids, id)
	}
	return ids, nil
}
