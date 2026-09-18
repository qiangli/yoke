//go:build !windows

package agentpty

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWrapperSignalContract pins WHICH signals terminate a subagent. SIGTERM
// (weave kill/abandon) and SIGINT (interactive ^C) must; SIGHUP (the launcher /
// controlling-terminal death signal) must NOT — an orchestrator that exits,
// restarts, or idle-times-out delivers SIGHUP to a still-attached worker, and
// forwarding it killed actively-working runs with ~0 commits.
func TestWrapperSignalContract(t *testing.T) {
	inKill := func(s os.Signal) bool {
		for _, k := range wrapperKillSignals {
			if k == s {
				return true
			}
		}
		return false
	}
	inIgnore := func(s os.Signal) bool {
		for _, k := range wrapperIgnoreSignals {
			if k == s {
				return true
			}
		}
		return false
	}
	if !inKill(syscall.SIGTERM) {
		t.Error("SIGTERM must remain a wrapper kill signal (weave kill/abandon relies on it)")
	}
	if !inKill(syscall.SIGINT) {
		t.Error("SIGINT must remain a wrapper kill signal (interactive ^C)")
	}
	if inKill(syscall.SIGHUP) {
		t.Error("SIGHUP must NOT be a wrapper kill signal — launcher/terminal death must not kill an active worker")
	}
	if !inIgnore(syscall.SIGHUP) {
		t.Error("SIGHUP must be intercepted-and-dropped so it neither kills the subagent nor terminates the wrapper")
	}
}

// TestRunSurvivesSIGHUPButHonorsSIGTERM is the behavioral guard for the bug: a
// worker actively running under the wrapper must survive a SIGHUP (its launcher
// went away) and only die on an explicit SIGTERM.
//
// The signals are sent to THIS process, which is safe because Run has them
// under signal.Notify for the whole window — Go delivers a notified signal to
// the registered channel instead of taking the default (process-terminating)
// action. We send SIGHUP/SIGTERM only after Run has installed its handlers.
func TestRunSurvivesSIGHUPButHonorsSIGTERM(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30")

	type result struct {
		exit   int
		reason string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		exit, reason, err := Run(cmd, io.Discard, Options{Capture: true})
		done <- result{exit, reason, err}
	}()

	// Wait for the subagent to actually start, then give Run a beat to install
	// its signal handlers (pty.Start sets cmd.Process just before Notify).
	deadline := time.Now().Add(5 * time.Second)
	for cmd.Process == nil && time.Now().Before(deadline) {
		select {
		case r := <-done:
			t.Fatalf("Run returned before the subagent started: exit=%d reason=%q err=%v", r.exit, r.reason, r.err)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if cmd.Process == nil {
		t.Skip("subagent never started (no PTY available in this environment)")
	}
	time.Sleep(300 * time.Millisecond) // ensure signal.Notify is armed

	// SIGHUP must NOT terminate the worker.
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
	select {
	case r := <-done:
		t.Fatalf("SIGHUP terminated the subagent (exit=%d reason=%q) — a launcher/terminal hangup must not kill an active worker", r.exit, r.reason)
	case <-time.After(750 * time.Millisecond):
		// Still running — correct.
	}

	// SIGTERM MUST terminate the worker (the weave kill/abandon path).
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case r := <-done:
		if !strings.Contains(r.reason, "terminated") {
			t.Fatalf("SIGTERM did not record a wrapper kill reason, got %q", r.reason)
		}
	case <-time.After(5 * time.Second):
		// Leave nothing behind if the contract regressed.
		_ = cmd.Process.Kill()
		t.Fatal("SIGTERM did not terminate the subagent within 5s")
	}
}
