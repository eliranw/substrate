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
			// Everything the probe prints goes to a file in its own rootfs, which
			// is the virtiofs-shared directory the host also sees. The agent's
			// stdout stream is opened when the container starts and does not
			// survive an eject or a restore, so it cannot report on the cycle it
			// exists to observe -- every "unresolved" result came from reading it.
			"exec >> /probe.log 2>&1; " +
				"echo '--- MARK ---'; ls /dev | grep -i nvidia | tr '\\n' ' '; echo; " +
				"echo '--- pci ---'; for d in /sys/bus/pci/devices/*/; do " +
				"[ \"$(cat $d/vendor 2>/dev/null)\" = 0x10de ] || continue; echo \"$d\"; " +
				"head -3 $d/resource; " +
				"echo \"  driver: $(readlink $d/driver 2>&1)\"; " +
				"echo \"  enable: $(cat $d/enable 2>&1)\"; " +
				// resource is the kernel's RECORD of the assignment, restored from the
				// snapshot along with the rest of guest memory. config space is the
				// device's actual BAR registers. If those disagree, the driver is
				// reading MMIO at an address nothing answers -- which is what a bound
				// driver reporting Unknown Error looks like.
				"echo '  config BARs (0x10..0x27):'; " +
				"od -A x -t x4 -j 16 -N 24 $d/config 2>&1 | head -3; " +
				// A device that is not responding reads back as all-ones.
				"echo \"  vendor/device: $(cat $d/vendor 2>&1) $(cat $d/device 2>&1)\"; " +
				"done; " +
				"echo '--- driver view ---'; ls /proc/driver/nvidia/gpus/ 2>&1; " +
				"nvidia-smi -L 2>&1 | head -3; " +
				"echo '--- persistence off ---'; nvidia-smi -pm 0 2>&1 | head -3; " +
				"echo '--- PROBE DONE ---'; " +
				"sleep " + quiet + "; " +
				"while true; do echo '--- MARK ---'; ls /dev | grep -i nvidia | tr '\\n' ' '; echo; " +
				// A bound driver that cannot produce a handle suggests its per-GPU
				// state did not survive the remove/re-add. Unbind and rebind forces a
				// clean re-probe; if the GPU comes back, that is the missing step.
				"nvidia-smi -L 2>&1 | grep -q UUID || { echo '--- rebinding ---'; " +
				"B=$(basename $(ls -d /sys/bus/pci/devices/*/ | while read d; do " +
				"[ \"$(cat $d/vendor 2>/dev/null)\" = 0x10de ] && echo $d; done | head -1)); " +
				"echo \"  bdf=$B\"; " +
				"echo $B > /sys/bus/pci/drivers/nvidia/unbind 2>&1; sleep 2; " +
				"echo $B > /sys/bus/pci/drivers/nvidia/bind 2>&1; sleep 3; " +
				"echo '  after rebind:'; nvidia-smi -L 2>&1 | head -3; }; " +
				"echo '--- pci ---'; for d in /sys/bus/pci/devices/*/; do " +
				"[ \"$(cat $d/vendor 2>/dev/null)\" = 0x10de ] || continue; echo \"$d\"; " +
				"head -3 $d/resource; " +
				"echo \"  driver: $(readlink $d/driver 2>&1)\"; " +
				"echo \"  enable: $(cat $d/enable 2>&1)\"; " +
				// resource is the kernel's RECORD of the assignment, restored from the
				// snapshot along with the rest of guest memory. config space is the
				// device's actual BAR registers. If those disagree, the driver is
				// reading MMIO at an address nothing answers -- which is what a bound
				// driver reporting Unknown Error looks like.
				"echo '  config BARs (0x10..0x27):'; " +
				"od -A x -t x4 -j 16 -N 24 $d/config 2>&1 | head -3; " +
				// A device that is not responding reads back as all-ones.
				"echo \"  vendor/device: $(cat $d/vendor 2>&1) $(cat $d/device 2>&1)\"; " +
				"done; " +
				"echo '--- driver view ---'; ls /proc/driver/nvidia/gpus/ 2>&1; " +
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
		// defaultKataMounts gives /sys read-only, as production should. The probe
		// needs to write /sys/bus/pci/drivers/nvidia/{unbind,bind} to test whether a
		// driver rebind revives a hot-plugged GPU, so this test alone remounts it rw.
		Mounts: sysWritable(defaultKataMounts()),
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

