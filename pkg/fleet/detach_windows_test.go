//go:build windows

package fleet

import (
	"errors"
	"syscall"
	"testing"
)

func TestStartDetachedRetryRetriesWithoutBreakawayOnAccessDenied(t *testing.T) {
	attr := &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob | detachedProcess}
	calls := 0
	err := startDetachedRetry(func() error {
		calls++
		if calls == 1 {
			return &pathLikeError{syscall.ERROR_ACCESS_DENIED}
		}
		return nil
	}, attr)
	if err != nil {
		t.Fatalf("startDetachedRetry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("start calls = %d, want 2 (one retry)", calls)
	}
	if attr.CreationFlags&createBreakawayFromJob != 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_BREAKAWAY_FROM_JOB cleared", attr.CreationFlags)
	}
}

func TestStartDetachedRetryDoesNotRetryOnSuccess(t *testing.T) {
	attr := &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob | detachedProcess}
	calls := 0
	err := startDetachedRetry(func() error {
		calls++
		return nil
	}, attr)
	if err != nil {
		t.Fatalf("startDetachedRetry: %v", err)
	}
	if calls != 1 {
		t.Fatalf("start calls = %d, want 1 (no retry on success)", calls)
	}
	if attr.CreationFlags&createBreakawayFromJob == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_BREAKAWAY_FROM_JOB left set", attr.CreationFlags)
	}
}

func TestStartDetachedRetryDoesNotRetryOnUnrelatedError(t *testing.T) {
	attr := &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob | detachedProcess}
	calls := 0
	wantErr := errors.New("boom")
	err := startDetachedRetry(func() error {
		calls++
		return wantErr
	}, attr)
	if !errors.Is(err, wantErr) {
		t.Fatalf("startDetachedRetry error = %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("start calls = %d, want 1 (no retry on unrelated error)", calls)
	}
}

func TestStartDetachedRetryDoesNotRetryWithoutBreakawayFlag(t *testing.T) {
	attr := &syscall.SysProcAttr{CreationFlags: detachedProcess}
	calls := 0
	err := startDetachedRetry(func() error {
		calls++
		return &pathLikeError{syscall.ERROR_ACCESS_DENIED}
	}, attr)
	if err == nil {
		t.Fatal("startDetachedRetry: want the access-denied error back, got nil")
	}
	if calls != 1 {
		t.Fatalf("start calls = %d, want 1 (nothing to retry without the breakaway flag)", calls)
	}
}

// pathLikeError mimics the *os.PathError exec.Cmd.Start wraps a
// CreateProcess failure in, so errors.Is(err, syscall.ERROR_ACCESS_DENIED)
// is exercised the way it is against a real Start() failure.
type pathLikeError struct{ err error }

func (e *pathLikeError) Error() string { return "fork/exec: " + e.err.Error() }
func (e *pathLikeError) Unwrap() error { return e.err }
