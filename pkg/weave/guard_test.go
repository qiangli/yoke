package weave

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeQueue lays down a queue.json the way weave does, so the guard is tested
// against the real on-disk shape rather than a hand-built struct.
func writeQueue(t *testing.T, stateRoot, tag, repoRoot string, items []*weaveItem) {
	t.Helper()
	dir := filepath.Join(stateRoot, tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	q := weaveQueue{NextID: int64(len(items) + 1), Root: repoRoot, Items: items}
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHoldersOf_ReportsRunningRuns(t *testing.T) {
	state, repo := t.TempDir(), t.TempDir()
	writeQueue(t, state, "repo-aaa", repo, []*weaveItem{
		{ID: 35, State: "working", Title: "wire real job carriers", Tool: "claude"},
	})

	got := HoldersOf(repo, HoldersQuery{StateRoot: state})
	if len(got) != 1 {
		t.Fatalf("holders = %d, want 1", len(got))
	}
	if got[0].ID != 35 || got[0].State != "working" {
		t.Fatalf("holder = %+v", got[0])
	}
}

// The noise floor is a design decision, not an oversight: a submitted run can
// sit unmerged for days, so warning on it would fire on nearly every commit.
func TestHoldersOf_SubmittedIsStrictOnly(t *testing.T) {
	state, repo := t.TempDir(), t.TempDir()
	writeQueue(t, state, "repo-aaa", repo, []*weaveItem{
		{ID: 28, State: "submitted"},
	})

	if got := HoldersOf(repo, HoldersQuery{StateRoot: state}); len(got) != 0 {
		t.Fatalf("default must not warn on a submitted run, got %d", len(got))
	}
	if got := HoldersOf(repo, HoldersQuery{StateRoot: state, Strict: true}); len(got) != 1 {
		t.Fatalf("strict must report the submitted run, got %d", len(got))
	}
}

// Terminal states hold nothing — their workspaces are done with.
func TestHoldersOf_IgnoresTerminalStates(t *testing.T) {
	state, repo := t.TempDir(), t.TempDir()
	writeQueue(t, state, "repo-aaa", repo, []*weaveItem{
		{ID: 1, State: "done"}, {ID: 2, State: "abandoned"},
		{ID: 3, State: "failed"}, {ID: 4, State: "killed"}, {ID: 5, State: "todo"},
	})
	if got := HoldersOf(repo, HoldersQuery{StateRoot: state, Strict: true}); len(got) != 0 {
		t.Fatalf("terminal and unstarted states hold nothing, got %+v", got)
	}
}

// A queue serving a DIFFERENT repo must never warn here. Cross-repo false
// positives are exactly what gets a guard disabled.
func TestHoldersOf_IgnoresOtherRepos(t *testing.T) {
	state, mine, theirs := t.TempDir(), t.TempDir(), t.TempDir()
	writeQueue(t, state, "repo-mine", mine, []*weaveItem{{ID: 1, State: "working"}})
	writeQueue(t, state, "repo-theirs", theirs, []*weaveItem{{ID: 2, State: "working"}})

	got := HoldersOf(mine, HoldersQuery{StateRoot: state})
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("only this repo's runs may be reported, got %+v", got)
	}
}

// THE SILENT-NO-OP GUARD. On macOS a temp dir is /var/... which resolves to
// /private/var/..., so an unnormalised compare makes one repo look like two and
// the guard reports all-clear forever — indistinguishable from "nothing is
// wrong", which is the worst failure available to it.
func TestHoldersOf_NormalisesSymlinkedPaths(t *testing.T) {
	state, real := t.TempDir(), t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Queue records the REAL path; the caller arrives via the symlink.
	writeQueue(t, state, "repo-aaa", real, []*weaveItem{{ID: 7, State: "working"}})

	if got := HoldersOf(link, HoldersQuery{StateRoot: state}); len(got) != 1 {
		t.Fatalf("a symlinked path must resolve to the same repo, got %d holders", len(got))
	}
}

// A guard that errors because an optional store is missing turns an advisory
// into an obstacle. Absent state is silence, not failure.
func TestHoldersOf_AbsentStateIsSilent(t *testing.T) {
	if got := HoldersOf(t.TempDir(), HoldersQuery{StateRoot: filepath.Join(t.TempDir(), "nope")}); got != nil {
		t.Fatalf("absent state root must yield no holders, got %+v", got)
	}
	if got := HoldersOf("", HoldersQuery{StateRoot: t.TempDir()}); got != nil {
		t.Fatalf("empty repo root must yield no holders, got %+v", got)
	}
}

func TestRepoRootOf(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := RepoRootOf(nested); got != root {
		t.Fatalf("RepoRootOf(%q) = %q, want %q", nested, got, root)
	}
	if got := RepoRootOf(t.TempDir()); got != "" {
		t.Fatalf("outside a repo must be empty, got %q", got)
	}
}