// sysWritable drops "ro" from the /sys mount so the probe can drive sysfs.
func sysWritable(ms []specs.Mount) []specs.Mount {
	out := make([]specs.Mount, len(ms))
	copy(out, ms)
	for i := range out {
		if out[i].Destination != "/sys" {
			continue
		}
		var opts []string
		for _, o := range out[i].Options {
			if o != "ro" {
				opts = append(opts, o)
			}
		}
		out[i].Options = opts
	}
	return out
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
	if _, err := client.WaitReady(ctx, 15*time.Second); err != nil {
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
	// NVIDIA's config carries pci=realloc, pci=nocrs and pci=assign-busses, all
	// aimed at bare-metal firmware. ATE_STRIP_KPARAMS removes any of them so their
	// effects can be separated.
	//
	// pci=nocrs is the suspect. It tells Linux to IGNORE the ACPI _CRS host-bridge
	// windows -- the ranges the VMM declares it will actually serve -- and assume
	// MMIO begins just above top-of-RAM. This guest has 2048MiB, so that is
	// 0x80000000, exactly the address the kernel keeps demanding and clh keeps
	// refusing. Cold boot is unaffected because clh lays the BARs out itself and
	// the guest only reads them; hot-plug makes the guest ASSIGN one, and nocrs
	// has blinded it to where clh's aperture is.
	//
	// pci=realloc was the first guess and is not the trigger: stripping it alone
	// reproduced the identical failure at the identical address.
	kparams := kata.WithDebugConsole(cfg.KernelParams)
	for _, p := range strings.Split(os.Getenv("ATE_STRIP_KPARAMS"), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		kparams = strings.ReplaceAll(kparams, p, "")
		t.Logf("stripped %q from the guest cmdline", p)
	}
	vmCfg := buildVMConfig(id,
		filepath.Join(env.assets, "vmlinux-gpu"), filepath.Join(env.assets, "rootfs-gpu.img"),
		kparams, cfg.RootfsType, serialLog,
		cfg.MemoryMiB, cfg.VCPUs, false, devices)
	t.Logf("cmdline: %s", vmCfg.Payload.Cmdline)
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

	out := probeLog(t, sharedDir, cid, "--- PROBE DONE ---", 60*time.Second)
	if strings.TrimSpace(out) == "" {
		// The compiled probe (no ATE_PROBE_ROOTFS) writes to stdout, not the file.
		out, _ = readProbeOutput(ctx, t, agent, cid, 60*time.Second)
	}
	beforeLen := len(out)
	t.Logf("=== container output (%d bytes) ===\n%s", len(out), out)

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
	// ATE_SKIP_SNAPSHOT isolates the two variables in the cycle. The full path is
	// eject -> snapshot -> restore -> hot-plug, and a failure at the end could be
	// caused by either half. With it set, the device is ejected and immediately
	// hot-plugged back into the SAME running VM: no snapshot, no restore, same
	// guest, same driver, same container.
	//
	//   still broken -> hot-plug itself is the fault; restore is innocent
	//   works        -> hot-plug is fine and the restore is what breaks it
	skipSnapshot := os.Getenv("ATE_SKIP_SNAPSHOT") != ""
	if skipSnapshot {
		t.Log("=== cycling the device: detach -> re-attach (no snapshot/restore) ===")
	} else {
		t.Log("=== cycling the device: detach -> snapshot -> restore -> re-attach ===")
	}

	// Quiesce exactly as production does. nvidia-persistenced keeps the device
	// open for as long as it runs, and the driver's removal path spins on
	// usage_count, so an eject requested without this never completes.
	//
	// Best-effort, like detachPassthrough: it needs nvidia-smi on the container's
	// PATH, which only a rootfs with a dynamic loader has (CDI mounts the binary
	// in, it does not static-link it). The hand-assembled probe rootfs has
	// neither, so this fails there with exit 127 and the eject below is what
	// reports the consequence.
	persistErr := clearGPUPersistence(ctx, id, []string{cid})
	if persistErr != nil {
		t.Logf("could not clear GPU persistence: %v", persistErr)
	}

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
			hint := "  Persistence was cleared, so the usual holder is gone. What remains is a\n" +
				"  live handle: a process in the container with /dev/nvidia* still open keeps\n" +
				"  usage_count non-zero and the driver's removal path spins until it drops.\n" +
				"  Suspend expects an idle GPU."
			if persistErr != nil {
				hint = "  Persistence was NOT cleared -- see the log line above. nvidia-persistenced\n" +
					"  holds the device from boot, so this eject cannot complete. Point\n" +
					"  ATE_PROBE_ROOTFS at a real image rootfs so nvidia-smi can run; the\n" +
					"  hand-assembled one has no dynamic loader. This is a limitation of the\n" +
					"  probe, not of the detach path."
			}
			t.Fatalf("WaitDeviceRemoved(%s): %v\n%s", did, err, hint)
		}
	}
	if err := errIfPassthroughSnapshot(ctx, client); err != nil {
		t.Fatalf("gate still sees a device after a confirmed eject: %v", err)
	}
	t.Log("PASS detach: device ejected while the container kept running")

	// The isolating path stops here: same VMM, same guest, straight back in.
	client2, chCmd2 := client, chCmd
	if skipSnapshot {
		for _, d := range devices {
			if _, err := client.AddDevice(ctx, d.Path); err != nil {
				t.Fatalf("AddDevice(%s): %v", d.Path, err)
			}
		}
		if err := waitDevicesAttached(ctx, client, len(devices), 60*time.Second); err != nil {
			t.Fatalf("re-attach: %v", err)
		}
		t.Log("PASS re-attach: the VMM holds the device again (no snapshot taken)")
		agent2, err := dialAgentRetry(ctx, vsock, 60*time.Second)
		if err != nil {
			t.Fatalf("re-dialing the agent: %v", err)
		}
		t.Cleanup(func() { _ = agent2.Close() })
		after := probeLogAfter(t, sharedDir, cid, "--- PROBE DONE ---", beforeLen, 90*time.Second)
		t.Logf("=== container view after eject+re-attach, NO restore (%d NEW bytes) ===\n%s", len(after), after)
		if strings.Contains(after, "Tesla") || strings.Contains(after, "UUID") {
			t.Log("PASS: the GPU survives a plain eject and re-attach")
			t.Log("  NOTE: this says nothing about the restore -- run without " +
				"ATE_SKIP_SNAPSHOT to exercise the full cycle")
			return
		}
		// The probe's stream rarely survives the cycle, so its silence decides
		// nothing. Ask the container directly instead of reading absence of output
		// as a dead GPU.
		if err := gpuUsableViaExec(ctx, agent2, cid, cid+"_post"); err != nil {
			t.Fatalf("the GPU is unusable after a plain eject+re-attach, with no snapshot "+
				"or restore involved: hot-plug itself is the fault and the restore is "+
				"innocent: %v", err)
		}
		t.Log("PASS: the GPU survives a plain eject and re-attach (proven by exec; " +
			"the probe's own stream did not survive)")
		t.Log("  NOTE: this says nothing about the restore -- run without " +
			"ATE_SKIP_SNAPSHOT to exercise the full cycle")
	}

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

	chCmd2, client2, err = ch.LaunchVMM(ctx, ch.LaunchVMMOptions{
		Binary: env.chBin, APISocket: kata.CLHSocketPath(id),
		Stdout: testWriter{t, "clh2"}, Stderr: testWriter{t, "clh2"},
	})
	if err != nil {
		t.Fatalf("relaunch VMM: %v", err)
	}
	t.Cleanup(func() { _ = chCmd2.Process.Kill(); _, _ = chCmd2.Process.Wait() })
	if _, err := client2.WaitReady(ctx, 15*time.Second); err != nil {
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

	after := probeLogAfter(t, sharedDir, cid, "--- PROBE DONE ---", beforeLen, 120*time.Second)
	t.Logf("=== container view AFTER the cycle (%d NEW bytes) ===\n%s", len(after), after)
	if strings.Contains(after, "Tesla") || strings.Contains(after, "UUID") {
		t.Log("PASS claim B: the container still uses the GPU after detach and re-attach")
		return
	}
	if strings.TrimSpace(after) == "" {
		// Silence is the usual case: the probe's stream is opened at container
		// start and does not survive restore. It decides nothing either way, so
		// fall through to an exec rather than calling claim B failed OR unresolved.
		t.Log("the probe produced no output after the cycle -- its stream did not survive; " +
			"asking the container directly instead")
		if err := gpuUsableViaExec(ctx, agent2, cid, cid+"_post"); err != nil {
			t.Fatalf("claim B FAILS: the container cannot use the GPU after the cycle: %v", err)
		}
		t.Log("PASS claim B: the container still uses the GPU after detach and re-attach " +
			"(proven by exec; the probe's own stream did not survive)")
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

// probeLog reads what the probe has written so far, waiting up to deadline for
// `want` to appear.
//
// The probe writes into its own rootfs, which lives under the virtiofs-shared
// directory, so this is a plain file read on the host. That matters because the
// alternative -- draining the container's stdout over ttrpc -- stops working the
// moment the device is ejected or the guest is restored, which is exactly when
// the interesting output is produced.
//
// Returns everything written so far; callers slice from a previously recorded
// length to isolate what a cycle produced.
func probeLog(t *testing.T, sharedDir, cid, want string, deadline time.Duration) string {
	t.Helper()
	path := filepath.Join(sharedDir, cid, "rootfs", "probe.log")
	end := time.Now().Add(deadline)
	var last []byte
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			last = b
			if want == "" || strings.Contains(string(b), want) {
				return string(b)
			}
		}
		if time.Now().After(end) {
			if err != nil {
				t.Logf("probe log %s unreadable after %s: %v", path, deadline, err)
			} else {
				t.Logf("probe log has %d bytes but never contained %q", len(last), want)
			}
			return string(last)
		}
		time.Sleep(time.Second)
	}
}

