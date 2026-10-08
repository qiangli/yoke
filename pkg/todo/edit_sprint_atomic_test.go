package todo

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
)

// A confined manager session (sandbox denies the sprint board — e.g. a
// managed Codex session on sandbox_mode=workspace-write whose writable roots
// cover the repo but not ~/.bashy) used to corrupt board/repo consistency:
// `todo edit --sprint` wrote the repo story file FIRST and only then took
// the board lock, so the lock failure left repo cards moved while the host
// index still said missing (Sprint 379 story 1302; the Sprint 338 story
// 05853a4f4c02 fix covered CLI-launched sessions via the YOLO default, not
// this API-managed profile path). The edit must fail BEFORE any store is
// written, and a late board failure must roll the story back.

func saveSprintSeams(t *testing.T) {
	t.Helper()
	prevHandles, prevChanged, prevPreflight := SprintHandles, SprintChanged, SprintPreflight
	t.Cleanup(func() {
		SprintHandles, SprintChanged, SprintPreflight = prevHandles, prevChanged, prevPreflight
	})
	SprintHandles, SprintChanged, SprintPreflight = nil, nil, nil
}

func runEditSprint(t *testing.T, sf storeFunc, id, sprint string) error {
	t.Helper()
	cmd := newEditCmd(sf)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{id, "--sprint", sprint})
	return cmd.Execute()
}

// The board preflight trips before the story file is touched: no partial
// save, and the board reconciler never runs.
func TestEditSprintPreflightFailureSavesNothing(t *testing.T) {
	saveSprintSeams(t)
	st := RepoStore(t.TempDir())
	sf := func() (*issue.Store, string, error) { return st, "repo", nil }
	it, err := Add(st, "story to move", "", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}

	denied := errors.New(`lockfile: open /home/op/.bashy/sprint/queue.lock: operation not permitted`)
	var preflights, reconciles int
	SprintPreflight = func() error { preflights++; return denied }
	SprintChanged = func(previous *issue.Issue) error { reconciles++; return nil }

	if err := runEditSprint(t, sf, it.ID, "345"); err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("edit under a denied board = %v, want the denial before any write", err)
	}
	if preflights != 1 {
		t.Fatalf("preflights = %d, want exactly 1", preflights)
	}
	if reconciles != 0 {
		t.Fatalf("reconciles = %d, want 0 (never reached)", reconciles)
	}
	back, err := ResolveRef(st, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Sprint != 0 || back.Title != "story to move" {
		t.Fatalf("denied edit mutated the story: %+v", back)
	}
}

// The board reconcile runs after the story save; when it fails the story
// must be rolled back rather than left moved with the board missing it.
func TestEditSprintChangedFailureRollsBackStory(t *testing.T) {
	saveSprintSeams(t)
	st := RepoStore(t.TempDir())
	sf := func() (*issue.Store, string, error) { return st, "repo", nil }
	it, err := Add(st, "story to move", "", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}

	var preflights int
	SprintPreflight = func() error { preflights++; return nil }
	SprintChanged = func(previous *issue.Issue) error {
		return errors.New("queue lock: operation not permitted")
	}

	if err := runEditSprint(t, sf, it.ID, "345"); err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("edit with a failing board reconcile = %v, want an error", err)
	}
	if preflights != 1 {
		t.Fatalf("preflights = %d, want exactly 1 (before the save)", preflights)
	}
	back, err := ResolveRef(st, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Sprint != 0 || back.Title != "story to move" {
		t.Fatalf("failed reconcile left a partial move: %+v", back)
	}
}

// The ordinary move still reconciles exactly once, preflight before save
// before reconcile.
func TestEditSprintHappyPathReconcilesBoardOnce(t *testing.T) {
	saveSprintSeams(t)
	st := RepoStore(t.TempDir())
	sf := func() (*issue.Store, string, error) { return st, "repo", nil }
	it, err := Add(st, "story to move", "", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}

	var order []string
	SprintPreflight = func() error { order = append(order, "preflight"); return nil }
	SprintChanged = func(previous *issue.Issue) error {
		order = append(order, "reconcile")
		if previous.Sprint != 0 {
			t.Errorf("reconcile saw previous.Sprint = %d, want 0", previous.Sprint)
		}
		return nil
	}

	if err := runEditSprint(t, sf, it.ID, "345"); err != nil {
		t.Fatalf("ordinary move: %v", err)
	}
	if len(order) != 2 || order[0] != "preflight" || order[1] != "reconcile" {
		t.Fatalf("call order = %v, want [preflight reconcile]", order)
	}
	back, err := ResolveRef(st, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Sprint != 345 {
		t.Fatalf("moved story sprint = %d, want 345", back.Sprint)
	}
}
