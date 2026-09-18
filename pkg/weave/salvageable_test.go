package weave

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A run that died WITH GOOD WORK INSIDE IT must say so.
//
// weaveTerminalState is correctly conservative: `submitted` requires a zero exit
// AND commits, so a crashed wrapper can never claim success. That asymmetry is
// right. But the inverse costs real work.
//
// Observed live: an opencode worker implemented a whole feature, committed it,
// and then crashed on the way out (its storage layer blows the filename limit on
// exit). weave marked the run `failed` — with a building, tested, complete diff
// sitting on the branch, and nothing anywhere saying so. It read exactly like a
// run that achieved nothing.
//
// The obvious response to a failed run is to run it again. Doing that would have
// thrown away eight minutes of finished work and paid for it twice.
func TestSalvageableFooterNamesRunsThatDiedWithCommits(t *testing.T) {
	var b bytes.Buffer
	weavePrintSalvageableFooter(&b, []int64{3, 7})
	out := b.String()

	if !strings.Contains(out, "#3") || !strings.Contains(out, "#7") {
		t.Errorf("footer must name the runs: %q", out)
	}
	if !strings.Contains(out, "salvage") {
		t.Errorf("footer must say how to keep the work: %q", out)
	}
	// The whole point: stop the reader from re-running it.
	if !strings.Contains(strings.ToLower(out), "do not re-run") {
		t.Errorf("footer must warn against re-running — that is the loss this exists to prevent: %q", out)
	}
}

// Silence when there is nothing to salvage. A footer that always fires is a
// footer nobody reads.
func TestSalvageableFooterIsSilentWhenThereIsNothing(t *testing.T) {
	var b bytes.Buffer
	weavePrintSalvageableFooter(&b, nil)
	if b.Len() != 0 {
		t.Errorf("expected no footer, got %q", b.String())
	}
}

// The state itself must NOT be loosened. A crash is a crash: `submitted` still
// requires a clean exit AND commits. Surfacing the evidence is not the same as
// asserting success on it, and confusing the two would recreate the exact bug
// the fleet-evidence rule exists to prevent.
func TestACrashNeverClaimsSubmittedEvenWithCommits(t *testing.T) {
	ev := weaveTerminalEvidence{CommitsAhead: 5}
	if got := weaveTerminalState(1, nil, "", ev); got != "failed" {
		t.Errorf("non-zero exit with commits = %q, want failed — a crash must never claim success", got)
	}
	if got := weaveTerminalState(0, nil, "sigkill", ev); got != "killed" {
		t.Errorf("killed with commits = %q, want killed", got)
	}
	if got := weaveTerminalState(0, nil, "", ev); got != "submitted" {
		t.Errorf("clean exit with commits = %q, want submitted", got)
	}
	// And the load-bearing half: no commits means no success, whatever the exit.
	if got := weaveTerminalState(0, nil, "", weaveTerminalEvidence{CommitsAhead: 0}); got != "no-op" {
		t.Errorf("clean exit with NO commits = %q, want no-op — success may not be reached by an absence of work", got)
	}
}

func TestTerminalRunWithUnmergedCommitIsFirstClass(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")

	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "commit", "--allow-empty", "-qm", "seed")
	base := gitT(t, root, "rev-parse", "HEAD")

	makeWorkspace := func(name string, commit bool) string {
		workspace := filepath.Join(t.TempDir(), name)
		gitT(t, filepath.Dir(workspace), "clone", "-q", root, workspace)
		gitT(t, workspace, "checkout", "-q", "-b", "agent/"+name)
		if commit {
			gitT(t, workspace, "commit", "--allow-empty", "-qm", name+" work")
		}
		return workspace
	}
	salvageWorkspace := makeWorkspace("salvage", true)
	cleanWorkspace := makeWorkspace("clean", false)

	t.Chdir(root)
	resolved, err := weaveRepoRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(resolved)
	if err != nil {
		t.Fatal(err)
	}
	q := &weaveQueue{NextID: 3, Root: resolved, Items: []*weaveItem{
		{ID: 1, Title: "committed then killed", State: "killed", Workspace: salvageWorkspace, BaseSHA: base, Created: time.Now().UTC()},
		{ID: 2, Title: "clean failure", State: "failed", Workspace: cleanWorkspace, BaseSHA: base, Created: time.Now().UTC()},
	}}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "list", "--json")
	if code != 0 {
		t.Fatalf("weave list exit=%d: %s", code, out)
	}
	if !strings.Contains(out, `"id": 1`) || !strings.Contains(out, `"salvageable": true`) || !strings.Contains(out, `"unmerged_commits": 1`) {
		t.Fatalf("committed killed run was not flagged salvageable: %s", out)
	}
	cleanAt := strings.Index(out, `"id": 2`)
	if cleanAt < 0 {
		t.Fatalf("clean terminal run missing: %s", out)
	}
	if tail := out[cleanAt:]; strings.Contains(tail, `"salvageable": true`) {
		t.Fatalf("clean terminal run was falsely flagged salvageable: %s", tail)
	}

	status, code := runWeave(t, "status", "1")
	if code != 0 || !strings.Contains(status, "SALVAGEABLE") || !strings.Contains(status, "1 unmerged commit") {
		t.Fatalf("weave status did not surface salvageable work (exit=%d): %s", code, status)
	}

	// Submitted is terminal too: it remains a steward decision until the
	// commit actually lands, and then ceases to be salvageable even if the
	// queue's recorded state has not caught up yet.
	item := q.Items[0]
	item.State = "submitted"
	if salvageable, n := weaveClassifySalvageable(root, "main", item); !salvageable || n != 1 {
		t.Fatalf("submitted unmerged run = salvageable %v, commits %d; want true, 1", salvageable, n)
	}
	gitT(t, root, "fetch", "-q", salvageWorkspace, "agent/salvage:agent/salvage")
	gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge salvaged work", "agent/salvage")
	if salvageable, n := weaveClassifySalvageable(root, "main", item); salvageable || n != 0 {
		t.Fatalf("merged terminal run = salvageable %v, commits %d; want false, 0", salvageable, n)
	}
}
