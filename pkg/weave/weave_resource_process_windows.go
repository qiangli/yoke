//go:build windows

package weave

import (
	"context"
	"errors"
	"os/exec"
)

func weavePrepareOwnedChild(cmd *exec.Cmd) {}
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
