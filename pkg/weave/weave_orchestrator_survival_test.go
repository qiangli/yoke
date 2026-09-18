package weave

import (
	"os"
	"testing"
	"time"
)

// These tests encode the invariant the orchestrator/worker split kept violating:
// `killed` must mean the WORKER actually died, never that the orchestrator that
// launched it exited, restarted, or was killed. A restarting orchestrator runs
// the reaper against the durable queue; that reconciliation must adopt a live
// worker (leave it `working`), and even a genuinely dead worker becomes `failed`,
// never `killed` — killed is reserved for a signal death the wrapper itself
// recorded.

// TestReaperAdoptsLiveWorkerOnOrchestratorRestart simulates an orchestrator
// restart (the reaper runs on every autopilot/heartbeat tick) while a worker is
// still alive. The run must stay `working` — the orchestrator's death is not the
// worker's death.
func TestReaperAdoptsLiveWorkerOnOrchestratorRestart(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{
		ID:         1,
		Title:      "worker still producing",
		State:      "working",
		WrapperPid: os.Getpid(), // guaranteed alive: stands in for a live worker
		StartedAt:  time.Now().UTC(),
	}}}

	actions := weaveReapPass(q, "", "", time.Now().UTC())

	if got := q.Items[0].State; got != "working" {
		t.Fatalf("reaper moved a live-wrapper run to %q; a running worker must survive an orchestrator restart", got)
	}
	for _, a := range actions {
		if a.To == "killed" {
			t.Fatalf("reaper fabricated a killed state on restart: %+v", a)
		}
	}
}

// TestReaperDeadWorkerIsFailedNotKilled: even when the worker really is gone,
// the reaper's terminal outcome is `failed` (with the cause named and any
// committed work surfaced as salvageable) — never `killed`. Only a signal death
// the wrapper recorded is a kill.
func TestReaperDeadWorkerIsFailedNotKilled(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{
		ID:         2,
		Title:      "worker process gone",
		State:      "working",
		WrapperPid: deadPID(t),
		StartedAt:  time.Now().Add(-time.Hour),
	}}}

	weaveReapPass(q, "", "", time.Now().UTC())

	switch q.Items[0].State {
	case "failed":
		// correct
	case "killed":
		t.Fatal("reaper marked a dead-wrapper run `killed`; killed must require an actual signal death the wrapper recorded, not an inferred orchestrator-side reap")
	default:
		t.Fatalf("dead-wrapper run => %q, want failed", q.Items[0].State)
	}
}

// TestTerminalStateKilledRequiresWorkerDeath pins the decision function the
// worker uses to write its own terminal state: `killed` iff the wrapper recorded
// a signal death (killedBy) or the process was signalled (exit >= 129). A clean
// exit with commits is `submitted`; nothing about the orchestrator disappearing
// can turn either into `killed`.
func TestTerminalStateKilledRequiresWorkerDeath(t *testing.T) {
	withCommits := weaveTerminalEvidence{CommitsAhead: 2, Head: "abcdef012345"}

	if got := weaveTerminalState(0, nil, "", withCommits); got != "submitted" {
		t.Fatalf("clean exit + commits => %q, want submitted", got)
	}
	if got := weaveTerminalState(0, nil, "", withCommits); got == "killed" {
		t.Fatal("a clean worker exit must never be `killed` — the orchestrator dying must not fabricate a kill")
	}
	if got := weaveTerminalState(0, nil, "signal hangup forwarded from wrapper", withCommits); got != "killed" {
		t.Fatalf("a wrapper-recorded signal death => %q, want killed", got)
	}
	if got := weaveTerminalState(143, nil, "", withCommits); got != "killed" {
		t.Fatalf("exit 143 (128+SIGTERM) => %q, want killed", got)
	}
	if got := weaveTerminalState(1, nil, "", weaveTerminalEvidence{}); got != "failed" {
		t.Fatalf("non-zero exit, no kill, no commits => %q, want failed", got)
	}
}
