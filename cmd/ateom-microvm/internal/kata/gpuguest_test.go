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
	"strings"
	"testing"
	"time"
)

// echoed mimics what DebugConsoleDump actually returns. The kata debug console
// is an interactive shell on a PTY, so it echoes the command line back before
// running it, and the reply contains BOTH the command text and its output.
//
// Every test here goes through this rather than returning a bare answer,
// because the echo is what makes naive matching wrong: a command that says
// "echo present || echo absent" puts both words in the reply no matter which
// branch runs. A stub that returns only "present\n" hides that entirely and
// will pass against an implementation that is broken in the guest.
func echoed(cmd, output string) string {
	s := "{ " + cmd + " ; } 2>&1; echo __ATE''_END__\r\n"
	if output != "" {
		s += output
	}
	return s
}

// stubGuest replaces guestExec with a scripted responder and records commands.
func stubGuest(t *testing.T, reply func(cmd string) string) *[]string {
	t.Helper()
	var seen []string
	orig := guestExec
	guestExec = func(ctx context.Context, vsockPath, cmd string) string {
		seen = append(seen, cmd)
		return echoed(cmd, reply(cmd))
	}
	t.Cleanup(func() { guestExec = orig })
	return &seen
}

func TestGuestGPUBDFsReadsSysfs(t *testing.T) {
	seen := stubGuest(t, func(cmd string) string {
		return "0000:00:02.0\n" + scanOK + "\n"
	})
	got, err := GuestGPUBDFs(context.Background(), "/tmp/vsock")
	if err != nil {
		t.Fatalf("GuestGPUBDFs: %v", err)
	}
	if len(got) != 1 || got[0] != "0000:00:02.0" {
		t.Fatalf("got %v, want [0000:00:02.0]", got)
	}
	// lspci is not in the chiselled NVIDIA guest rootfs (busybox + NVRC +
	// kata-agent); pciutils is not part of that set.
	if strings.Contains(strings.Join(*seen, "\n"), "lspci") {
		t.Error("must not depend on lspci; read sysfs instead")
	}
}

// A guest we could not reach must NOT read as a guest with no GPU. That reading
// is the dangerous one: "no device attached" is exactly the state that tells the
// caller a detach already succeeded and it is safe to snapshot.
func TestGuestGPUBDFsFailsWhenTheGuestNeverAnswered(t *testing.T) {
	stubGuest(t, func(cmd string) string { return "" })
	guestExec = func(ctx context.Context, vsockPath, cmd string) string {
		return "debug-console dial: dial unix /tmp/vsock: connect: connection refused"
	}
	got, err := GuestGPUBDFs(context.Background(), "/tmp/vsock")
	if err == nil {
		t.Fatalf("expected an error when the console is unreachable, got %v", got)
	}
	if !strings.Contains(err.Error(), "never answered") {
		t.Errorf("error should name the unreachable guest, got %q", err)
	}
}

// The console echoing the command back but never running it is the case a
// literal sentinel gets wrong: the echo would carry the sentinel and the scan
// would report "completed, no GPUs" for a shell that never executed. Only the
// split-quote form distinguishes them.
func TestGuestGPUBDFsFailsWhenOnlyTheEchoComesBack(t *testing.T) {
	stubGuest(t, func(cmd string) string { return "" }) // echo only, no output
	got, err := GuestGPUBDFs(context.Background(), "/tmp/vsock")
	if err == nil {
		t.Fatalf("a command that was echoed but never ran must not read as a completed scan, got %v", got)
	}
	if !strings.Contains(err.Error(), "never answered") {
		t.Errorf("error should name the unanswered guest, got %q", err)
	}
}

