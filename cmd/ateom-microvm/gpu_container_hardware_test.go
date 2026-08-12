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

// The container half of the GPU work, which TestGPUCycleOnHardware cannot reach.
//
// Two things are only observable from inside a container: whether the CDI
// annotation actually causes the guest agent to inject /dev/nvidia* (the whole
// of gpucdi.go, which has never executed), and whether those nodes still work
// after the device is ejected and re-attached (design claim B). The guest itself
// cannot be asked -- its rootfs has no shell.
//
// Same invocation as the other hardware test:
//
//	sudo -E env "PATH=$PATH" PCI_RESOURCE_NVIDIA_COM_<MODEL>=<BDF> \
//	  go test -tags gpuhw -run TestGPUContainer -v -timeout 20m ./cmd/ateom-microvm/

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/third_party/kata/agentpb"
)

// probeSource is the container's entire userspace: a static binary that reports
// what the guest injected, then idles so it is still running if this is extended
// across a suspend/resume cycle.
//
// It prints rather than asserts. The test decides what is a failure; the probe's
// job is to make the container's view observable, including the absence of
// things.
const probeSource = `package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	fmt.Println("--- /dev ---")
	ents, err := os.ReadDir("/dev")
	if err != nil {
		fmt.Println("readdir /dev:", err)
	}
	for _, e := range ents {
		fmt.Println(" ", e.Name())
	}
	fmt.Println("--- /proc/devices ---")
	if b, err := os.ReadFile("/proc/devices"); err == nil {
		fmt.Print(string(b))
	} else {
		fmt.Println("read /proc/devices:", err)
	}
	// Opening it is the real question: a device node whose driver is not bound is
	// present but unusable, which is exactly the state a re-attach has to undo.
	for _, d := range []string{"/dev/nvidiactl", "/dev/nvidia0"} {
		f, err := os.Open(d)
		if err != nil {
			fmt.Printf("open %s: %v\n", d, err)
			continue
		}
		f.Close()
		fmt.Printf("open %s: OK\n", d)
	}
	fmt.Println("--- PROBE DONE ---")
	time.Sleep(10 * time.Minute)
}
`