// probeLogAfter returns only what the probe wrote past mark, or "" if it wrote
// nothing new before the deadline.
//
// Returning "" rather than the whole file is the point. The probe reports once
// at startup and then sleeps through its quiet window, so a read taken during
// that window returns the PRE-cycle block -- which names the GPU, lists healthy
// BARs, and is indistinguishable from a good post-cycle result. Stale evidence
// that reads as success is worse than no evidence, because nothing downstream
// can tell the difference.
func probeLogAfter(t *testing.T, sharedDir, cid, want string, mark int, deadline time.Duration) string {
	t.Helper()
	path := filepath.Join(sharedDir, cid, "rootfs", "probe.log")
	end := time.Now().Add(deadline)
	var tail string
	for {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > mark {
			tail = string(b[mark:])
			// Wait for a COMPLETE iteration, not merely for new bytes. When the
			// GPU looks dead the probe unbinds the driver and rebinds it, which
			// takes five seconds; returning at the first new byte samples the
			// middle of that, where the driver is deliberately detached. Anything
			// asking the device during that window sees a broken GPU that our own
			// recovery attempt broke.
			if want == "" || strings.Contains(tail, want) {
				return tail
			}
		}
		if time.Now().After(end) {
			if tail == "" {
				t.Logf("the probe wrote nothing new in %s -- it is probably still in its "+
					"quiet window (ATE_PROBE_QUIET_SECS, default 90s). Deciding by exec instead.",
					deadline)
			} else {
				t.Logf("the probe wrote %d bytes in %s but never completed an iteration "+
					"(no %q); it may still be rebinding", len(tail), deadline, want)
			}
			return tail
		}
		time.Sleep(time.Second)
	}
}

// gpuUsableViaExec runs nvidia-smi in the container and reports whether it found
// a GPU, using the exit code rather than the probe's stdout.
//
// The probe writes to a stream opened when the container started, and that
// stream does not survive the cycle -- ttrpc reports "message on inactive
// stream" and reads return nothing whether the GPU works or not. Silence there
// is a harness artefact, so it cannot decide claim B in either direction. A
// fresh exec is independent of it.
func gpuUsableViaExec(ctx context.Context, agent *kata.AgentClient, cid, execID string) error {
	code, err := agent.ExecProcess(ctx, cid, execID, []string{"nvidia-smi", "-L"})
	if err != nil {
		return fmt.Errorf("exec nvidia-smi: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("nvidia-smi exited %d", code)
	}
	return nil
}
