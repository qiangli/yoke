package weave

import (
	"os"
	"strings"
	"testing"
)

// THE BUG, AS OBSERVED LIVE (2026-11-06, Sprint 580).
//
// `bashy weave reset 3 --yes` declared no cobra Args rule, so the "3" was
// silently DISCARDED and reset did what reset does: tore down every weave for
// the project and deleted the whole state directory. A manager meaning to
// restart one failed run cleared the umbrella queue, taking two older terminal
// items and their workspaces with it.
//
// Every neighbouring verb — pull, abandon, say, start --run N — is per-run, so
// the shape of the typo is natural and the cost of it is total. Both halves are
// pinned here: the argument must be REFUSED (and nothing touched), and a
// whole-queue reset must refuse to destroy commits that live nowhere else.

// A run number must fail, name the per-run alternatives, and leave the queue
// exactly as it was.
func TestWeaveResetRejectsRunArgumentAndLeavesQueueIntact(t *testing.T) {
	root, _, dir := setupPullRefusalFixture(t, 3, "failed", 0)

	out, code := runWeave(t, "reset", "3", "--yes", "--plain")
	if code == 0 {
		t.Fatalf("reset with a run number must fail, got exit 0: %s", out)
	}
	for _, want := range []string{
		"takes no run number",
		"WHOLE queue",
		"weave abandon N",
		"weave start --resume --run N",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reset arg refusal must contain %q: %s", want, out)
		}
	}

	// Nothing may have been torn down: the state dir, the queue, and the item.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a refused reset destroyed the project state: %v", err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatalf("load queue after refused reset: %v", err)
	}
	if len(q.Items) != 1 || q.Items[0].ID != 3 {
		t.Fatalf("a refused reset mutated the queue: %+v", q.Items)
	}
	if q.Root != root {
		t.Fatalf("queue root rewritten by a refused reset: %q", q.Root)
	}
}

// An item holding commits the source checkout cannot reach must stop the
// sweep — including a terminal item, because the state label is the wrapper's
// report and the branch is git's truth. The refusal names the run and its
// count so the operator knows what is at stake.
func TestWeaveResetRefusesItemHoldingUnmergedCommits(t *testing.T) {
	_, workspace, dir := setupPullRefusalFixture(t, 7, "no-op", 2)

	out, code := runWeave(t, "reset", "--yes", "--plain")
	if code == 0 {
		t.Fatalf("reset must refuse while a run holds unmerged commits, got exit 0: %s", out)
	}
	if !strings.Contains(out, "#7 held work (2 unmerged commits") {
		t.Errorf("refusal must name the run and its commit count: %s", out)
	}
	if !strings.Contains(out, "--force") {
		t.Errorf("refusal must name the override flag: %s", out)
	}
	// --yes skips the prompt; it must not have overridden the refusal.
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("a refused reset removed the workspace holding the commits: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a refused reset destroyed the project state: %v", err)
	}
}

// --force is the "I accept the risk" escape hatch, so the same fixture must go
// all the way through.
func TestWeaveResetForceProceedsThroughHeldWork(t *testing.T) {
	_, workspace, dir := setupPullRefusalFixture(t, 7, "no-op", 2)

	out, code := runWeave(t, "reset", "--yes", "--force", "--plain")
	if code != 0 {
		t.Fatalf("reset --force exit=%d: %s", code, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("project state survived reset --force: %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace survived reset --force: %v", err)
	}
}

// The guard must not change the ordinary case: a queue whose runs hold nothing
// resets exactly as it did before, with no --force needed.
func TestWeaveResetWithNothingHeldBehavesAsBefore(t *testing.T) {
	_, workspace, dir := setupPullRefusalFixture(t, 4, "done", 0)

	out, code := runWeave(t, "reset", "--yes", "--plain")
	if code != 0 {
		t.Fatalf("reset of a queue holding nothing exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "queue cleared") {
		t.Errorf("reset must report the teardown: %s", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("project state survived reset: %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace survived reset: %v", err)
	}
}

// weaveResetHeldWork is the measurement behind the refusal; pin it directly so
// a future change to the gate cannot quietly start skipping terminal history.
func TestWeaveResetHeldWorkCoversTerminalHistory(t *testing.T) {
	root, _, dir := setupPullRefusalFixture(t, 9, "done", 3)

	held := weaveResetHeldWork(root, dir)
	if len(held) != 1 {
		t.Fatalf("held=%+v, want exactly the one run", held)
	}
	if held[0].Issue != 9 || held[0].Unmerged != 3 || held[0].State != "done" {
		t.Fatalf("held[0]=%+v, want #9 done with 3 unmerged", held[0])
	}
}
