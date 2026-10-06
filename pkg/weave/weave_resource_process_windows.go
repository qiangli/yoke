//go:build windows

package weave

import (
	"context"
	"errors"
	"os/exec"
)

func weavePrepareOwnedChild(cmd *exec.Cmd)      {}
func weaveOwnedChildGroup(pid int) (int, error) { return 0, nil }
func weaveRecordedGroupStopped(it *weaveItem) error {
	return errors.New("owned child termination proof unavailable on Windows; retain reservation")
}
func weaveOwnedChildTerminated(cmd *exec.Cmd) bool {
	// No job-object lifetime proof in this wrapper yet. Retain capacity when a
	// child was launched; a verified external reconciliation may settle it.
	return cmd.Process == nil
}

func weaveStopVerifiedWrapper(ctx context.Context, pid int, startID string, checkpoint func() bool) error {
	return errors.New("verified wrapper control unavailable on Windows")
}

func weaveWatchPlainTermination(cancel context.CancelFunc) func() { return func() {} }

func weaveConfigureOwnedCancellation(cmd *exec.Cmd) {}
func weaveAbortOwnedChild(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