// The scan completing with nothing found is a real, different answer.
func TestGuestGPUBDFsEmptyScanIsNotAnError(t *testing.T) {
	stubGuest(t, func(cmd string) string { return scanOK + "\n" })
	got, err := GuestGPUBDFs(context.Background(), "/tmp/vsock")
	if err != nil {
		t.Fatalf("a completed scan finding nothing is not an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestGuestDetachStopsPersistencedThenUnbinds(t *testing.T) {
	seen := stubGuest(t, func(cmd string) string {
		if strings.Contains(cmd, "test -e") {
			return "absent\n"
		}
		return ""
	})
	if err := GuestDetachGPU(context.Background(), "/tmp/vsock", "0000:00:02.0"); err != nil {
		t.Fatalf("GuestDetachGPU: %v", err)
	}
	all := strings.Join(*seen, "\n")
	pi, ui := strings.Index(all, "nvidia-persistenced"), strings.Index(all, "unbind")
	if pi < 0 {
		t.Error("must stop nvidia-persistenced before unbinding")
	}
	if ui < 0 {
		t.Error("must write to the driver unbind file")
	}
	if pi > ui {
		t.Error("persistenced must be stopped BEFORE the unbind")
	}
}

// The NVIDIA driver's remove path returns EIO to the sysfs write even when the
// unbind succeeded, so success is the ABSENCE of the driver symlink, never the
// exit status.
func TestGuestDetachIgnoresEIOAndChecksSymlink(t *testing.T) {
	stubGuest(t, func(cmd string) string {
		if strings.Contains(cmd, "test -e") {
			return "absent\n"
		}
		return "sh: echo: I/O error\n"
	})
	if err := GuestDetachGPU(context.Background(), "/tmp/vsock", "0000:00:02.0"); err != nil {
		t.Fatalf("EIO on the unbind write must not fail the detach: %v", err)
	}
}

func TestGuestDetachFailsIfStillBound(t *testing.T) {
	stubGuest(t, func(cmd string) string {
		if strings.Contains(cmd, "test -e") {
			return "present\n"
		}
		return ""
	})
	if err := GuestDetachGPU(context.Background(), "/tmp/vsock", "0000:00:02.0"); err == nil {
		t.Fatal("expected an error while the driver symlink is still present")
	}
}

// An unreadable guest must not be read as "unbound". Reporting a detach that
// never happened is what lets a snapshot run with the device still live, which
// is the torn-memory bug the whole detach exists to avoid.
func TestGuestDetachFailsWhenDriverStateIsUnreadable(t *testing.T) {
	guestExec = func(ctx context.Context, vsockPath, cmd string) string {
		return "debug-console CONNECT reply: EOF"
	}
	t.Cleanup(func() { guestExec = DebugConsoleDump })
	err := GuestDetachGPU(context.Background(), "/tmp/vsock", "0000:00:02.0")
	if err == nil {
		t.Fatal("unreadable driver state must fail the detach, not pass it")
	}
	if !strings.Contains(err.Error(), "could not determine") {
		t.Errorf("error should name the unreadable state, got %q", err)
	}
}

func TestGuestVerifyBoundSucceedsWhenSymlinkAppears(t *testing.T) {
	stubGuest(t, func(cmd string) string { return "present\n" })
	if err := GuestVerifyGPUBound(context.Background(), "/tmp/vsock", "0000:00:02.0", time.Second); err != nil {
		t.Fatalf("GuestVerifyGPUBound: %v", err)
	}
}

func TestGuestVerifyBoundBindsExplicitlyIfNeeded(t *testing.T) {
	calls := 0
	seen := stubGuest(t, func(cmd string) string {
		if strings.Contains(cmd, "test -e") {
			calls++
			if calls == 1 {
				return "absent\n" // not auto-bound on the first look
			}
			return "present\n" // bound after we asked explicitly
		}
		return ""
	})
	if err := GuestVerifyGPUBound(context.Background(), "/tmp/vsock", "0000:00:02.0", 3*time.Second); err != nil {
		t.Fatalf("GuestVerifyGPUBound: %v", err)
	}
	if !strings.Contains(strings.Join(*seen, "\n"), "drivers/nvidia/bind") {
		t.Error("expected an explicit bind after the driver did not auto-bind")
	}
}

func TestGuestVerifyBoundTimesOut(t *testing.T) {
	stubGuest(t, func(cmd string) string { return "absent\n" })
	err := GuestVerifyGPUBound(context.Background(), "/tmp/vsock", "0000:00:02.0", 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout while the device never binds")
	}
	if !strings.Contains(err.Error(), "did not bind") {
		t.Errorf("error should say the device never bound, got %q", err)
	}
}

func TestGuestVerifyBoundHonoursContextCancellation(t *testing.T) {
	stubGuest(t, func(cmd string) string { return "absent\n" })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := GuestVerifyGPUBound(ctx, "/tmp/vsock", "0000:00:02.0", time.Minute); err == nil {
		t.Fatal("expected the cancelled context to abort the wait")
	}
}