// stageProbeRootfs builds the container rootfs at the path virtiofsd serves.
//
// The probe is a statically linked Go binary rather than the guest's busybox.
// That busybox is 55KB and dynamically linked against the guest's libc, so it
// exits 1 immediately in a rootfs that has no loader -- which is what a container
// rootfs assembled by hand is. CGO_ENABLED=0 removes the question entirely, and
// a CUDA image is still unnecessary: CDI supplies the driver libraries itself,
// which is the mechanism under test.
func stageProbeRootfs(t *testing.T, sharedDir, cid string) (rootfs string, probeArgs []string) {
	t.Helper()
	rootfs = filepath.Join(sharedDir, cid, "rootfs")

	// A real image rootfs is the better probe when one is available: it has a
	// libc, so it can run the nvidia-smi and driver libraries CDI mounts in, which
	// is the actual product assertion rather than a proxy for it. Stage one with
	// whatever the host has and point ATE_PROBE_ROOTFS at the unpacked directory:
	//
	//   sudo ctr -n k8s.io images pull docker.io/library/ubuntu:24.04
	//   sudo ctr -n k8s.io images mount --rw docker.io/library/ubuntu:24.04 /mnt/ubuntu
	//   export ATE_PROBE_ROOTFS=/mnt/ubuntu
	//
	// or with docker:
	//
	//   docker create --name u ubuntu:24.04 && mkdir -p /mnt/ubuntu && \
	//     docker export u | sudo tar -x -C /mnt/ubuntu
	//   export ATE_PROBE_ROOTFS=/mnt/ubuntu
	if img := os.Getenv("ATE_PROBE_ROOTFS"); img != "" {
		if err := os.MkdirAll(rootfs, 0o755); err != nil {
			t.Fatalf("mkdir rootfs: %v", err)
		}
		// Copied, not bind-mounted. The agent writes into the container root when
		// it sets it up (mount points, and the device nodes CDI asks for), and an
		// image snapshot mount is read-only -- `ctr images mount` without --rw, or
		// a squashed layer -- which surfaces as "Read-only file system (os error
		// 30)" from setup_rootfs. Production does bind its lowers, but it puts the
		// writable layer in an overlay upper on a guest tmpfs; this test has no
		// overlay, so the root itself has to be writable.
		if out, err := exec.Command("cp", "-a", img+"/.", rootfs).CombinedOutput(); err != nil {
			t.Fatalf("copying %s into the shared dir: %v: %s", img, err, out)
		}
		t.Logf("probe rootfs: %s (image)", img)
		// nvidia-smi and libcuda come from CDI's mounts, not from the image.
		// Reports once, goes QUIET, then loops.
		//
		// The quiet window exists because the eject failed with the probe polling
		// nvidia-smi every 3s: the container had opened the GPU and the ACPI eject
		// never completed. Whether that is a LIVE user or any PRIOR use is the
		// question, and a window where nothing touches the device separates them.
		// The detach runs inside it; if the eject succeeds there, a live handle is
		// the blocker and suspend needs the workload idle, not merely GPU-free.
		//
		// It loops afterwards so the same container can still answer claim B once
		// the device is back.
		quiet := envOr("ATE_PROBE_QUIET_SECS", "90")
		// Persistence mode keeps the driver initialised and the device claimed with
		// no client processes, which is why the eject blocked after first use and
		// why going quiet did not help. nvidia-smi reported Persistence-M: On.
		// Turning it off here tests exactly that: if the eject then succeeds,
		// persistence was the holder and production must clear it before detaching.
		return rootfs, []string{"/bin/sh", "-c",
			"echo '--- MARK ---'; ls /dev | grep -i nvidia | tr '\\n' ' '; echo; " +
				"echo '--- BARs ---'; cat /sys/bus/pci/devices/*/resource 2>/dev/null | head -6; " +
				"nvidia-smi -L 2>&1 | head -3; " +
				"echo '--- persistence off ---'; nvidia-smi -pm 0 2>&1 | head -3; " +
				"echo '--- PROBE DONE ---'; " +
				"sleep " + quiet + "; " +
				"while true; do echo '--- MARK ---'; ls /dev | grep -i nvidia | tr '\\n' ' '; echo; " +
				"echo '--- BARs ---'; cat /sys/bus/pci/devices/*/resource 2>/dev/null | head -6; " +
				"nvidia-smi -L 2>&1 | head -3; echo '--- PROBE DONE ---'; sleep 3; done"}
	}

	t.Log("probe rootfs: static binary (set ATE_PROBE_ROOTFS to use a real image)")
	for _, d := range []string{"dev", "proc", "sys", "etc", "tmp"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	src := filepath.Join(t.TempDir(), "probe")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir probe src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(probeSource), 0o644); err != nil {
		t.Fatalf("writing probe source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module probe\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("writing probe go.mod: %v", err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(rootfs, "probe"), ".")
	build.Dir = src
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the static probe: %v: %s", err, out)
	}
	return rootfs, []string{"/probe"}
}

// containerSpec is the OCI spec for the probe container: the same shaping
// production applies, plus the CDI annotation under test.
func containerSpec(t *testing.T, args []string) *specs.Spec {
	t.Helper()
	// kata-agent rejects a spec with no process capabilities ("missing process
	// capabilities"), which production never hits because atelet's spec carries
	// them. This is the OCI default set.
	caps := []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER",
		"CAP_MKNOD", "CAP_NET_RAW", "CAP_SETGID", "CAP_SETUID",
		"CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE",
		"CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
	}
	spec := &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			Args: args,
			Cwd:  "/",
			Env:  []string{"PATH=/bin:/usr/bin"},
			User: specs.User{UID: 0, GID: 0},
			Capabilities: &specs.LinuxCapabilities{
				Bounding: caps, Effective: caps, Permitted: caps,
			},
		},
		Hostname: "gpuprobe",
		Mounts:   defaultKataMounts(),
		Linux: &specs.Linux{
			Resources:   defaultKataResources(),
			CgroupsPath: "/ateomchv/gpuprobe",
			// The agent rejects a spec with no pid namespace ("cannot find the pid
			// ns"). specconv drops network/cgroup/time and forwards the rest with an
			// empty Path, so these are the four the guest actually sets up.
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.IPCNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.MountNamespace},
			},
		},
	}
	// The point of the test: this is what asks the guest agent to inject the GPU.
	withCDI, err := withGuestCDIDevices(spec)
	if err != nil {
		t.Fatalf("withGuestCDIDevices: %v", err)
	}
	if withCDI.Annotations[guestCDIAnnotation] == "" {
		t.Fatal("no CDI annotation was set; the worker allocation did not reach the spec")
	}
	t.Logf("container annotation: %s=%s", guestCDIAnnotation, withCDI.Annotations[guestCDIAnnotation])
	return withCDI
}

