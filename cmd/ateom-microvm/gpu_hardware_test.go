//go:build linux && gpuhw

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

// Drives the real GPU suspend/resume cycle against real hardware: a real
// cloud-hypervisor, a real VFIO-bound GPU, and the real NVIDIA guest. Nothing
// here is stubbed, and every function it calls is the one production uses.
//
// It deliberately stops below the container layer. Running it needs no
// Kubernetes, no atelet, no ateapi and no OCI bundles -- only the staged guest
// assets and a GPU bound to vfio-pci -- which is what makes it the cheapest way
// to find out whether the VM-level design actually holds. What it therefore does
// NOT cover is the container half: CDI injection and whether a container's
// /dev/nvidia* survive the cycle. Those need the full actor path (see
// docs/dev/microvm-gpu-e2e.md).
//
// Build-tagged so it never runs in CI or on a developer laptop:
//
//	sudo -E env "PATH=$PATH" \
//	  PCI_RESOURCE_NVIDIA_COM_<MODEL>=<BDF> \
//	  go test -tags gpuhw -run TestGPUCycleOnHardware -v -timeout 20m ./cmd/ateom-microvm/
//
// The asset paths default to what the assemble scripts produce for THIS host's
// architecture; ATE_GPU_ASSETS / ATE_CH_BIN / ATE_VIRTIOFSD_BIN override them.
// Note the two scripts disagree on both default arch and layout -- assemble.sh
// defaults to arm64 and writes bin/microvm-assets/$ARCH, assemble-gpu.sh
// defaults to amd64 and writes bin/microvm-gpu-assets -- so on an amd64 host
// assemble.sh must be run as ARCH=amd64 or it silently stages binaries that
// cannot execute here.
//
// Root is required: cloud-hypervisor needs the /dev/vfio group node, and the
// eject check reads /proc/<vmm pid>/fd.

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
)

// hwEnv collects the host paths the run needs, failing with one message that
// names everything missing rather than one per lookup.
type hwEnv struct {
	assets, chBin, virtiofsd string
}

