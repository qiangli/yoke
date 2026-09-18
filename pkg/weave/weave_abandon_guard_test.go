package weave

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// `weave abandon` must not decide what is expendable from a STATE LABEL —
// the exact bug `weave prune` was already fixed for. An agent branch lives
// ONLY inside its workspace clone until `weave pull` fetches it, so tearing
// down the workspace does not leave its commits "unmerged", it DESTROYS
// them. `weave abandon --yes` used to do exactly that to a run with a
// finished, committed feature; the only thing that saved it was a manually
// preserved ref, a step nothing enforced.
//
// setupAbandonGuardFixture builds a user repo (root) and a workspace clone
// with one commit on agent/weave-issue-1 that is NOT yet in root — the
// commit that a bare `abandon` would otherwise destroy.
func setupAbandonGuardFixture(t *testing.T) (root, workspace, sha string) {
	t.Helper()
	root = setupIsolationFixture(t)
	workspace = t.TempDir()
	gitT(t, workspace, "clone", "-q", root, ".")
	gitT(t, workspace, "checkout", "-q", "-b", "agent/weave-issue-1")
	gitT(t, workspace, "commit", "--allow-empty", "-qm", "agent work")
	sha = gitT(t, workspace, "rev-parse", "HEAD")
	return root, workspace, sha
}

func TestWeaveAbandonRefusesUnmergedCommitsWithoutForce(t *testing.T) {
	root, workspace, _ := setupAbandonGuardFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "failed", Workspace: workspace, Branch: "agent/weave-issue-1", CommitsAhead: 1,
	}}}); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "abandon", "1", "--yes", "--json")
	if code == 0 {
		t.Fatalf("abandon of a run holding an unmerged commit must be refused, got exit 0: %s", out)
	}
	if !strings.Contains(out, "unmerged") {
		t.Fatalf("refusal must say why: %s", out)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("a refused abandon must leave the workspace (and its only-copy commit) intact: %v", err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil || it.State != "failed" || it.Workspace == "" {
		t.Fatalf("a refused abandon must not mutate the item: %#v", it)
	}
}

func TestWeaveAbandonForcePreservesRefBeforeDestroying(t *testing.T) {
	root, workspace, sha := setupAbandonGuardFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "failed", Workspace: workspace, Branch: "agent/weave-issue-1", CommitsAhead: 1,
	}}}); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "abandon", "1", "--yes", "--force", "--json")
	if code != 0 {
		t.Fatalf("--force abandon must succeed: exit=%d output=%s", code, out)
	}
	if !strings.Contains(out, "refs/salvage/abandoned-1") {
		t.Fatalf("output must report the ref the commit was preserved under: %s", out)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("--force must still destroy the workspace: %v", err)
	}
	got := gitT(t, root, "rev-parse", "refs/salvage/abandoned-1")
	if got != sha {
		t.Fatalf("preserved ref must point at the commit that would otherwise be lost: got %s want %s", got, sha)
	}
}

// A killed run may already have left the legacy salvage ref behind before a
// resumed worker amends its WIP commit. Reusing the fixed ref makes git fetch
// reject that non-fast-forward update and strands the workspace. The older tip
// must remain recoverable while abandon preserves the resumed tip separately.
func TestWeaveAbandonForcePreservesAmendedTipAlongsideOlderSalvage(t *testing.T) {
	root, workspace, oldSHA := setupAbandonGuardFixture(t)
	t.Chdir(root)
	gitT(t, root, "fetch", "-q", workspace, "agent/weave-issue-1:refs/salvage/abandoned-1")
	gitT(t, workspace, "commit", "--allow-empty", "--amend", "-qm", "resumed WIP")
	newSHA := gitT(t, workspace, "rev-parse", "HEAD")
	if newSHA == oldSHA {
		t.Fatal("fixture did not create an amended WIP tip")
	}

	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "killed", Workspace: workspace, Branch: "agent/weave-issue-1", CommitsAhead: 1,
	}}}); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "abandon", "1", "--yes", "--force", "--json")
	if code != 0 {
		t.Fatalf("--force abandon must preserve both tips and succeed: exit=%d output=%s", code, out)
	}
	newRef := fmt.Sprintf("refs/salvage/abandoned-1-%s", newSHA)
	if !strings.Contains(out, newRef) {
		t.Fatalf("output must report the SHA-qualified preserved ref: %s", out)
	}
	if got := gitT(t, root, "rev-parse", "refs/salvage/abandoned-1"); got != oldSHA {
		t.Fatalf("older salvage ref was overwritten: got %s want %s", got, oldSHA)
	}
	if got := gitT(t, root, "rev-parse", newRef); got != newSHA {
		t.Fatalf("resumed WIP tip was not preserved: got %s want %s", got, newSHA)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("successful abandon must remove workspace: %v", err)
	}
}

// A run with nothing at risk (no commits ahead, clean tree) must dispose
// exactly as before the guard existed — no ref, no --force needed.
func TestWeaveAbandonDisposesCleanRunNormally(t *testing.T) {
	root := setupIsolationFixture(t)
	workspace := t.TempDir()
	gitT(t, root, "clone", "--local", "--no-hardlinks", root, workspace)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "failed", Workspace: workspace,
	}}}); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "abandon", "1", "--yes", "--json")
	if code != 0 {
		t.Fatalf("abandon of a clean run must succeed: exit=%d output=%s", code, out)
	}
	if strings.Contains(out, "refs/salvage") {
		t.Fatalf("a clean run has nothing to preserve: %s", out)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("clean abandon must still remove the workspace: %v", err)
	}
}
