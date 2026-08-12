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
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
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

// Seams so the detach ORDER can be tested without a VMM-shaped /proc or a live
// guest. The steps themselves are covered in internal/ch and internal/kata.
var (
	waitDeviceGone     = (*ch.Client).WaitDeviceRemoved
	releasePersistence = clearGPUPersistence
	verifyGuestGPU     = ensureActorGPUUsable
)

// clearGPUPersistence asks the guest to stop holding the GPU, so the eject can
// complete.
//
// NVRC starts nvidia-persistenced unconditionally, and that daemon's default is
// persistence mode ENABLED, so the guest holds the device's file descriptors
// open from boot with no workload involved. The driver's PCI remove callback
// then spins forever waiting for that refcount to drop:
//
//	// kernel-open/nvidia/nv-pci.c, "we wait for the usage count to go to zero"
//	while (atomic64_read(&nvl->usage_count) != 0) { os_delay(500); }
//
// so vm.remove-device is accepted and the eject never finishes. NVIDIA
// documents the contract for their own removal API in the same terms:
// "persistence mode counts as an attachment to the GPU thus it must be disabled
// prior to this call".
//
// nvidia-smi -pm 0 does not write a kernel flag; it RPCs the daemon over the IPC
// socket, which closes the device and drops the refcount. It has to run in a
// CONTAINER because the guest rootfs has no shell -- CDI mounts both nvidia-smi
// and the daemon's socket into every GPU container, so the container is the only
// place with the tools and the access.
//
// Best-effort by design: a workload that removed nvidia-smi from its image
// cannot be helped here, and failing the suspend now would be worse than letting
// the eject report the real state a moment later.
func clearGPUPersistence(ctx context.Context, actorUID string, containerIDs []string) error {
	if len(containerIDs) == 0 {
		return fmt.Errorf("no containers to run nvidia-smi in")
	}
	agent, err := dialAgentRetry(ctx, kata.VsockSocketPath(actorUID), 15*time.Second)
	if err != nil {
		return fmt.Errorf("dialing the agent: %w", err)
	}
	defer agent.Close()

	var lastErr error
	for _, cid := range containerIDs {
		// A distinct exec id: the container's own id belongs to its init process.
		code, err := agent.ExecProcess(ctx, cid, cid+"_pm", []string{"nvidia-smi", "-pm", "0"})
		if err != nil {
			lastErr = err
			continue
		}
		if code != 0 {
			lastErr = fmt.Errorf("nvidia-smi -pm 0 in %s exited %d", cid, code)
			continue
		}
		return nil
	}
	return lastErr
}

// actorGPUUsable asks the actor whether its GPU works, polling for up to tries
// seconds because the driver needs a moment to bring the device up after a
// probe.
//
// nvidia-smi is the oracle rather than the presence of /dev/nvidia*: those nodes
// ride through a snapshot whether or not a driver is behind them, so they are
// present exactly as often when the GPU is dead. It runs in a container because
// the guest rootfs has no shell, and CDI mounts nvidia-smi into every GPU
// container.
func actorGPUUsable(ctx context.Context, actorUID string, containerIDs []string, tries int) error {
	if len(containerIDs) == 0 {
		return fmt.Errorf("no containers to check the GPU from")
	}
	agent, err := dialAgentRetry(ctx, kata.VsockSocketPath(actorUID), 15*time.Second)
	if err != nil {
		return fmt.Errorf("dialing the agent: %w", err)
	}
	defer agent.Close()

	script := fmt.Sprintf(`i=0
while [ $i -lt %d ]; do
nvidia-smi -L >/dev/null 2>&1 && exit 0
i=$((i+1)); sleep 1
done
exit 1`, tries)

	var lastErr error
	for _, cid := range containerIDs {
		// A distinct exec id: the container's own id belongs to its init process.
		code, err := agent.ExecProcess(ctx, cid, fmt.Sprintf("%s_gpucheck%d", cid, tries), []string{"/bin/sh", "-c", script})
		if err != nil {
			lastErr = err
			continue
		}
		if code != 0 {
			lastErr = fmt.Errorf("nvidia-smi found no GPU in %s", cid)
			continue
		}
		return nil
	}
	return lastErr
}