func hardwareEnv(t *testing.T) hwEnv {
	t.Helper()
	// repoRoot: this test runs from cmd/ateom-microvm.
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	// assemble.sh writes per-architecture; assemble-gpu.sh does not.
	hostAssets := filepath.Join(root, "bin", "microvm-assets", runtime.GOARCH)
	e := hwEnv{
		assets:    envOr("ATE_GPU_ASSETS", filepath.Join(root, "bin", "microvm-gpu-assets")),
		chBin:     envOr("ATE_CH_BIN", filepath.Join(hostAssets, "cloud-hypervisor")),
		virtiofsd: envOr("ATE_VIRTIOFSD_BIN", filepath.Join(hostAssets, "virtiofsd")),
	}
	for _, f := range []struct{ path, fix string }{
		{filepath.Join(e.assets, "vmlinux-gpu"), "hack/microvm-assets/assemble-gpu.sh"},
		{filepath.Join(e.assets, "rootfs-gpu.img"), "hack/microvm-assets/assemble-gpu.sh"},
		{filepath.Join(e.assets, "configuration-clh-gpu.toml"), "hack/microvm-assets/assemble-gpu.sh"},
		{e.chBin, "ARCH=" + runtime.GOARCH + " hack/microvm-assets/assemble.sh"},
		{e.virtiofsd, "ARCH=" + runtime.GOARCH + " hack/microvm-assets/assemble.sh"},
	} {
		if _, err := os.Stat(f.path); err != nil {
			t.Fatalf("missing %s\n  run: %s", f.path, f.fix)
		}
	}
	// Both scripts stage binaries for whatever ARCH they were told, and neither
	// checks it against the host. A cross-architecture binary fails deep inside
	// LaunchVMM as a bare "exec format error" with no hint about why, so run each
	// one now: the failure is the same either way, but here it can name the cause.
	for _, bin := range []string{e.chBin, e.virtiofsd} {
		if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil {
			t.Fatalf("%s will not execute on this %s host (%v): %s\n"+
				"  assemble.sh defaults to ARCH=arm64; re-run it as: ARCH=%s hack/microvm-assets/assemble.sh",
				bin, runtime.GOARCH, err, strings.TrimSpace(string(out)), runtime.GOARCH)
		} else {
			t.Logf("%s: %s", filepath.Base(bin), strings.TrimSpace(string(out)))
		}
	}
	if os.Geteuid() != 0 {
		t.Fatal("must run as root: cloud-hypervisor needs /dev/vfio and the eject check reads /proc/<pid>/fd")
	}
	return e
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestGPUCycleOnHardware boots the NVIDIA guest with a passthrough GPU, detaches
// it, snapshots, restores, re-attaches, and confirms the GPU works again.
//
// The assertions are ordered so a failure names the stage that broke rather than
// leaving a dead VM and no explanation; the guest serial log is dumped on every
// failure, because a guest that will not boot says so there and nowhere else.
func TestGPUCycleOnHardware(t *testing.T) {
	env := hardwareEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// The worker's allocation, exactly as production reads it. Set
	// PCI_RESOURCE_NVIDIA_COM_<MODEL> the way the device plugin would.
	devices, err := resolveWorkerDevices()
	if err != nil {
		t.Fatalf("resolveWorkerDevices: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("no PCI_RESOURCE_* in the environment: export the BDF the device plugin would, " +
			"e.g. PCI_RESOURCE_NVIDIA_COM_TU104GL_TESLA_T4=0000:da:00.0")
	}
	t.Logf("passthrough devices: %v", devices)

	id := "gpuhw-" + fmt.Sprint(os.Getpid())
	vmDir := kata.VMDir(id)
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", vmDir, err)
	}
	sharedDir := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatalf("mkdir shared: %v", err)
	}
	serialLog := filepath.Join(vmDir, "serial.log")
	dumpSerial := func(stage string) {
		b, _ := os.ReadFile(serialLog)
		if len(b) > 6000 {
			b = b[len(b)-6000:]
		}
		t.Logf("=== guest serial log (%s) ===\n%s", stage, b)
	}

	// --- boot -------------------------------------------------------------
	vfsd, err := kata.StartVirtiofsd(ctx, kata.VirtiofsdOptions{
		Binary: env.virtiofsd, SocketPath: kata.VirtiofsdSocketPath(id), SharedDir: sharedDir,
		Log: testWriter{t, "virtiofsd"},
	})
	if err != nil {
		t.Fatalf("StartVirtiofsd: %v", err)
	}
	t.Cleanup(func() { _ = vfsd.Process.Kill(); _, _ = vfsd.Process.Wait() })

	apiSocket := kata.CLHSocketPath(id)
	chCmd, client, err := ch.LaunchVMM(ctx, ch.LaunchVMMOptions{
		Binary: env.chBin, APISocket: apiSocket,
		Stdout: testWriter{t, "clh"}, Stderr: testWriter{t, "clh"},
	})
	if err != nil {
		t.Fatalf("LaunchVMM: %v", err)
	}
	t.Cleanup(func() { _ = chCmd.Process.Kill(); _, _ = chCmd.Process.Wait() })
	if err := client.WaitReady(ctx, 15*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	// The guest's own config decides the root parameters: NVIDIA's guest is
	// EROFS, and booting it with the stock ext4 parameters fails in the kernel
	// before anything we could log.
	cfgBytes, err := os.ReadFile(filepath.Join(env.assets, "configuration-clh-gpu.toml"))
	if err != nil {
		t.Fatalf("reading generated kata config: %v", err)
	}
	cfg, err := kata.ParseConfig(cfgBytes, 2048, 1)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	t.Logf("guest: rootfs_type=%q vcpus=%d mem=%dMiB", cfg.RootfsType, cfg.VCPUs, cfg.MemoryMiB)
	if cfg.RootfsType != "erofs" {
		t.Errorf("expected the NVIDIA guest to be erofs, got %q -- assemble-gpu.sh may be stale", cfg.RootfsType)
	}

	vmCfg := buildVMConfig(id,
		filepath.Join(env.assets, "vmlinux-gpu"), filepath.Join(env.assets, "rootfs-gpu.img"),
		kata.WithDebugConsole(cfg.KernelParams), cfg.RootfsType, serialLog,
		cfg.MemoryMiB, cfg.VCPUs, false, devices)
	t.Logf("cmdline: %s", vmCfg.Payload.Cmdline)

	if err := client.CreateVM(ctx, vmCfg); err != nil {
		t.Fatalf("CreateVM (device cold-plug rejected?): %v", err)
	}
	if err := client.BootVM(ctx); err != nil {
		dumpSerial("boot failed")
		t.Fatalf("BootVM: %v", err)
	}

	vsock := kata.VsockSocketPath(id)

	// --- claim A: did NVRC come up on a non-verity root? ------------------
	//
	// Judged at the VMM, not in the guest: this image has no /bin/sh (only
	// /bin/busybox, with /init pointing straight at NVRC), so kata-agent's debug
	// console cannot spawn a shell and no command can be run in the guest rootfs
	// at all. The guest reaching the point where it drives an ACPI eject is the
	// observable proof that it booted and bound the device.
	ids, err := client.VFIOPassthroughIDs(ctx)
	if err != nil {
		dumpSerial("could not read the device tree")
		t.Fatalf("VFIOPassthroughIDs: %v", err)
	}
	if len(ids) != len(devices) {
		dumpSerial("device count mismatch")
		t.Fatalf("VMM reports %v for %d cold-plugged device(s)", ids, len(devices))
	}
	t.Logf("PASS boot: guest booted on a non-verity erofs root; VMM holds %v", ids)

	// Best-effort only. It will fail on the NVIDIA guest for the reason above;
	// logged rather than asserted so the same test still reports it if a future
	// image ships a shell.
	if out := kata.DebugConsoleDump(ctx, vsock, "echo probe"); strings.Contains(out, "probe") {
		t.Logf("guest debug console available: %s", oneLineHW(out))
		t.Logf("guest nvidia-smi:\n%s", firstLinesHW(kata.DebugConsoleDump(ctx, vsock, "nvidia-smi 2>&1"), 12))
	} else {
		t.Logf("guest debug console unavailable (expected on this image -- no /bin/sh): %s", oneLineHW(out))
	}

	// --- detach -----------------------------------------------------------
	// No guest-side unbind: the eject IS the unbind. clh's remove-device raises an
	// ACPI eject, and the guest kernel's hotplug path runs
	// pci_stop_and_remove_bus_device, which calls the bound driver's .remove().
	// Whether that completes with the NVIDIA driver loaded -- and with
	// nvidia-persistenced possibly holding the device, which we cannot stop
	// without guest exec -- is exactly what this proves or disproves.
	for _, did := range ids {
		if err := client.RemoveDevice(ctx, did); err != nil {
			t.Fatalf("RemoveDevice(%s): %v", did, err)
		}
	}
	for _, did := range ids {
		if err := client.WaitDeviceRemoved(ctx, did, chCmd.Process.Pid, 60*time.Second); err != nil {
			t.Fatalf("WaitDeviceRemoved(%s): %v", did, err)
		}
	}
	if err := errIfPassthroughSnapshot(ctx, client); err != nil {
		t.Fatalf("gate still sees a device after a confirmed eject: %v", err)
	}
	t.Log("PASS detach: devices ejected and the eject confirmed")

	// --- snapshot / restore ----------------------------------------------
	snapDir := filepath.Join(t.TempDir(), "snapshot")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}
	if err := client.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := client.Snapshot(ctx, snapDir); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	_ = chCmd.Process.Kill()
	_, _ = chCmd.Process.Wait()
	t.Log("PASS snapshot: taken with no device attached")

	// virtiofsd serves exactly one vhost-user connection, so the one the first VMM
	// used is spent. Production restarts it on the restore path (stageOverlayLowers
	// in restoreFullScope) for the same reason; reusing it here made vm.restore
	// fail with a bare HTTP 500.
	// The snapshot's config.json names this actor's hybrid-vsock socket, and clh
	// BINDS it on restore. The first VM's socket file is still on disk, so without
	// removing it the restore dies with AddrInUse before it touches any device.
	//
	// Production does not need this: a restore runs under a NEW actor UID with
	// rewriteSnapshotSocketPaths repointing the snapshot at that actor's paths, so
	// there is nothing to collide with. Reusing one id back-to-back is what makes
	// it visible here. (LaunchVMM already clears the API socket itself.)
	if err := os.Remove(kata.VsockSocketPath(id)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing the stale vsock socket before restore: %v", err)
	}

	// virtiofsd logs "Client disconnected, shutting down" the moment the first VMM
	// goes away, so the restore needs a fresh one.
	_ = vfsd.Process.Kill()
	_, _ = vfsd.Process.Wait()
	vfsd2, err := startVirtiofsdWaitingForLock(ctx, t, env, id, sharedDir)
	if err != nil {
		t.Fatalf("restarting virtiofsd for restore: %v", err)
	}
	t.Cleanup(func() { _ = vfsd2.Process.Kill(); _, _ = vfsd2.Process.Wait() })

	chCmd2, client2, err := ch.LaunchVMM(ctx, ch.LaunchVMMOptions{
		Binary: env.chBin, APISocket: apiSocket,
		Stdout: testWriter{t, "clh2"}, Stderr: testWriter{t, "clh2"},
	})
	if err != nil {
		t.Fatalf("relaunch VMM: %v", err)
	}
	t.Cleanup(func() { _ = chCmd2.Process.Kill(); _, _ = chCmd2.Process.Wait() })
	if err := client2.WaitReady(ctx, 15*time.Second); err != nil {
		t.Fatalf("WaitReady after relaunch: %v", err)
	}
	// No net FDs: this VM has no virtio-net (production adds one separately from
	// the tap).
	//
	// Copy, not OnDemand: re-attaching a VFIO device maps the guest's memory into
	// the IOMMU, which requires pinning it, and userfaultfd-backed pages cannot be
	// pinned. OnDemand restores fine and then fails at vm.add-device with EFAULT.
	// restoreFullScope picks the same way, on the same grounds.
	if err := client2.RestoreWithNetFDs(ctx, snapDir, nil, "Copy"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := client2.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Log("PASS restore: guest resumed from a device-free snapshot")

	// --- re-attach --------------------------------------------------------
	for _, d := range devices {
		if _, err := client2.AddDevice(ctx, d.Path); err != nil {
			t.Fatalf("AddDevice(%s): %v", d.Path, err)
		}
	}

	// Judged at the VMM for the same reason as the boot check: no guest exec is
	// possible on this image. The device reappearing in the device tree proves
	// the VMM wired it back to a guest that accepted the hot-plug -- a guest that
	// rejected it would leave the tree unchanged.
	//
	// What this deliberately does NOT prove is that the guest driver re-bound and
	// that a CONTAINER can use the device again. That is design claim B, it can
	// only be observed from inside a container, and it is what the production-path
	// run in docs/dev/microvm-gpu-e2e.md exists to settle.
	reIDs, err := client2.VFIOPassthroughIDs(ctx)
	if err != nil {
		t.Fatalf("VFIOPassthroughIDs after re-attach: %v", err)
	}
	if len(reIDs) != len(devices) {
		dumpSerial("device did not come back")
		t.Fatalf("after re-attach the VMM holds %v, want %d device(s)", reIDs, len(devices))
	}
	t.Logf("PASS re-attach: VMM holds %v again", reIDs)

	if out := kata.DebugConsoleDump(ctx, vsock, "nvidia-smi 2>&1"); strings.Contains(out, "NVIDIA-SMI") {
		t.Logf("PASS guest nvidia-smi after resume:\n%s", firstLinesHW(out, 12))
	} else {
		t.Log("guest-level nvidia-smi not checkable on this image (no shell); " +
			"claim B needs the container path -- see docs/dev/microvm-gpu-e2e.md step 8")
	}
	t.Log("PASS cycle complete: boot -> eject -> snapshot -> restore -> re-attach")
}

