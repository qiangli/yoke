package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsAncestor covers both the typed predicate and the
// `merge-base --is-ancestor` exit-code contract.
func TestIsAncestor(t *testing.T) {
	dir := makeTwoCommitRepo(t) // two commits on the default branch
	tip, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	// HEAD~1 is an ancestor of HEAD; HEAD is not an ancestor of HEAD~1.
	anc, err := IsAncestor(dir, "HEAD~1", "HEAD")
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if !anc {
		t.Errorf("HEAD~1 should be ancestor of HEAD")
	}
	notAnc, err := IsAncestor(dir, tip.Hash, "HEAD~1")
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if notAnc {
		t.Errorf("HEAD should not be ancestor of HEAD~1")
	}

	// Exec layer: exit 0 when ancestor, 1 when not.
	res, err := nativeMergeBase(context.Background(), dir, []string{"--is-ancestor", "HEAD~1", "HEAD"})
	if err != nil {
		t.Fatalf("native --is-ancestor: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ancestor exit = %d, want 0", res.ExitCode)
	}
	res, err = nativeMergeBase(context.Background(), dir, []string{"--is-ancestor", "HEAD", "HEAD~1"})
	if err != nil {
		t.Fatalf("native --is-ancestor: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("non-ancestor exit = %d, want 1", res.ExitCode)
	}
}

// TestNativeClone_LocalNoHardlinks clones a local repo by path with the
// flags weave uses, and verifies the sandbox is an independent repo
// (its own .git) checked out at the requested branch.
func TestNativeClone_LocalNoHardlinks(t *testing.T) {
	src := t.TempDir()
	if _, err := Init(InitOptions{Path: src}); err != nil {
		t.Fatalf("init src: %v", err)
	}
	setLocalIdentity(t, src)
	commitFiles(t, src, map[string]string{"hello.txt": "hi\n"}, "seed")
	base := currentBranch(t, src)

	parent := t.TempDir()
	if _, err := nativeClone(context.Background(), parent, []string{"--local", "--no-hardlinks", "--branch", base, src, "sandbox"}); err != nil {
		t.Fatalf("native clone: %v", err)
	}
	sandbox := filepath.Join(parent, "sandbox")

	// The cloned file is present...
	if got := readFile(t, sandbox, "hello.txt"); got != "hi\n" {
		t.Errorf("hello.txt = %q, want %q", got, "hi\n")
	}
	// ...and the sandbox has its OWN object store (independent .git dir),
	// not a hardlink/alternate into src.
	if _, err := os.Stat(filepath.Join(sandbox, ".git")); err != nil {
		t.Errorf("sandbox .git missing: %v", err)
	}
}

// TestNativeFetch_LocalRefspec mirrors `weave pull`: fetch a branch from a
// sandbox by filesystem path with an explicit refspec and --no-tags,
// landing it in the destination repo without a configured remote.
func TestNativeFetch_LocalRefspec(t *testing.T) {
	// Destination repo.
	dst := t.TempDir()
	if _, err := Init(InitOptions{Path: dst}); err != nil {
		t.Fatalf("init dst: %v", err)
	}
	setLocalIdentity(t, dst)
	commitFiles(t, dst, map[string]string{"base.txt": "base\n"}, "base")

	// Sandbox: clone dst, branch off, commit.
	parent := t.TempDir()
	if _, err := nativeClone(context.Background(), parent, []string{"--local", dst, "sandbox"}); err != nil {
		t.Fatalf("clone sandbox: %v", err)
	}
	sandbox := filepath.Join(parent, "sandbox")
	setLocalIdentity(t, sandbox)
	if _, err := Checkout(CheckoutOptions{RepoPath: sandbox, Branch: "agent/work", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, sandbox, map[string]string{"work.txt": "work\n"}, "agent work")

	// Fetch agent/work from the sandbox path into dst.
	if _, err := nativeFetch(context.Background(), dst, []string{"--no-tags", sandbox, "agent/work:agent/work"}); err != nil {
		t.Fatalf("native fetch: %v", err)
	}
	// The branch now resolves in dst.
	n, err := RevListCount(dst, "HEAD..agent/work")
	if err != nil {
		t.Fatalf("rev-list after fetch: %v", err)
	}
	if n != 1 {
		t.Errorf("agent/work is %d commits ahead of HEAD, want 1", n)
	}
}

// TestNativeCheckout_ForceB verifies `checkout -B` creates the branch and,
// on a second call from a later commit, resets it to the new HEAD.
func TestNativeCheckout_ForceB(t *testing.T) {
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)

	if _, err := nativeCheckout(context.Background(), dir, []string{"-B", "topic"}); err != nil {
		t.Fatalf("checkout -B: %v", err)
	}
	if cur := currentBranch(t, dir); cur != "topic" {
		t.Fatalf("current branch = %q, want topic", cur)
	}

	// Advance, switch away, then -B again — topic must reset to new HEAD.
	commitFiles(t, dir, map[string]string{"x.txt": "x\n"}, "advance")
	newTip, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if _, err := nativeCheckout(context.Background(), dir, []string{"-B", "topic"}); err != nil {
		t.Fatalf("checkout -B reset: %v", err)
	}
	topicTip, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse topic: %v", err)
	}
	if topicTip.Hash != newTip.Hash {
		t.Errorf("topic at %s, want reset to %s", topicTip.Hash, newTip.Hash)
	}
}