// TestGPUContainerSeesDeviceOnHardware creates a container with the CDI
// annotation and reports what it can see of the GPU.
//
// It asserts only what it can prove and logs the rest: a failure to inject is a
// hard failure, since that is gpucdi.go's entire contract, while the exact
// contents of /dev are recorded for comparison across the cycle.
func TestGPUContainerSeesDeviceOnHardware(t *testing.T) {
	env := hardwareEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	devices, err := resolveWorkerDevices()
	if err != nil {
		t.Fatalf("resolveWorkerDevices: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("no PCI_RESOURCE_* in the environment")
	}

	id := "gpuctr-" + fmt.Sprint(os.Getpid())
	const cid = "gpuprobe"
	if err := os.MkdirAll(kata.VMDir(id), 0o700); err != nil {
		t.Fatalf("mkdir vm dir: %v", err)
	}
	// virtiofsd must serve the tree the agent looks for the rootfs under.
	sharedDir := kata.SharedDir(id)
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatalf("mkdir shared: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sharedDir) })
	_, probe := stageProbeRootfs(t, sharedDir, cid)

	serialLog := filepath.Join(kata.VMDir(id), "serial.log")
	vfsd, err := kata.StartVirtiofsd(ctx, kata.VirtiofsdOptions{
		Binary: env.virtiofsd, SocketPath: kata.VirtiofsdSocketPath(id), SharedDir: sharedDir,
		Log: testWriter{t, "virtiofsd"},
	})
	if err != nil {
		t.Fatalf("StartVirtiofsd: %v", err)
	}
	t.Cleanup(func() { _ = vfsd.Process.Kill(); _, _ = vfsd.Process.Wait() })

	chCmd, client, err := ch.LaunchVMM(ctx, ch.LaunchVMMOptions{
		Binary: env.chBin, APISocket: kata.CLHSocketPath(id),
		Stdout: testWriter{t, "clh"}, Stderr: testWriter{t, "clh"},
	})
	if err != nil {
		t.Fatalf("LaunchVMM: %v", err)
	}
	t.Cleanup(func() { _ = chCmd.Process.Kill(); _, _ = chCmd.Process.Wait() })
	if err := client.WaitReady(ctx, 15*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	cfgBytes, err := os.ReadFile(filepath.Join(env.assets, "configuration-clh-gpu.toml"))
	if err != nil {
		t.Fatalf("reading kata config: %v", err)
	}
	cfg, err := kata.ParseConfig(cfgBytes, 2048, 1)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	vmCfg := buildVMConfig(id,
		filepath.Join(env.assets, "vmlinux-gpu"), filepath.Join(env.assets, "rootfs-gpu.img"),
		kata.WithDebugConsole(cfg.KernelParams), cfg.RootfsType, serialLog,
		cfg.MemoryMiB, cfg.VCPUs, false, devices)
	if err := client.CreateVM(ctx, vmCfg); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if err := client.BootVM(ctx); err != nil {
		t.Fatalf("BootVM: %v", err)
	}

	// --- drive the agent ---------------------------------------------------
	vsock := kata.VsockSocketPath(id)
	agent, err := dialAgentRetry(ctx, vsock, 60*time.Second)
	if err != nil {
		b, _ := os.ReadFile(serialLog)
		t.Fatalf("dialing the kata-agent: %v\n=== serial ===\n%s", err, tailBytes(b, 4000))
	}
	t.Cleanup(func() { _ = agent.Close() })

	if err := agent.CreateSandboxForActor(ctx, id, "gpuprobe", false); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	// Print what the container can see of the GPU, then idle so the process is
	// still alive if this is extended across a suspend/resume cycle.

	// Not CreateCarrier: that pins Root.Readonly = true, which is right for
	// production's carrier (its rootfs is the immutable overlay lower) and wrong
	// for a container the agent must populate.
	pbSpec := kata.SpecToAgentPB(containerSpec(t, probe))
	pbSpec.Root = &agentpb.Root{Path: kata.GuestSharedRootfs(cid), Readonly: false}
	if err := agent.CreateContainer(ctx, &agentpb.CreateContainerRequest{
		ContainerId: cid, ExecId: cid, OCI: pbSpec,
	}); err != nil {
		b, _ := os.ReadFile(serialLog)
		hint := "  check the serial log below for what the agent objected to"
		// Only claim CDI when the agent actually says so; the annotation is one of
		// several things CreateContainer can reject, and blaming it for a plain
		// spec-shaping error sends the reader to the wrong file.
		if strings.Contains(err.Error(), "cdi") || strings.Contains(err.Error(), "CDI") {
			hint = "  this is gpucdi.go's contract: the agent could not resolve " +
				"nvidia.com/gpu=all against the guest's /var/run/cdi spec"
		}
		t.Fatalf("CreateContainer failed: %v\n%s\n=== serial ===\n%s", err, hint, tailBytes(b, 4000))
	}
	if err := agent.StartContainer(ctx, cid); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	t.Log("PASS create: the agent accepted the CDI annotation and created the container")

	out, stderr := readProbeOutput(ctx, t, agent, cid, 60*time.Second)
	t.Logf("=== container stdout (%d bytes) ===\n%s", len(out), out)
	if stderr != "" {
		t.Logf("=== container stderr ===\n%s", stderr)
	}

	// No output at all is a DIFFERENT failure from output without device nodes,
	// and must not be reported as the latter: it means the probe never ran or its
	// streams were never readable, which says nothing about what CDI injected.
	if strings.TrimSpace(out) == "" {
		b, _ := os.ReadFile(serialLog)
		t.Fatalf("the probe container produced no output, so what CDI injected is unknown.\n"+
			"  the container was created and started, so look for an exec failure below\n"+
			"=== serial ===\n%s", tailBytes(b, 6000))
	}
	if !strings.Contains(out, "nvidia") {
		t.Fatal("the container ran but /dev has no nvidia nodes -- the annotation was " +
			"accepted and had no effect")
	}
	t.Log("PASS inject: the container can see nvidia device nodes")

	// A node that exists but will not open is the state a re-attach has to undo,
	// and it is the difference between CDI having injected something and the
	// container being able to USE the device.
	switch {
	case strings.Contains(out, "Tesla") || strings.Contains(out, "UUID"):
		t.Log("PASS use: nvidia-smi ran inside the container and saw the GPU")
	case strings.Contains(out, "open /dev/nvidiactl: OK"):
		t.Log("PASS open: the container can open the device, so the driver is bound to it")
	default:
		t.Fatal("the device nodes are present but unusable: nothing opened them and " +
			"nvidia-smi did not report a GPU. CDI injected the nodes and the guest " +
			"driver is not backing them")
	}

	// ---------------------------------------------------------------------
	// Claim B: do the container's device nodes still work after the device is
	// ejected and re-attached?
	//
	// The nodes are (major, minor) pairs in the container's /dev tmpfs. They ride
	// through the memory snapshot whether or not a driver is behind them, so
	// their presence afterwards proves nothing -- only using them does. This is
	// the design's central unobserved inference (4.3).
	// ---------------------------------------------------------------------
	if os.Getenv("ATE_SKIP_CYCLE") != "" {
		t.Log("ATE_SKIP_CYCLE set; stopping before the suspend/resume cycle")
		return
	}
	t.Log("=== cycling the device: detach -> snapshot -> restore -> re-attach ===")

	// The agent's connection dies with the VMM; production re-dials after restore.
	_ = agent.Close()

	ids, err := client.VFIOPassthroughIDs(ctx)
	if err != nil {
		t.Fatalf("VFIOPassthroughIDs: %v", err)
	}
	for _, did := range ids {
		if err := client.RemoveDevice(ctx, did); err != nil {
			t.Fatalf("RemoveDevice(%s): %v", did, err)
		}
	}
	for _, did := range ids {
		if err := client.WaitDeviceRemoved(ctx, did, chCmd.Process.Pid, 60*time.Second); err != nil {
			t.Fatalf("WaitDeviceRemoved(%s): %v\n"+
				"  The VM-only test ejects fine, so what changed is that a container has USED\n"+
				"  the GPU. If this fails even in the probe's quiet window, then any prior use\n"+
				"  blocks the eject, not just a live handle -- which means the guest must release\n"+
				"  the device before a snapshot, and that cannot be done over the debug console\n"+
				"  on this guest (no shell). Re-opens design 4.2b.", did, err)
		}
	}
	if err := errIfPassthroughSnapshot(ctx, client); err != nil {
		t.Fatalf("gate still sees a device after a confirmed eject: %v", err)
	}
	t.Log("PASS detach: device ejected while the container kept running")

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
	t.Log("PASS snapshot: taken with the container live and the device gone")

	// Same per-actor state collisions as the other hardware test: the snapshot
	// names this actor's vsock socket and clh binds it, and virtiofsd serves one
	// vhost-user connection. Production avoids both by restoring under a new id.
	if err := os.Remove(kata.VsockSocketPath(id)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing the stale vsock socket: %v", err)
	}
	_ = vfsd.Process.Kill()
	_, _ = vfsd.Process.Wait()
	vfsd2, err := startVirtiofsdWaitingForLock(ctx, t, env, id, sharedDir)
	if err != nil {
		t.Fatalf("restarting virtiofsd: %v", err)
	}
	t.Cleanup(func() { _ = vfsd2.Process.Kill(); _, _ = vfsd2.Process.Wait() })

	chCmd2, client2, err := ch.LaunchVMM(ctx, ch.LaunchVMMOptions{
		Binary: env.chBin, APISocket: kata.CLHSocketPath(id),
		Stdout: testWriter{t, "clh2"}, Stderr: testWriter{t, "clh2"},
	})
	if err != nil {
		t.Fatalf("relaunch VMM: %v", err)
	}
	t.Cleanup(func() { _ = chCmd2.Process.Kill(); _, _ = chCmd2.Process.Wait() })
	if err := client2.WaitReady(ctx, 15*time.Second); err != nil {
		t.Fatalf("WaitReady after relaunch: %v", err)
	}
	// Copy, not OnDemand: VFIO pins guest memory to map it into the IOMMU and
	// userfaultfd-backed pages cannot be pinned (restoreFullScope does the same).
	if err := client2.RestoreWithNetFDs(ctx, snapDir, nil, "Copy"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := client2.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Log("PASS restore: guest resumed with the container inside it")

	for _, d := range devices {
		if _, err := client2.AddDevice(ctx, d.Path); err != nil {
			t.Fatalf("AddDevice(%s): %v", d.Path, err)
		}
	}
	if err := waitDevicesAttached(ctx, client2, len(devices), 60*time.Second); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	t.Log("PASS re-attach: the VMM holds the device again")

	// --- the answer ------------------------------------------------------
	agent2, err := dialAgentRetry(ctx, vsock, 60*time.Second)
	if err != nil {
		t.Fatalf("re-dialing the agent after restore: %v", err)
	}
	t.Cleanup(func() { _ = agent2.Close() })

	after, afterErr := readProbeOutput(ctx, t, agent2, cid, 60*time.Second)
	t.Logf("=== container view AFTER the cycle (%d bytes) ===\n%s", len(after), after)
	if afterErr != "" {
		t.Logf("=== stderr after ===\n%s", afterErr)
	}
	if strings.TrimSpace(after) == "" {
		t.Fatal("the container produced no output after the cycle, so claim B is unresolved: " +
			"the probe loops every 3s, so silence means the process or its streams did not survive")
	}
	if strings.Contains(after, "Tesla") || strings.Contains(after, "UUID") {
		t.Log("PASS claim B: the container still uses the GPU after detach and re-attach")
		return
	}
	// Deliberately does NOT name a cause. The device nodes surviving is 4.3's
	// claim and is separately visible above; if they are present, the fault is
	// below them and C5 (re-injecting nodes) would change nothing. Compare the
	// BAR listings either side of the cycle, and check the clh log for
	// "Failed moving device BAR" -- a guest that reprogrammed the BAR to an
	// address the VMM could not allocate can see the device and not address it,
	// which reads as a device-handle error rather than a missing GPU.
	t.Errorf("CLAIM B FAILS: the container survived the cycle but can no longer use the GPU.\n"+
		"  device nodes present: %v\n"+
		"  compare the --- BARs --- blocks above, and the [clh2] lines for BAR reallocation",
		strings.Contains(after, "nvidia0"))
}

// readProbeOutput drains the container's stdout until the probe's end marker,
// and collects stderr alongside it.
//
// Both streams are reported: a container that fails to exec says so on stderr,
// and reading only stdout turns that into an indistinguishable silence.
func readProbeOutput(ctx context.Context, t *testing.T, agent *kata.AgentClient, cid string, deadline time.Duration) (string, string) {
	t.Helper()
	var out, errOut strings.Builder
	end := time.Now().Add(deadline)
	reads, empties := 0, 0

	// Each read gets its own timeout. ReadStdout BLOCKS while the container is
	// alive and has written nothing -- the earlier version returned promptly only
	// because the probe had already exited -- so an unbounded call wedges the loop
	// until the outer context expires, many minutes later.
	read := func(f func(context.Context, string, string, uint32) ([]byte, error)) []byte {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		b, err := f(rctx, cid, cid, 8192)
		if err != nil {
			return nil
		}
		return b
	}

	for time.Now().Before(end) {
		reads++
		b := read(agent.ReadStdout)
		out.Write(b)
		errOut.Write(read(agent.ReadStderr))
		if len(b) == 0 {
			empties++
		}
		if strings.Contains(out.String(), "PROBE DONE") {
			break
		}
	}
	t.Logf("drained the container streams in %d reads (%d returned nothing)", reads, empties)
	return out.String(), errOut.String()
}

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
