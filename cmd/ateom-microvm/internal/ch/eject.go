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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// procFDDir is the /proc root used to inspect the VMM's open file descriptors.
// A var so tests can point it at a fixture.
var procFDDir = "/proc"

// vfioGroupFD matches a VFIO *group* fd, e.g. /dev/vfio/66. It deliberately does
// NOT match /dev/vfio/vfio (the container control node) or the anon_inode
// kvm-vfio entries: cloud-hypervisor retains those after an eject, and they do
// not pin the device. The group fd is the one that does.
//
// " (deleted)" is how the kernel renders a /proc/<pid>/fd target whose backing
// dentry has been unlinked — a host-side vfio-pci unbind, a device removal or
// reset, or udev churn can all do that while the VMM still holds the fd. Such an
// fd pins the device exactly as firmly, so the suffix must not be allowed to
// hide it: matching only the bare path reports success with the group fd open.
var vfioGroupFD = regexp.MustCompile(`/dev/vfio/[0-9]+( \(deleted\))?$`)

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
//   - the VFIO group fds leave the VMM's /proc/<pid>/fd.
//
// The fd check is the stronger of the two: a partial eject failure can remove
// the tree node while the group is still held. If the fds cannot be READ, that
// is a failure, not a pass — an unobservable state must never look like success.
//
// PRECONDITION: the fd check counts every VFIO group fd the VMM holds, not only
// the one behind id, so it can only reach zero when the VM's entire VFIO set is
// being ejected. With several devices attached, issue every RemoveDevice first
// and wait afterwards; waiting after each individual eject can never reach zero
// and will simply time out.
//
// vmmPID must be the cloud-hypervisor process serving this Client's api-socket.
// The whole oracle rests on it: a pid naming some other live process reads
// cleanly and holds no VFIO fds, which is precisely what a completed eject looks
// like. That is the false success seen during hardware validation, so the pid is
// verified against /proc/<pid>/cmdline rather than trusted.
//
// Known limit: this check has no positive control. It cannot distinguish "zero
// because the device was released" from "zero because we are looking for the
// wrong node" — were cloud-hypervisor to move to an iommufd backend the pinning
// fd would be /dev/iommu and this would report success while the device is live.
// Re-verify when bumping the cloud-hypervisor version.
func (c *Client) WaitDeviceRemoved(ctx context.Context, id string, vmmPID int, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	// Bound the whole wait, not just the sleeps between polls. cloud-hypervisor
	// can be heavily swapped out during reclaim and leave an api-socket request
	// outstanding indefinitely (see apiClient), and an unbounded poll would hang
	// the checkpoint path instead of failing it.
	parent := ctx
	ctx, cancel := context.WithDeadline(ctx, end)
	defer cancel()

	// observed is the last reason derived from actually seeing the VM's state;
	// lastErr is whatever went wrong most recently. They are kept apart because
	// the deadline above cancels an in-flight vm.info, and that cancellation says
	// nothing about the device. Letting it overwrite the reason would strip the
	// diagnosis from the one failure that most needs it — an eject stuck behind a
	// slow VMM, where "still holds a vfio group fd" is actionable and "context
	// deadline exceeded" is not.
	var observed, lastErr error
	timeout := func() error {
		reason := observed
		if reason == nil {
			reason = lastErr
		}
		return fmt.Errorf("device %s not ejected within %s: %w", id, deadline, reason)
	}
	for {
		inTree, err := c.deviceInTree(ctx, id)
		switch {
		case err != nil:
			lastErr = err
			// Only our own deadline produces DeadlineExceeded here, and a request
			// it cut short observed nothing.
			if !errors.Is(err, context.DeadlineExceeded) {
				observed = err
			}
		case inTree:
			observed = fmt.Errorf("device %s still in the device tree", id)
		default:
			held, err := c.vfioGroupFDsHeld(vmmPID)
			switch {
			case err != nil:
				observed = fmt.Errorf("cannot observe VMM fds: %w", err)
			case held == 0:
				return nil
			default:
				observed = fmt.Errorf("VMM still holds %d vfio group fd(s)", held)
			}
		}
		if time.Now().After(end) {
			return timeout()
		}
		select {
		case <-ctx.Done():
			// A caller-side cancellation is reported as such. Our own deadline
			// firing mid-sleep is reported as the eject failure instead, since
			// the state that blocked it is far more use than "deadline
			// exceeded".
			if err := parent.Err(); err != nil {
				return err
			}
			return timeout()
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

// vfioGroupFDsHeld counts VFIO group fds open by the VMM. An error means the
// count is not trustworthy, which callers must treat as "not verified" rather
// than as zero.
func (c *Client) vfioGroupFDsHeld(pid int) (int, error) {
	if err := c.assertIsVMM(pid); err != nil {
		return 0, err
	}
	dir := filepath.Join(procFDDir, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", dir, err)
	}
	n := 0
	for _, e := range entries {
		link := filepath.Join(dir, e.Name())
		target, err := os.Readlink(link)
		// A vanished entry is genuinely absent: the fd was closed between
		// listing the directory and reading it, so it pins nothing. Any other
		// failure leaves this fd's target unknown, and an unknown fd could be
		// the group node that pins the device — so it must not be skipped.
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("reading %s: %w", link, err)
		}
		if vfioGroupFD.MatchString(target) {
			n++
		}
	}
	return n, nil
}

// assertIsVMM fails unless pid is the cloud-hypervisor serving this Client's
// api-socket. The VMM is exec'd as `cloud-hypervisor --api-socket <path>` (see
// LaunchVMM), so the socket path appears verbatim as one of the NUL-separated
// argv entries in /proc/<pid>/cmdline.
//
// Without this, a stale or miscomputed pid that happens to name a live process
// yields a clean, empty and entirely meaningless fd scan that reads as a
// completed eject.
func (c *Client) assertIsVMM(pid int) error {
	// An empty socket would make the match below vacuously true, which is the
	// one answer this check must never give.
	if c.apiSocket == "" {
		return fmt.Errorf("client has no api-socket, so pid %d cannot be confirmed to be its VMM", pid)
	}
	path := filepath.Join(procFDDir, strconv.Itoa(pid), "cmdline")
	argv, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if !strings.Contains(string(argv), c.apiSocket) {
		return fmt.Errorf("pid %d does not serve api-socket %s, so its open fds say nothing about this VM", pid, c.apiSocket)
	}
	return nil
}
