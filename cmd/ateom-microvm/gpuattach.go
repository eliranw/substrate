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
	"log/slog"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
)

const (
	// ejectTimeout bounds the wait for the guest to complete an ACPI eject.
	// vm.remove-device only REQUESTS the eject; the guest runs it.
	ejectTimeout = 30 * time.Second
	// bindTimeout bounds the wait for the guest driver to claim a re-attached
	// device, including the explicit bind GuestVerifyGPUBound falls back to.
	bindTimeout = 30 * time.Second
	// enumerateTimeout bounds the wait for a freshly attached device to appear on
	// the guest PCI bus at all. Hot-plug is asynchronous: vm.add-device returns as
	// soon as the VMM has wired the device up, before the guest has processed the
	// hot-plug event, so the first look can legitimately find nothing.
	enumerateTimeout = 30 * time.Second
)

// Seams for the guest and eject steps, so the ORDER this file imposes can be
// tested without a live guest or a VMM-shaped /proc. The sequencing is the part
// that carries the correctness argument -- the steps themselves are covered in
// internal/kata and internal/ch -- and it is invisible to any test that cannot
// observe the calls interleave.
var (
	guestGPUBDFs     = kata.GuestGPUBDFs
	guestDetachGPU   = kata.GuestDetachGPU
	guestVerifyBound = kata.GuestVerifyGPUBound
	waitDeviceGone   = (*ch.Client).WaitDeviceRemoved
)

// vmmPID returns the pid of the cloud-hypervisor process ateom launched for an
// actor, or 0 when it is not known (ra lost across an ateom restart).
//
// 0 is not a usable answer for the eject check: WaitDeviceRemoved confirms the
// VMM has dropped its /dev/vfio group fd, and it cannot do that without knowing
// which process to look at. Callers must treat 0 as "cannot verify".
func vmmPID(ra *runningActor) int {
	if ra == nil || ra.chCmd == nil || ra.chCmd.Process == nil {
		return 0
	}
	return ra.chCmd.Process.Pid
}

// detachPassthrough releases every passthrough device before a snapshot.
//
// Order is the whole point. The guest must let go first: cloud-hypervisor's
// VfioPciDevice has an empty Pausable impl, so nothing downstream ever quiesces
// a bus-mastering device, and ejecting one that the guest driver still owns
// leaves it free to DMA into guest RAM while the memory image is written.
//
//  1. guest — stop nvidia-persistenced and unbind the driver, so the device has
//     no owner and stops mastering.
//  2. VMM — request the eject for every device, THEN wait for all of them. The
//     requests are issued up front on purpose: waiting for each in turn would
//     serialise ejects that the guest can process concurrently.
//  3. confirm — WaitDeviceRemoved, which trusts observed state rather than the
//     204 that vm.remove-device returns (that means "requested", not "gone").
//
// Returns without doing anything when the VM holds no passthrough device, so it
// is safe to call unconditionally.
func (s *AteomService) detachPassthrough(ctx context.Context, client *ch.Client, ra *runningActor, actorUID string) error {
	ids, err := client.VFIOPassthroughIDs(ctx)
	if err != nil {
		return fmt.Errorf("while listing attached passthrough devices: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}

	pid := vmmPID(ra)
	if pid == 0 {
		// Refusing is the safe answer: the alternative is snapshotting on the
		// strength of an eject we cannot confirm, which is the torn-memory bug.
		return fmt.Errorf("cannot detach %d passthrough device(s): the cloud-hypervisor pid is unknown, so the eject cannot be confirmed", len(ids))
	}

	tDetach := time.Now()
	vsockPath := kata.VsockSocketPath(actorUID)
	bdfs, err := guestGPUBDFs(ctx, vsockPath)
	if err != nil {
		return fmt.Errorf("while listing guest GPUs: %w", err)
	}
	for _, bdf := range bdfs {
		if err := guestDetachGPU(ctx, vsockPath, bdf); err != nil {
			return fmt.Errorf("while releasing guest GPU %s: %w", bdf, err)
		}
	}

	for _, id := range ids {
		if err := client.RemoveDevice(ctx, id); err != nil {
			return fmt.Errorf("while requesting eject of %s: %w", id, err)
		}
	}
	for _, id := range ids {
		if err := waitDeviceGone(client, ctx, id, pid, ejectTimeout); err != nil {
			return fmt.Errorf("while confirming eject of %s: %w", id, err)
		}
	}

	slog.InfoContext(ctx, "Detached passthrough devices for snapshot",
		slog.String("id", actorUID), slog.Int("devices", len(ids)),
		slog.Any("guest_bdfs", bdfs), slog.Duration("took", time.Since(tDetach)))
	return nil
}

// attachPassthrough gives the actor its passthrough device(s) back after a
// restore, and does not return until the guest driver has actually claimed them.
//
// The devices come from the worker's own allocation rather than from anything in
// the snapshot: the snapshot was deliberately taken with none attached, and the
// actor may well be resuming on a different worker with different host BDFs.
//
// Verifying the bind is not ceremony. A container's /dev/nvidia* nodes survive
// the cycle in guest RAM, so they exist again the moment the memory image is
// restored — but they are just (major, minor) pairs, and they only start working
// once the driver rebinds. Returning early would hand the actor device nodes
// that fail on open.
func (s *AteomService) attachPassthrough(ctx context.Context, client *ch.Client, actorUID string) error {
	devices, err := resolveWorkerDevices()
	if err != nil {
		return fmt.Errorf("while resolving worker passthrough devices: %w", err)
	}
	if len(devices) == 0 {
		return nil
	}

	tAttach := time.Now()
	for _, d := range devices {
		if _, err := client.AddDevice(ctx, d.Path); err != nil {
			return fmt.Errorf("while attaching %s: %w", d.Path, err)
		}
	}

	// Guest BDFs are assigned by the VMM and are not the host's, so they have to
	// be read back after the attach rather than carried over.
	vsockPath := kata.VsockSocketPath(actorUID)
	bdfs, err := waitGuestGPUs(ctx, vsockPath, len(devices), enumerateTimeout)
	if err != nil {
		return err
	}
	for _, bdf := range bdfs {
		if err := guestVerifyBound(ctx, vsockPath, bdf, bindTimeout); err != nil {
			return fmt.Errorf("while waiting for guest GPU %s to bind: %w", bdf, err)
		}
	}

	slog.InfoContext(ctx, "Re-attached passthrough devices after restore",
		slog.String("id", actorUID), slog.Int("devices", len(devices)),
		slog.Any("guest_bdfs", bdfs), slog.Duration("took", time.Since(tAttach)))
	return nil
}

// waitGuestGPUs polls until the guest has enumerated want devices.
//
// Hot-plug is asynchronous — vm.add-device returns once the VMM has wired the
// device up, which is before the guest has handled the event — so an empty or
// short first read is expected rather than a failure. Waiting for the full count
// avoids binding only the devices that happened to appear first.
func waitGuestGPUs(ctx context.Context, vsockPath string, want int, deadline time.Duration) ([]string, error) {
	end := time.Now().Add(deadline)
	for {
		bdfs, err := guestGPUBDFs(ctx, vsockPath)
		// A scan error means we could not ask the guest at all; keep trying until
		// the deadline, since the agent may still be coming back after the restore.
		if err == nil && len(bdfs) >= want {
			return bdfs, nil
		}
		if time.Now().After(end) {
			if err != nil {
				return nil, fmt.Errorf("guest never reported its GPUs within %s: %w", deadline, err)
			}
			return nil, fmt.Errorf("guest enumerated %d of %d passthrough device(s) within %s", len(bdfs), want, deadline)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
