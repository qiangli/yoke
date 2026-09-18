package weave

import (
	"strings"
	"testing"
)

// A crash on the way OUT is not evidence that the work is bad. It is evidence of
// nothing at all about the work.
//
// Three opencode workers each finished their gate — tests passing, by their own
// logs — and then died in their storage layer on exit. Non-zero exit meant no
// auto-commit, which meant no commits, which meant no evidence, which meant
// `failed`, and the finished work sat uncommitted in a workspace that `weave
// prune` would have deleted.
//
// Now the tree is committed either way. The commit must be HONEST in both
// directions: it must not read like completed work (the run failed and the state
// says so), and it must not read like garbage (the work is often correct, and the
// next reader needs to know a decision is required, not a rerun).
func TestCrashedAutoCommitMessageIsHonestBothWays(t *testing.T) {
	it := &weaveItem{ID: 5, Title: "Gate 2: failure is loud"}
	msg := weaveCrashedAutoCommitMessage(it, 1, "")

	// It must NOT claim the work is done.
	if !strings.Contains(msg, "NOT submitted") {
		t.Errorf("message must say the run is not submitted:\n%s", msg)
	}
	if !strings.Contains(msg, "asserts nothing") {
		t.Errorf("message must disclaim any judgment about correctness:\n%s", msg)
	}
	if !strings.HasPrefix(msg, "wip(") {
		t.Errorf("a preserved crash must be labelled wip, not presented as a finished commit:\n%s", msg)
	}

	// And it must tell the reader what to DO.
	if !strings.Contains(msg, "weave salvage 5") {
		t.Errorf("message must name the recovery path for THIS issue:\n%s", msg)
	}
	if !strings.Contains(msg, "exited 1") {
		t.Errorf("message must record how the run ended:\n%s", msg)
	}

	killed := weaveCrashedAutoCommitMessage(it, 0, "watchdog: idle timeout")
	if !strings.Contains(killed, "killed: watchdog: idle timeout") {
		t.Errorf("a killed run must say so:\n%s", killed)
	}
}

// THE LOAD-BEARING INVARIANT: preserving the artifact must never promote it.
//
// Committing a crashed run's tree gives it commits — and `submitted` is decided
// partly on commits. If that were the whole test, this change would have quietly
// turned every crash into a success, which is the exact bug the fleet-evidence
// rule exists to forbid. weaveTerminalState must still refuse.
func TestPreservingACrashNeverPromotesItToSubmitted(t *testing.T) {
	withWork := weaveTerminalEvidence{CommitsAhead: 3}

	if got := weaveTerminalState(1, nil, "", withWork); got != "failed" {
		t.Errorf("non-zero exit WITH preserved commits = %q, want failed — "+
			"preserving work must not assert success on it", got)
	}
	if got := weaveTerminalState(0, nil, "watchdog", withWork); got != "killed" {
		t.Errorf("killed WITH preserved commits = %q, want killed", got)
	}
	// Only a clean exit with real commits earns submitted. Both halves required.
	if got := weaveTerminalState(0, nil, "", withWork); got != "submitted" {
		t.Errorf("clean exit with commits = %q, want submitted", got)
	}
	if got := weaveTerminalState(0, nil, "", weaveTerminalEvidence{}); got != "no-op" {
		t.Errorf("clean exit with NO commits = %q, want no-op", got)
	}
}