// startVirtiofsdWaitingForLock starts virtiofsd, retrying while the previous one
// still holds its pid-file lock.
//
// virtiofsd flocks <socket-path>.pid, and the lock outlives the exiting process
// briefly, so an immediate restart fails with EAGAIN. Waiting on our own
// Process.Wait is not enough: it returns once the child is reaped, which can
// precede the kernel dropping the flock.
//
// Production never meets this. A restore follows a teardown, usually minutes
// later and often on another worker, so nothing is holding the lock. Only a test
// that snapshots and restores back-to-back in one process sees it -- so the
// retry belongs here rather than in StartVirtiofsd.
func startVirtiofsdWaitingForLock(ctx context.Context, t *testing.T, env hwEnv, id, sharedDir string) (*exec.Cmd, error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 1; ; attempt++ {
		cmd, err := kata.StartVirtiofsd(ctx, kata.VirtiofsdOptions{
			Binary: env.virtiofsd, SocketPath: kata.VirtiofsdSocketPath(id), SharedDir: sharedDir,
			Log: testWriter{t, "virtiofsd2"},
		})
		if err == nil {
			if attempt > 1 {
				t.Logf("virtiofsd started on attempt %d (waited for the old pid-file lock)", attempt)
			}
			return cmd, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("after %d attempts over 30s: %w "+
				"(check the [virtiofsd2] lines: EAGAIN on the .pid file means the previous "+
				"instance still holds its flock)", attempt, err)
		}
		time.Sleep(time.Second)
	}
}

// testWriter routes a subprocess's output into the test log, so a bare HTTP 500
// from the VMM arrives with the reason it printed alongside it.
type testWriter struct {
	t   *testing.T
	tag string
}

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("[%s] %s", w.tag, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func oneLineHW(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstLinesHW(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
