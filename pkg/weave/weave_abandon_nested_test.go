package weave

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	nestedMidPath  = "external/sub1"
	nestedLeafPath = "external/sub1/vendor/sub2"
	nestedMidRef   = "refs/salvage/abandoned-1-sub-external-sub1"
	nestedLeafRef  = "refs/salvage/abandoned-1-sub-external-sub1-vendor-sub2"
)

// setupAbandonNestedFixture builds a weave root whose tree carries a gitlink
// (external/sub1) with its own nested gitlink (vendor/sub2), then the run-1
// workspace clone of it on agent/weave-issue-1. The workspace is left
// UNHYDRATED so each test chooses how far hydration got.
func setupAbandonNestedFixture(t *testing.T) (root, workspace string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root = setupIsolationFixture(t)
	allow := []string{"-c", "protocol.file.allow=always"}
	sub := func(args ...string) []string { return append(append([]string{}, allow...), args...) }

	sub2 := t.TempDir()
	gitT(t, sub2, "init", "-q", "-b", "main")
	mustWrite(t, filepath.Join(sub2, "nested.txt"), "nested\n")
	gitT(t, sub2, "add", ".")
	gitT(t, sub2, "commit", "-qm", "init sub2")

	sub1 := t.TempDir()
	gitT(t, sub1, "init", "-q", "-b", "main")
	mustWrite(t, filepath.Join(sub1, "mid.txt"), "mid\n")
	gitT(t, sub1, "add", ".")
	gitT(t, sub1, "commit", "-qm", "init sub1")
	gitT(t, sub1, sub("submodule", "add", sub2, "vendor/sub2")...)
	gitT(t, sub1, "commit", "-qm", "add sub2")

	gitT(t, root, sub("submodule", "add", sub1, nestedMidPath)...)
	gitT(t, root, "commit", "-qm", "add sub1")
	gitT(t, root, sub("submodule", "update", "--init", "--recursive")...)

	dir, _ := weaveQueueDir(root)
	workspace = filepath.Join(dir, "workspaces", "issue-1")
	mustMkdir(t, workspace)
	gitT(t, workspace, "clone", "-q", root, ".")
	gitT(t, workspace, "checkout", "-q", "-b", "agent/weave-issue-1")
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "failed", Workspace: workspace, Branch: "agent/weave-issue-1",
	}}}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	return root, workspace
}

func assertAbandonedAndGone(t *testing.T, root, workspace string) {
	t.Helper()
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("--force abandon must tear the workspace down once work is preserved: %v", err)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil || it.State != "abandoned" {
		t.Fatalf("run must end abandoned: %#v", it)
	}
	if it.SalvageRef == "" {
		t.Fatalf("abandoned run must record the superproject salvage ref: %#v", it)
	}
}

