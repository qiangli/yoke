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

// weaveStopWrapper precisely terminates the wrapper process whose
// PID is recorded on a queue item. The wrapper auto-setsid'd at
// startup, so signalling the negative PID hits the whole subagent
// process group — claude, codex, their MCP children, all caught in
// one SIGTERM. After a brief grace window we escalate to SIGKILL.
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
	// SIGTERM the process group (negative PID). The wrapper put
	// itself in a new session via Setsid, so its PID is the PGID
	// of the entire subagent tree.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		// Group send failed (group may not exist if Setsid never
		// ran); fall back to single-process TERM.
		_ = proc.Signal(syscall.SIGTERM)
	}
	// 5-second grace; if still alive, escalate.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = proc.Signal(syscall.SIGKILL)
	}
}
