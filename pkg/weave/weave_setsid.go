//go:build !windows

package weave

import (
	"log/slog"
	"os"
	"syscall"
	"time"
)

// weaveMaybeSetsid moves the current process into a new session
// when invoked non-interactively, so a backgrounded `bashy weave
// start ... &` survives SIGHUP from its launching shell. Skipped
// when the parent stdin is a TTY because a user at a terminal
// expects ^C to reach the foreground ycode (Setsid would detach us
// from the controlling terminal and break that).
//
// Errors are logged and ignored — we may already be a session
// leader (EPERM) on platforms where the parent forked us with
// setsid for some other reason; either way, the worst case is the
// child gets SIGHUP'd when the shell exits, the same behavior as
// before this helper existed. Refusing to start is the wrong call.
func weaveMaybeSetsid(parentStdinTTY bool) {
	if parentStdinTTY {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		slog.Debug("weave: Setsid failed (likely already a session leader)", "err", err)
	}
}

// pidAlive reports whether a process with the given PID currently
// exists (signal 0 probe). Subject to PID reuse — callers use it as
// a conservative "maybe still running" check, never as proof of
// identity.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// weaveStopWrapper signals only the recorded wrapper. A plain child uses
// its own process group, and a PTY child uses its own session; both depend on
// the wrapper's cancellation path to stop them. The later recorded-child
// group probe is therefore required before releasing any reservation.
//
// Used by `weave abandon` instead of pkill-by-name. pkill -f would
// also catch peer ycode / claude / codex sessions belonging to
// other agents in a shared environment, which the dogfood found
// (and the user called out) as a real safety issue.
func weaveStopWrapper(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	// Existence probe — if the wrapper already exited, nothing to do.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return
	}
	// A wrapper group can contain sibling wrappers: setsid is best effort,
	// and interactive launches intentionally retain their launcher's session.
	// Only the wrapper owns cancellation of its isolated child group. Never
	// infer ownership of every group member from the wrapper's recorded PID.
	_ = proc.Signal(syscall.SIGTERM)
	// 5-second grace; if still alive, escalate.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
}