// Work that lives only inside a submodule (gitlink unchanged in the
// superproject) must survive `weave abandon --force` through the real handler:
// nested commits become salvage refs in root before the workspace is removed.
func TestWeaveAbandonForcePreservesNestedOnlyDirtyWork(t *testing.T) {
	root, workspace := setupAbandonNestedFixture(t)
	if err := weaveHydrateSubmodules(root, workspace, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(workspace, filepath.FromSlash(nestedLeafPath))
	mid := filepath.Join(workspace, filepath.FromSlash(nestedMidPath))
	mustWrite(t, filepath.Join(leaf, "nested.txt"), "edited nested\n")
	mustWrite(t, filepath.Join(leaf, "new.txt"), "untracked nested\n")
	mustWrite(t, filepath.Join(mid, "mid-new.txt"), "mid work\n")
	if got := gitT(t, workspace, "diff", "--name-only"); got != "" && got != nestedMidPath {
		t.Fatalf("precondition: superproject must see only the gitlink, got %q", got)
	}

	out, code := runWeave(t, "abandon", "1", "--yes", "--json")
	if code == 0 {
		t.Fatalf("abandon without --force must refuse nested-only dirty work: %s", out)
	}
	if _, err := os.Stat(filepath.Join(leaf, "new.txt")); err != nil {
		t.Fatalf("a refused abandon must leave nested work intact: %v", err)
	}

	out, code = runWeave(t, "abandon", "1", "--yes", "--force", "--json")
	if code != 0 {
		t.Fatalf("--force abandon must succeed: exit=%d output=%s", code, out)
	}
	for _, ref := range []string{nestedMidRef, nestedLeafRef} {
		if !strings.Contains(out, ref) {
			t.Fatalf("output must report %s: %s", ref, out)
		}
	}
	if got := gitT(t, root, "show", nestedLeafRef+":new.txt"); got != "untracked nested" {
		t.Fatalf("untracked nested file not recoverable: %q", got)
	}
	if got := gitT(t, root, "show", nestedLeafRef+":nested.txt"); got != "edited nested" {
		t.Fatalf("modified nested file not recoverable: %q", got)
	}
	if got := gitT(t, root, "show", nestedMidRef+":mid-new.txt"); got != "mid work" {
		t.Fatalf("mid-level untracked file not recoverable: %q", got)
	}
	leafTip := gitT(t, root, "rev-parse", nestedLeafRef)
	if got := gitT(t, root, "rev-parse", nestedMidRef+":vendor/sub2"); got != leafTip {
		t.Fatalf("mid salvage must record the preserved leaf tip: got %s want %s", got, leafTip)
	}
	if got := gitT(t, root, "rev-parse", "refs/salvage/abandoned-1:"+nestedMidPath); got != gitT(t, root, "rev-parse", nestedMidRef) {
		t.Fatalf("superproject salvage must record the preserved mid tip, got %s", got)
	}
	assertAbandonedAndGone(t, root, workspace)
}

// An interrupted hydration leaves the deeper submodule as an empty directory.
// That is no work and must not block or fail abandon, while the populated level
// above it (and a superproject edit) is still preserved.
func TestWeaveAbandonForceInterruptedHydrationPreservesPopulatedWork(t *testing.T) {
	root, workspace := setupAbandonNestedFixture(t)
	gitT(t, workspace, "-c", "protocol.file.allow=always", "submodule", "update", "--init", nestedMidPath)
	mid := filepath.Join(workspace, filepath.FromSlash(nestedMidPath))
	if entries, err := os.ReadDir(filepath.Join(mid, "vendor", "sub2")); err != nil || len(entries) != 0 {
		t.Fatalf("precondition: nested level must be empty, entries=%v err=%v", entries, err)
	}
	mustWrite(t, filepath.Join(mid, "mid-new.txt"), "mid work\n")

	out, code := runWeave(t, "abandon", "1", "--yes", "--force", "--json")
	if code != 0 {
		t.Fatalf("interrupted hydration must not block --force abandon: exit=%d output=%s", code, out)
	}
	if got := gitT(t, root, "show", nestedMidRef+":mid-new.txt"); got != "mid work" {
		t.Fatalf("populated level's work not recoverable: %q", got)
	}
	if strings.Contains(out, nestedLeafRef) {
		t.Fatalf("an empty submodule holds nothing and must create no ref: %s", out)
	}
	if got := gitT(t, root, "for-each-ref", "--format=%(refname)", "refs/salvage/"); strings.Contains(got, "vendor-sub2") {
		t.Fatalf("unexpected leaf salvage ref: %s", got)
	}
	assertAbandonedAndGone(t, root, workspace)
}

// Only a MISSING .git means "never populated". Any other stat failure on a
// populated submodule is unknown state: abandon must refuse and keep the
// workspace rather than destroy what it cannot read.
func TestWeaveAbandonForceRefusesUnreadableSubmodule(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced on the caller")
	}
	root, workspace := setupAbandonNestedFixture(t)
	if err := weaveHydrateSubmodules(root, workspace, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	// A superproject edit keeps the dirtiness guard engaged even though git
	// itself may not be able to look inside the locked submodule.
	mustWrite(t, filepath.Join(workspace, "seed.txt"), "edited\n")
	mid := filepath.Join(workspace, filepath.FromSlash(nestedMidPath))
	if err := os.Chmod(mid, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(mid, 0o755) })

	if _, err := weavePreserveNestedWork(root, workspace, 1); err == nil || !strings.Contains(err.Error(), "stat .git") {
		t.Fatalf("unreadable .git must be an error naming the stat failure, got %v", err)
	}

	out, code := runWeave(t, "abandon", "1", "--yes", "--force", "--json")
	if code == 0 {
		t.Fatalf("--force abandon must fail closed on an unreadable submodule: %s", out)
	}
	if !strings.Contains(out, "refusing to destroy") {
		t.Fatalf("refusal must say work was not destroyed: %s", out)
	}
	_ = os.Chmod(mid, 0o755)
	if _, err := os.Stat(filepath.Join(workspace, "seed.txt")); err != nil {
		t.Fatalf("workspace must be left intact: %v", err)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if it := findWeaveItem(q, 1); it == nil || it.State != "failed" {
		t.Fatalf("a refused abandon must not mutate the item: %#v", it)
	}
}