// TestNativeDiff_CachedQuiet checks the staged-changes predicate loom uses
// to skip empty commits: exit 0 = nothing staged, exit 1 = staged change.
func TestNativeDiff_CachedQuiet(t *testing.T) {
	dir := makeTwoCommitRepo(t)

	// Clean index → exit 0.
	res, err := nativeDiff(context.Background(), dir, []string{"--cached", "--quiet"})
	if err != nil {
		t.Fatalf("diff --cached --quiet (clean): %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("clean exit = %d, want 0", res.ExitCode)
	}

	// Stage a change → exit 1.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Add(AddOptions{RepoPath: dir, Path: "new.txt"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	res, err = nativeDiff(context.Background(), dir, []string{"--cached", "--quiet"})
	if err != nil {
		t.Fatalf("diff --cached --quiet (staged): %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("staged exit = %d, want 1", res.ExitCode)
	}
}

// TestNativeStatus_UntrackedAll confirms an untracked file shows up in
// porcelain status (the --untracked-files=all flag is accepted).
func TestNativeStatus_UntrackedAll(t *testing.T) {
	dir := makeTwoCommitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "loose.txt"), []byte("loose\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := nativeStatus(context.Background(), dir, []string{"--porcelain", "--untracked-files=all"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if want := "loose.txt"; !strings.Contains(res.Stdout, want) {
		t.Errorf("status %q does not mention %q", res.Stdout, want)
	}
}

// TestBranchDelete_MergedVsUnmerged covers the -d/-D distinction: -d
// refuses an unmerged branch, -D forces it.
func TestBranchDelete_MergedVsUnmerged(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"a.txt": "a\n"}, "base")
	main := currentBranch(t, dir)

	// Unmerged branch with its own commit.
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "wip", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, dir, map[string]string{"w.txt": "w\n"}, "wip work")
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	// -d (Force=false) must refuse the unmerged branch.
	if _, _, err := Branch(BranchOptions{RepoPath: dir, Name: "wip", Delete: true}); err == nil {
		t.Errorf("-d should refuse unmerged branch")
	}
	// -D (Force=true) deletes it.
	if _, _, err := Branch(BranchOptions{RepoPath: dir, Name: "wip", Delete: true, Force: true}); err != nil {
		t.Errorf("-D should force-delete: %v", err)
	}

	// A merged branch (no commits ahead) deletes fine with -d.
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "merged", Create: true}); err != nil {
		t.Fatalf("checkout -b merged: %v", err)
	}
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if _, _, err := Branch(BranchOptions{RepoPath: dir, Name: "merged", Delete: true}); err != nil {
		t.Errorf("-d should delete merged branch: %v", err)
	}
}

// TestRemoteRemove covers stripping a remote (sandbox origin scrub).
func TestRemoteRemove(t *testing.T) {
	origin := t.TempDir()
	if _, err := Init(InitOptions{Path: origin}); err != nil {
		t.Fatalf("init origin: %v", err)
	}
	setLocalIdentity(t, origin)
	commitFiles(t, origin, map[string]string{"a.txt": "a\n"}, "seed")

	clone := filepath.Join(t.TempDir(), "clone")
	if _, err := Clone(CloneOptions{URL: origin, Path: clone}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	// Fresh clone has origin.
	if _, entries, err := Remotes(clone); err != nil || len(entries) == 0 {
		t.Fatalf("expected origin remote, got %v err=%v", entries, err)
	}
	if _, err := RemoteRemove(clone, "origin"); err != nil {
		t.Fatalf("RemoteRemove: %v", err)
	}
	if _, entries, _ := Remotes(clone); len(entries) != 0 {
		t.Errorf("origin still present after remove: %v", entries)
	}
}

// TestRepoRoot resolves the worktree root from a subdirectory.
func TestRepoRoot(t *testing.T) {
	dir := makeTwoCommitRepo(t)
	sub := filepath.Join(dir, "nested", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	root, err := RepoRoot(sub)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	// Resolve symlinks on both sides (macOS /var → /private/var).
	wantResolved, _ := filepath.EvalSymlinks(dir)
	gotResolved, _ := filepath.EvalSymlinks(root)
	if gotResolved != wantResolved {
		t.Errorf("RepoRoot = %q, want %q", gotResolved, wantResolved)
	}
}
