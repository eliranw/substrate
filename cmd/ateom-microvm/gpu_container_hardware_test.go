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
)

// stageBusyboxRootfs builds a container rootfs out of the guest image's own
// busybox, at the path virtiofsd serves to the guest.
//
// A CUDA image would be the realistic workload, but it is not needed to answer
// either question and would drag a registry pull onto the host. CDI supplies the
// driver libraries and device nodes itself, from the guest -- that is precisely
// the mechanism under test -- so the rootfs only has to provide a shell.
//
// busybox comes from the guest image because it is known to be a static binary
// that runs in this guest, which a host busybox may not be.
func stageBusyboxRootfs(t *testing.T, env hwEnv, sharedDir, cid string) string {
	t.Helper()
	rootfs := filepath.Join(sharedDir, cid, "rootfs")
	for _, d := range []string{"bin", "dev", "proc", "sys", "etc", "tmp"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// p1 of the guest image is a plain EROFS filesystem at 1MiB (the dm-verity
	// hash tree is a separate partition), so it mounts read-only as-is.
	mnt := filepath.Join(t.TempDir(), "guestroot")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatalf("mkdir mountpoint: %v", err)
	}
	img := filepath.Join(env.assets, "rootfs-gpu.img")
	mount := exec.Command("mount", "-o", "loop,offset=1048576,ro", img, mnt)
	if out, err := mount.CombinedOutput(); err != nil {
		t.Fatalf("mounting the guest image to borrow busybox: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("umount", mnt).Run() })

	src, err := os.ReadFile(filepath.Join(mnt, "bin", "busybox"))
	if err != nil {
		t.Fatalf("reading busybox from the guest image: %v", err)
	}
	dst := filepath.Join(rootfs, "bin", "busybox")
	if err := os.WriteFile(dst, src, 0o755); err != nil {
		t.Fatalf("writing busybox: %v", err)
	}
	// The agent execs the process directly, so give it the usual names too.
	for _, name := range []string{"sh", "ls", "cat", "sleep"} {
		if err := os.Symlink("busybox", filepath.Join(rootfs, "bin", name)); err != nil && !os.IsExist(err) {
			t.Fatalf("linking %s: %v", name, err)
		}
	}
	return rootfs
}

// containerSpec is the OCI spec for the probe container: the same shaping
// production applies, plus the CDI annotation under test.
func containerSpec(t *testing.T, args []string) *specs.Spec {
	t.Helper()
	spec := &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			Args: args,
			Cwd:  "/",
			Env:  []string{"PATH=/bin:/usr/bin"},
		},
		Mounts: defaultKataMounts(),
		Linux: &specs.Linux{
			Resources:   defaultKataResources(),
			CgroupsPath: "/ateomchv/gpuprobe",
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
	stageBusyboxRootfs(t, env, sharedDir, cid)

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
	probe := []string{"/bin/busybox", "sh", "-c",
		"echo '--- /dev/nvidia* ---'; ls -l /dev/nvidia* 2>&1; " +
			"echo '--- nvidia majors ---'; cat /proc/devices 2>&1 | grep -i nvidia; " +
			"echo '--- libs ---'; ls /usr/lib/x86_64-linux-gnu/libcuda* /usr/lib64/libcuda* 2>&1 | head -5; " +
			"echo '--- PROBE DONE ---'; sleep 600"}

	if err := agent.CreateCarrier(ctx, cid, containerSpec(t, probe)); err != nil {
		b, _ := os.ReadFile(serialLog)
		t.Fatalf("CreateContainer with the CDI annotation failed: %v\n"+
			"  this is gpucdi.go's contract: the agent could not resolve nvidia.com/gpu=all "+
			"against the guest's /var/run/cdi spec\n=== serial ===\n%s", err, tailBytes(b, 4000))
	}
	if err := agent.StartContainer(ctx, cid); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	t.Log("PASS create: the agent accepted the CDI annotation and created the container")

	out := readProbeOutput(ctx, t, agent, cid, 60*time.Second)
	t.Logf("=== container view of the GPU ===\n%s", out)

	if !strings.Contains(out, "/dev/nvidia") || strings.Contains(out, "No such file") {
		t.Errorf("CDI injected no device nodes into the container -- gpucdi.go's annotation " +
			"was accepted but had no effect")
	} else {
		t.Log("PASS inject: the container has /dev/nvidia* device nodes")
	}
}

// readProbeOutput drains the container's stdout until the probe's end marker.
func readProbeOutput(ctx context.Context, t *testing.T, agent *kata.AgentClient, cid string, deadline time.Duration) string {
	t.Helper()
	var b strings.Builder
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		chunk, err := agent.ReadStdout(ctx, cid, cid, 8192)
		if err != nil {
			t.Logf("ReadStdout stopped: %v", err)
			break
		}
		b.Write(chunk)
		if strings.Contains(b.String(), "PROBE DONE") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return b.String()
}

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