// retriggerHotplug ejects every passthrough device and adds it straight back, so
// the guest runs its driver probe a second time.
//
// The guest's first probe, the one its own hot-plug path runs, fails: it reads
// the device 14ms after assigning its BARs and the NVIDIA open module reports
// what comes back as the GPU lacking a GSP, then gives up for good. The device
// is fine -- config space and the kernel's resource record agree at the new
// addresses, it answers reads, and a probe seconds later binds the same GPU with
// the same UUID. So the fix is to make the guest probe again, later.
//
// The eject here does not go through detachPassthrough: that path exists to make
// a snapshot safe and refuses to proceed without confirming the VMM dropped its
// /dev/vfio fd. Nothing is being snapshotted, the driver never bound so nothing
// holds the device, and the device tree is enough to know the guest saw it go.
func retriggerHotplug(ctx context.Context, client *ch.Client) error {
	devices, err := resolveWorkerDevices()
	if err != nil {
		return fmt.Errorf("while resolving worker passthrough devices: %w", err)
	}
	ids, err := client.VFIOPassthroughIDs(ctx)
	if err != nil {
		return fmt.Errorf("while listing passthrough devices: %w", err)
	}
	for _, id := range ids {
		if err := client.RemoveDevice(ctx, id); err != nil {
			return fmt.Errorf("while ejecting %s: %w", id, err)
		}
	}
	// Not waitDevicesAttached: that one returns as soon as at least want devices
	// are present, so asking it for zero is satisfied immediately.
	end := time.Now().Add(ejectTimeout)
	for {
		left, err := client.VFIOPassthroughIDs(ctx)
		if err == nil && len(left) == 0 {
			break
		}
		if time.Now().After(end) {
			return fmt.Errorf("the guest did not release %d passthrough device(s) within %s", len(left), ejectTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
	for _, d := range devices {
		if _, err := client.AddDevice(ctx, d.Path); err != nil {
			return fmt.Errorf("while re-attaching %s: %w", d.Path, err)
		}
	}
	return waitDevicesAttached(ctx, client, len(devices), attachSettleTimeout)
}

// ensureActorGPUUsable makes the actor's re-attached GPU usable, or reports why
// it could not.
//
// Checking first keeps the cost off actors that do not need it, and keeps the
// remedy honest: a re-trigger that runs unconditionally would look like it was
// doing something even on a resume that never needed it.
func ensureActorGPUUsable(ctx context.Context, client *ch.Client, actorUID string, containerIDs []string) error {
	if err := actorGPUUsable(ctx, actorUID, containerIDs, 3); err == nil {
		return nil
	}
	slog.InfoContext(ctx, "The re-attached GPU is not bound yet; re-triggering hot-plug so the guest probes again",
		slog.String("id", actorUID))
	if err := retriggerHotplug(ctx, client); err != nil {
		return fmt.Errorf("while re-triggering hot-plug: %w", err)
	}
	return actorGPUUsable(ctx, actorUID, containerIDs, 25)
}

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
func (s *AteomService) detachPassthrough(ctx context.Context, client *ch.Client, ra *runningActor, actorUID string, containerIDs []string) (detached bool, err error) {
	ids, err := client.VFIOPassthroughIDs(ctx)
	if err != nil {
		return false, fmt.Errorf("while listing attached passthrough devices: %w", err)
	}
	if len(ids) == 0 {
		return false, nil
	}

	pid := vmmPID(ra)
	if pid == 0 {
		// Refusing is the safe answer: the alternative is snapshotting on the
		// strength of an eject we cannot confirm, which is the torn-memory bug.
		return false, fmt.Errorf("cannot detach %d passthrough device(s): the cloud-hypervisor pid is unknown, so the eject cannot be confirmed", len(ids))
	}

	tDetach := time.Now()
	// Release the guest's hold BEFORE asking for the eject: the request is
	// accepted either way, and the driver then blocks in its remove callback
	// until the refcount drops, so an eject requested first simply waits.
	if err := releasePersistence(ctx, actorUID, containerIDs); err != nil {
		slog.WarnContext(ctx, "Could not clear GPU persistence before detach; the eject may not complete",
			slog.String("id", actorUID), slog.Any("err", err))
	}

	for _, id := range ids {
		if err := client.RemoveDevice(ctx, id); err != nil {
			return false, fmt.Errorf("while requesting eject of %s: %w", id, err)
		}
	}
	// Wait concurrently. The ejects were all requested above and the guest
	// processes them in parallel, so waiting in sequence would make the worst case
	// N x ejectTimeout for no reason. That matters beyond this actor:
	// CheckpointWorkload runs under a lock that deliberately serialises every RPC
	// on this ateom, so any time spent here is time other actors' runs, restores
	// and suspends are blocked.
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := waitDeviceGone(client, ctx, id, pid, ejectTimeout); err != nil {
				errs[i] = fmt.Errorf("while confirming eject of %s (the guest may still hold "+
					"the device: nvidia-persistenced keeps it open unless nvidia-smi -pm 0 "+
					"ran in a container): %w", id, err)
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return false, err
	}

	slog.InfoContext(ctx, "Detached passthrough devices for snapshot",
		slog.String("id", actorUID), slog.Int("devices", len(ids)),
		slog.Duration("took", time.Since(tDetach)))
	return true, nil
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
