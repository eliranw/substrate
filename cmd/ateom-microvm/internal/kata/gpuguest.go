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

package kata

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// guestExec runs a shell command inside the guest. A var so tests can stub it.
//
// Note what it does NOT do: report failure. DebugConsoleDump returns its own
// error text as the output string ("debug-console dial: ..."), so every caller
// here has to distinguish "the guest answered" from "we never reached it" by
// looking at the content. Each command below therefore prints a sentinel of its
// own, and a missing sentinel is an error rather than an empty result.
var guestExec = DebugConsoleDump

// scanOK is printed after the device scan completes. Without it, a debug console
// that failed to dial is indistinguishable from a guest that genuinely has no
// GPU -- and the second reading is the dangerous one, because "no GPU attached"
// is exactly the state that makes the caller believe a detach already succeeded.
// The console runs on a PTY and echoes the command line before running it, so a
// sentinel written literally would come back in the echo whether or not the
// command ever ran. It is therefore split by an empty quote pair, which the
// shell strips: the echo shows the split form and does not match, while the
// shell's own output is the joined form and does. DebugConsoleDump plays the
// same trick with its end marker, for the same reason.
const scanOK = "__ATE_SCAN_OK__"

// listGPUsCmd enumerates NVIDIA display-class devices straight out of sysfs.
//
// Deliberately not lspci: the NVIDIA guest rootfs is chiselled down to busybox,
// NVRC and the kata-agent, and lspci ships in pciutils, which is not part of
// that set. sysfs is always there, needs no tool, and its directory names are
// already the full-domain BDF form that /sys/bus/pci/drivers/*/{bind,unbind}
// expects -- lspci prints the short form and would need normalising.
//
// Class 0x03* is the display controllers (0x0300 VGA, 0x0302 3D -- a T4 reports
// 3D). The filter matters because passthrough moves the whole IOMMU group, so a
// card's HDMI-audio function comes along as a second 0x10de device; it belongs
// to snd_hda_intel, not nvidia, and unbinding it here would be wrong.
const listGPUsCmd = `for d in /sys/bus/pci/devices/*/; do
v=$(cat "$d/vendor" 2>/dev/null)
c=$(cat "$d/class" 2>/dev/null)
[ "$v" = "0x10de" ] || continue
case "$c" in 0x03*) ;; *) continue ;; esac
d=${d%/}
echo "${d##*/}"
done
echo __ATE_SCAN''_OK__`

// GuestGPUBDFs lists the PCI addresses of the NVIDIA GPUs the guest can see.
//
// These are GUEST addresses and do not match the host BDFs the worker was
// granted: cloud-hypervisor places a passed-through device at an address of its
// own choosing on the guest bus. Anything driving guest-side sysfs has to
// enumerate here rather than reuse the host's allocation.
func GuestGPUBDFs(ctx context.Context, vsockPath string) ([]string, error) {
	out := guestExec(ctx, vsockPath, listGPUsCmd)
	if !strings.Contains(out, scanOK) {
		return nil, fmt.Errorf("could not scan guest PCI devices (the guest never answered): %s", oneLine(out))
	}
	var bdfs []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.TrimSpace(line)
		// A sysfs device directory name is domain:bus:slot.func, and the scan
		// prints nothing else per line, so anything shaped differently is echo or
		// console noise rather than a device.
		if strings.Count(f, ":") == 2 && strings.Count(f, ".") == 1 && !strings.ContainsAny(f, " \t/") {
			bdfs = append(bdfs, f)
		}
	}
	return bdfs, nil
}

// GuestDetachGPU releases the GPU inside the guest so the VMM can eject it.
//
// Two things make this subtle:
//   - nvidia-persistenced holds the device open even with no workload running,
//     and it is the only systematic holder; with it stopped an idle actor has
//     none, so the unbind returns immediately instead of spinning on a non-zero
//     usage count.
//   - the NVIDIA driver's remove path returns EIO to the sysfs write AFTER
//     device_release_driver() has completed, so the write's exit status is not a
//     success signal. The absence of /sys/bus/pci/devices/<bdf>/driver is.
func GuestDetachGPU(ctx context.Context, vsockPath, bdf string) error {
	guestExec(ctx, vsockPath, `pkill nvidia-persistenced 2>/dev/null; sleep 1`)
	guestExec(ctx, vsockPath, fmt.Sprintf(`echo %s > /sys/bus/pci/devices/%s/driver/unbind 2>/dev/null`, bdf, bdf))
	bound, err := guestDriverBound(ctx, vsockPath, bdf)
	if err != nil {
		return err
	}
	if bound {
		return fmt.Errorf("guest GPU %s is still bound to a driver after unbind", bdf)
	}
	return nil
}

// GuestVerifyGPUBound waits for the driver to claim a freshly attached GPU. The
// PCI core normally auto-binds it against the resident nvidia driver, but that
// is an inference rather than a guarantee, so poll and bind explicitly if it has
// not happened.
func GuestVerifyGPUBound(ctx context.Context, vsockPath, bdf string, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	askedExplicitly := false
	for {
		bound, err := guestDriverBound(ctx, vsockPath, bdf)
		if err != nil {
			return err
		}
		if bound {
			return nil
		}
		if !askedExplicitly {
			guestExec(ctx, vsockPath, fmt.Sprintf(`echo %s > /sys/bus/pci/drivers/nvidia/bind 2>/dev/null`, bdf))
			askedExplicitly = true
		}
		if time.Now().After(end) {
			return fmt.Errorf("guest GPU %s did not bind to the nvidia driver within %s", bdf, deadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// guestDriverBound reports whether a driver currently owns the device. Checking
// the sysfs symlink is deliberate: `lspci -k` prints BOTH a "Kernel modules:"
// candidate list (always present) and a "Kernel driver in use:" state line, and
// conflating them is easy. The symlink exists if and only if a driver is bound.
//
// An answer that is neither present nor absent is an error, never a default:
// treating an unreadable guest as "unbound" would report a detach that never
// happened, and the caller uses that to decide it is safe to snapshot.
func guestDriverBound(ctx context.Context, vsockPath, bdf string) (bool, error) {
	// Both answers are split by '' so the echoed command line contains neither
	// literally -- otherwise every reply would match "present" from the echo
	// alone and the device would always look bound.
	out := guestExec(ctx, vsockPath,
		fmt.Sprintf(`test -e /sys/bus/pci/devices/%s/driver && echo pres''ent || echo abs''ent`, bdf))
	switch {
	case strings.Contains(out, "present"):
		return true, nil
	case strings.Contains(out, "absent"):
		return false, nil
	default:
		return false, fmt.Errorf("could not determine driver state for %s in the guest: %s", bdf, oneLine(out))
	}
}

// oneLine flattens guest output for an error message.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
