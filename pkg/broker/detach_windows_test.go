//go:build windows

package broker

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDetachSysProcAttr_Breakaway(t *testing.T) {
	attr := detachSysProcAttr()
	if attr == nil {
		t.Fatalf("detachSysProcAttr returned nil, want non-nil SysProcAttr")
	}
	wantFlags := uint32(windows.CREATE_BREAKAWAY_FROM_JOB | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	if attr.CreationFlags != wantFlags {
		t.Errorf("detachSysProcAttr CreationFlags = 0x%08x, want 0x%08x", attr.CreationFlags, wantFlags)
	}
}

func TestStartWithBreakawayRetry_AccessDeniedRetriesOnce(t *testing.T) {
	cmd := exec.Command("door")
	cmd.SysProcAttr = detachSysProcAttr()

	calls := 0
	fakeStart := func() error {
		calls++
		if calls == 1 {
			return windows.ERROR_ACCESS_DENIED
		}
		return nil
	}

	if err := startWithBreakawayRetry(cmd, fakeStart); err != nil {
		t.Fatalf("startWithBreakawayRetry failed: %v", err)
	}

	if calls != 2 {
		t.Fatalf("expected 2 calls (retried once after access denied), got %d", calls)
	}

	if (cmd.SysProcAttr.CreationFlags & windows.CREATE_BREAKAWAY_FROM_JOB) != 0 {
		t.Errorf("CREATE_BREAKAWAY_FROM_JOB should be cleared on retry, got 0x%08x", cmd.SysProcAttr.CreationFlags)
	}

	wantFlags := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	if cmd.SysProcAttr.CreationFlags != wantFlags {
		t.Errorf("CreationFlags after retry = 0x%08x, want 0x%08x", cmd.SysProcAttr.CreationFlags, wantFlags)
	}
}

func TestStartWithBreakawayRetry_PathErrorAccessDenied(t *testing.T) {
	cmd := exec.Command("door")
	cmd.SysProcAttr = detachSysProcAttr()

	calls := 0
	fakeStart := func() error {
		calls++
		if calls == 1 {
			return &os.PathError{Op: "CreateProcess", Path: "door.exe", Err: syscall.ERROR_ACCESS_DENIED}
		}
		return nil
	}

	if err := startWithBreakawayRetry(cmd, fakeStart); err != nil {
		t.Fatalf("startWithBreakawayRetry failed: %v", err)
	}

	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}

	if (cmd.SysProcAttr.CreationFlags & windows.CREATE_BREAKAWAY_FROM_JOB) != 0 {
		t.Errorf("CREATE_BREAKAWAY_FROM_JOB should be cleared on retry, got 0x%08x", cmd.SysProcAttr.CreationFlags)
	}
}

func TestStartWithBreakawayRetry_SuccessFirstTry(t *testing.T) {
	cmd := exec.Command("door")
	cmd.SysProcAttr = detachSysProcAttr()

	calls := 0
	fakeStart := func() error {
		calls++
		return nil
	}

	if err := startWithBreakawayRetry(cmd, fakeStart); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}

	if (cmd.SysProcAttr.CreationFlags & windows.CREATE_BREAKAWAY_FROM_JOB) == 0 {
		t.Errorf("CREATE_BREAKAWAY_FROM_JOB should remain set when start succeeds")
	}
}

func TestStartWithBreakawayRetry_OtherErrorNoRetry(t *testing.T) {
	cmd := exec.Command("door")
	cmd.SysProcAttr = detachSysProcAttr()

	calls := 0
	otherErr := windows.ERROR_FILE_NOT_FOUND
	fakeStart := func() error {
		calls++
		return otherErr
	}

	err := startWithBreakawayRetry(cmd, fakeStart)
	if !errors.Is(err, otherErr) {
		t.Fatalf("expected otherErr, got %v", err)
	}

	if calls != 1 {
		t.Fatalf("expected 1 call (no retry on file not found), got %d", calls)
	}

	if (cmd.SysProcAttr.CreationFlags & windows.CREATE_BREAKAWAY_FROM_JOB) == 0 {
		t.Errorf("CREATE_BREAKAWAY_FROM_JOB should not be modified on unrelated error")
	}
}

func TestStartWithBreakawayRetry_NoBreakawayFlagNoRetry(t *testing.T) {
	cmd := exec.Command("door")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}

	calls := 0
	fakeStart := func() error {
		calls++
		return windows.ERROR_ACCESS_DENIED
	}

	err := startWithBreakawayRetry(cmd, fakeStart)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("expected ERROR_ACCESS_DENIED, got %v", err)
	}

	if calls != 1 {
		t.Fatalf("expected 1 call when breakaway flag was not set, got %d", calls)
	}
}
