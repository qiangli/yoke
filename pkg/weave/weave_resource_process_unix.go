//go:build !windows

package weave

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func weavePrepareOwnedChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
func weaveConfigureOwnedCancellation(cmd *exec.Cmd) {
	// Cancellation targets this child's process group, never the wrapper's peers.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
func weaveOwnedChildTerminated(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return true
	} // start failed
	if cmd.ProcessState == nil {
		return false
	}
	// Wait alone is insufficient: descendants may outlive the direct child.
	return syscall.Kill(-cmd.Process.Pid, 0) == syscall.ESRCH
}

// Recheck the stored birth identity before every signal. Unknown/reused PIDs
// never grant authority; only the explicit owner's pause reaches this helper.
func weaveStopVerifiedWrapper(ctx context.Context, pid int, startID string, checkpoint func() bool) error {
	it := &weaveItem{WrapperPid: pid, WrapperStartID: startID}
	if err := weaveVerifiedWrapper(ctx, it); err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if checkpoint != nil && checkpoint() {
				return nil
			}
			if !pidAlive(pid) {
				return nil
			}
		case <-deadline.C:
			// The wrapper may still be preserving progress. Do not kill it and falsely
			// claim its isolated child session has terminated; report pending pause.
			return errors.New("pause requested; waiting for wrapper to preserve progress and exit")
		}
	}
}

func weaveWatchPlainTermination(cancel context.CancelFunc) func() {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-signals:
			cancel()
		case <-done:
		}
	}()
	return func() { signal.Stop(signals); close(done) }
}
