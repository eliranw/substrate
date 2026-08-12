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
)

const (
	// ejectTimeout bounds the wait for the guest to complete an ACPI eject.
	// vm.remove-device only REQUESTS the eject; the guest runs it.
	ejectTimeout = 30 * time.Second
	// attachSettleTimeout bounds the wait for a re-attached device to reappear in
	// the VMM's device tree. Hot-plug is asynchronous: vm.add-device returns once
	// the VMM has wired the device up, before the guest has processed the event.
	attachSettleTimeout = 30 * time.Second
)

// waitDeviceGone is a seam so the detach ORDER can be tested without a
// VMM-shaped /proc. The step itself is covered in internal/ch.
var waitDeviceGone = (*ch.Client).WaitDeviceRemoved

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
// The eject IS the unbind. clh's vm.remove-device raises an ACPI eject, and the
// guest kernel's hotplug path runs pci_stop_and_remove_bus_device, which calls
// the bound driver's .remove(). An earlier design had ateom stop
// nvidia-persistenced and unbind the driver over the guest debug console first;
// that was both impossible and unnecessary. Impossible because NVIDIA's guest
// rootfs has no shell at all -- only /bin/busybox, with /init pointing straight
// at NVRC -- so kata-agent's console cannot spawn one and no command can run in
// the guest. Unnecessary because the eject completes anyway, verified on a T4.
//
// Ejects are requested for every device before any is awaited, so the guest can
// process them concurrently rather than one round-trip at a time.
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
		slog.Duration("took", time.Since(tDetach)))
	return nil
}

// attachPassthrough gives the actor its passthrough device(s) back after a
// restore.
//
// The devices come from the worker's own allocation rather than from anything in
// the snapshot: the snapshot was deliberately taken with none attached, and the
// actor may well be resuming on a different worker with different host BDFs.
//
// The device reappearing in the VMM's device tree is as far as ateom can verify
// from the host. Whether the guest driver rebound, and whether the container's
// /dev/nvidia* work again, is only observable from inside the container -- the
// guest itself cannot be asked, having no shell.
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
	if err := waitDevicesAttached(ctx, client, len(devices), attachSettleTimeout); err != nil {
		return err
	}

	slog.InfoContext(ctx, "Re-attached passthrough devices after restore",
		slog.String("id", actorUID), slog.Int("devices", len(devices)),
		slog.Duration("took", time.Since(tAttach)))
	return nil
}

// waitDevicesAttached polls until the VMM's device tree holds want devices.
func waitDevicesAttached(ctx context.Context, client *ch.Client, want int, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for {
		ids, err := client.VFIOPassthroughIDs(ctx)
		if err == nil && len(ids) >= want {
			return nil
		}
		if time.Now().After(end) {
			if err != nil {
				return fmt.Errorf("could not confirm the re-attached devices within %s: %w", deadline, err)
			}
			return fmt.Errorf("only %d of %d passthrough device(s) came back within %s", len(ids), want, deadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
