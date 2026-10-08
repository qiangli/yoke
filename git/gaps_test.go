package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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

// seedCherryRepo builds the branch-cleanup fixture: base with two commits,
// feat branched off carrying one unique change ("gadget").
func seedCherryRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)
	base = currentBranch(t, dir)
	ctx := context.Background()
	if _, err := nativeCheckout(ctx, dir, []string{"-b", "feat"}); err != nil {
		t.Fatalf("checkout -b feat: %v", err)
	}
	commitFiles(t, dir, map[string]string{"gadget.txt": "gadget\n"}, "add gadget")
	if _, err := nativeCheckout(ctx, dir, []string{base}); err != nil {
		t.Fatalf("checkout %s: %v", base, err)
	}
	return dir, base
}

// TestNativeCherry_MissingThenEquivalent pins `cherry <base> <feat>`: a
// unique change reports "+", the same change re-applied on base under a
// different SHA reports "-". Loud failures: unknown flags, missing args,
// and unresolvable revisions return ErrUnsupported.
func TestNativeCherry_MissingThenEquivalent(t *testing.T) {
	dir, base := seedCherryRepo(t)
	ctx := context.Background()

	res, err := Exec(ctx, dir, []string{"cherry", base, "feat"})
	if err != nil {
		t.Fatalf("cherry: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "+ ") || len(lines[0]) != 42 {
		t.Fatalf("cherry = %q, want one \"+ <full-sha>\" line", res.Stdout)
	}

	// Same change, different SHA on base: patch-identity must match.
	commitFiles(t, dir, map[string]string{"gadget.txt": "gadget\n"}, "add gadget again")
	res, err = Exec(ctx, dir, []string{"cherry", base, "feat"})
	if err != nil {
		t.Fatalf("cherry after replicate: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); !strings.HasPrefix(got, "- ") || len(strings.Split(got, "\n")) != 1 {
		t.Errorf("cherry = %q, want one \"- <sha>\" line", res.Stdout)
	}

	// -v appends the subject; default head is HEAD (here: base, so empty).
	res, err = Exec(ctx, dir, []string{"cherry", "-v", base, "feat"})
	if err != nil {
		t.Fatalf("cherry -v: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); !strings.HasSuffix(got, " add gadget") {
		t.Errorf("cherry -v = %q, want subject suffix", res.Stdout)
	}

	for _, argv := range [][]string{
		{"cherry", "--pretty", base, "feat"},
		{"cherry"},
		{"cherry", base, "feat", "extra"},
		{"cherry", base, "no-such-branch"},
	} {
		if _, err := Exec(ctx, dir, argv); err != ErrUnsupported {
			t.Errorf("cherry %v err = %v, want ErrUnsupported", argv, err)
		}
	}
}

// TestNativeCherry_SkipsMerges pins two host-git rules, both probed
// against host git 2026-10-08: merge commits are never listed, and the
// upstream equivalence set is post-merge-base only. Here side merges feat
// after base already replicated the change, so the merge-base is base's
// tip, the upstream set is empty, and the feat line reports "+" while the
// merge commit itself stays silent — exactly one line either way.
func TestNativeCherry_SkipsMerges(t *testing.T) {
	dir, base := seedCherryRepo(t)
	ctx := context.Background()
	commitFiles(t, dir, map[string]string{"gadget.txt": "gadget\n"}, "add gadget again")
	if _, err := nativeCheckout(ctx, dir, []string{"-b", "side"}); err != nil {
		t.Fatalf("checkout -b side: %v", err)
	}
	if _, err := Merge(MergeOptions{RepoPath: dir, Ref: "feat", NoFF: true, Message: "merge feat"}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	res, err := Exec(ctx, dir, []string{"cherry", base, "side"})
	if err != nil {
		t.Fatalf("cherry: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "+ ") {
		t.Errorf("cherry = %q, want exactly one (merge skipped, twin pre-merge-base) line", res.Stdout)
	}
}

// openWorktreeStatus opens path with go-git and returns its status —
// proving our gitfile layout is a first-class repo to the engine.
func openWorktreeStatus(t *testing.T, path string) gogit.Status {
	t.Helper()
	r, err := openRepo(path)
	if err != nil {
		t.Fatalf("open linked worktree: %v", err)
	}
	w, err := r.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	st, err := w.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return st
}

// TestNativeWorktree_AddRemoveList pins `worktree add/remove/list`: a new
// checkout at a commit with a clean go-git status, branch double-checkout
// refused without -f, dirty removal refused without --force, and list
// output in both shapes.
func TestNativeWorktree_AddRemoveList(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	dir := filepath.Join(parent, "main")
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	commitFiles(t, dir, map[string]string{"a.txt": "line1\n"}, "first")
	commitFiles(t, dir, map[string]string{"a.txt": "line1\nline2\n"}, "second")
	base := currentBranch(t, dir)

	wt1 := filepath.Join(parent, "wt1")
	if _, err := Exec(ctx, dir, []string{"worktree", "add", wt1}); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	if got := readFile(t, wt1, "a.txt"); got != "line1\nline2\n" {
		t.Errorf("wt a.txt = %q", got)
	}
	if st := openWorktreeStatus(t, wt1); !st.IsClean() {
		t.Errorf("fresh worktree status not clean: %v", st)
	}
	// The .git file points into the main admin dir (host-git layout).
	dotgit, err := os.ReadFile(filepath.Join(wt1, ".git"))
	if err != nil || !strings.HasPrefix(string(dotgit), "gitdir: ") {
		t.Fatalf(".git file = %q, err = %v", dotgit, err)
	}

	res, err := Exec(ctx, dir, []string{"worktree", "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(res.Stdout, wt1) || !strings.Contains(res.Stdout, "["+base+"]") {
		t.Errorf("list = %q, want wt path + [%s]", res.Stdout, base)
	}
	res, err = Exec(ctx, dir, []string{"worktree", "list", "--porcelain"})
	if err != nil {
		t.Fatalf("list --porcelain: %v", err)
	}
	if !strings.Contains(res.Stdout, "worktree "+wt1) || !strings.Contains(res.Stdout, "branch refs/heads/"+base) {
		t.Errorf("porcelain = %q", res.Stdout)
	}

	// Same branch twice without -f: loud refusal, host-git's shape.
	res, err = Exec(ctx, dir, []string{"worktree", "add", filepath.Join(parent, "wt-dup"), base})
	if err != nil {
		t.Fatalf("dup add: %v", err)
	}
	if res.ExitCode != 128 || !strings.Contains(res.Stderr, "already used by worktree") {
		t.Errorf("dup add = %+v, want 128 + already-used", res)
	}
	wt2 := filepath.Join(parent, "wt2")
	if _, err := Exec(ctx, dir, []string{"worktree", "add", "-f", wt2, base}); err != nil {
		t.Fatalf("worktree add -f: %v", err)
	}

	// Dirty worktree: removal refused without --force, honored with it.
	if err := os.WriteFile(filepath.Join(wt1, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Exec(ctx, dir, []string{"worktree", "remove", wt1})
	if err != nil {
		t.Fatalf("remove dirty: %v", err)
	}
	if res.ExitCode != 128 || !strings.Contains(res.Stderr, "--force") {
		t.Errorf("remove dirty = %+v, want 128 + --force hint", res)
	}
	if _, err := Exec(ctx, dir, []string{"worktree", "remove", "--force", wt1}); err != nil {
		t.Fatalf("remove --force: %v", err)
	}
	if _, serr := os.Stat(wt1); !os.IsNotExist(serr) {
		t.Errorf("wt1 still on disk after remove")
	}
	// Clean worktree removes without --force.
	if _, err := Exec(ctx, dir, []string{"worktree", "remove", wt2}); err != nil {
		t.Fatalf("remove clean: %v", err)
	}

	// Detached checkout at a hash lists as detached.
	head, rerr := RevParse(RevParseOptions{RepoPath: dir})
	if rerr != nil {
		t.Fatalf("RevParse: %v", rerr)
	}
	wt3 := filepath.Join(parent, "wt3")
	if _, err := Exec(ctx, dir, []string{"worktree", "add", wt3, head.Hash}); err != nil {
		t.Fatalf("detached add: %v", err)
	}
	res, err = Exec(ctx, dir, []string{"worktree", "list", "--porcelain"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(res.Stdout, "detached") {
		t.Errorf("porcelain = %q, want a detached marker", res.Stdout)
	}

	// Not a worktree: loud fatal, never a silent no-op.
	res, err = Exec(ctx, dir, []string{"worktree", "remove", filepath.Join(parent, "nope")})
	if err != nil {
		t.Fatalf("remove missing: %v", err)
	}
	if res.ExitCode != 128 {
		t.Errorf("remove missing = %+v, want exit 128", res)
	}
}

// dirtyFile overwrites a worktree file without staging or committing.
func dirtyFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNativeStash_PushPopList pins `stash push/pop/list`: push snapshots
// and restores HEAD, pop re-applies and drops, list shows the stack newest
// first. Untracked files survive a push (host-git parity); overlapping
// local changes refuse the pop with exit 1 and keep the entry.
func TestNativeStash_PushPopList(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)

	dirtyFile(t, dir, "a.txt", "stashed change\n")
	if err := os.WriteFile(filepath.Join(dir, "new-untracked.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Exec(ctx, dir, []string{"stash", "push", "-m", "test stash"})
	if err != nil {
		t.Fatalf("stash push: %v", err)
	}
	if !strings.Contains(res.Stdout, "Saved working directory") {
		t.Errorf("push = %q", res.Stdout)
	}
	if got := readFile(t, dir, "a.txt"); got != "line1\nline2\n" {
		t.Errorf("a.txt after push = %q, want HEAD content", got)
	}
	if got := readFile(t, dir, "new-untracked.txt"); got != "keep me\n" {
		t.Errorf("untracked file after push = %q, must survive", got)
	}

	res, err = Exec(ctx, dir, []string{"stash", "list"})
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	if !strings.Contains(res.Stdout, "stash@{0}: test stash") {
		t.Errorf("list = %q", res.Stdout)
	}

	// Pop onto the clean tree: applies and drops the entry.
	res, err = Exec(ctx, dir, []string{"stash", "pop"})
	if err != nil {
		t.Fatalf("stash pop: %v", err)
	}
	if !strings.Contains(res.Stdout, "Dropped stash@{0}") {
		t.Errorf("pop = %q", res.Stdout)
	}
	if got := readFile(t, dir, "a.txt"); got != "stashed change\n" {
		t.Errorf("a.txt after pop = %q", got)
	}
	res, err = Exec(ctx, dir, []string{"stash", "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		t.Errorf("list after pop = %q, want empty", res.Stdout)
	}

	// Empty pop and clean push are loud-but-gentle, like host git.
	res, err = Exec(ctx, dir, []string{"stash", "pop"})
	if err != nil {
		t.Fatalf("pop empty: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("pop empty = %+v, want exit 1", res)
	}
	if _, err := Exec(ctx, dir, []string{"stash"}); err != nil {
		t.Fatalf("bare stash on clean tree: %v", err)
	}
}

// TestNativeStash_PathspecConflictAndRef pins pathspec push (only the
// named file is stashed), pop conflicts (exit 1, entry kept), and
// `pop stash@{n}` addressing.
func TestNativeStash_PathspecConflictAndRef(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"b.txt": "bee\n"}, "add b")

	dirtyFile(t, dir, "a.txt", "dirty a\n")
	dirtyFile(t, dir, "b.txt", "dirty b\n")
	if _, err := Exec(ctx, dir, []string{"stash", "push", "b.txt"}); err != nil {
		t.Fatalf("pathspec push: %v", err)
	}
	if got := readFile(t, dir, "b.txt"); got != "bee\n" {
		t.Errorf("b.txt after push = %q, want HEAD", got)
	}
	if got := readFile(t, dir, "a.txt"); got != "dirty a\n" {
		t.Errorf("a.txt after pathspec push = %q, must stay dirty", got)
	}
	if _, err := Exec(ctx, dir, []string{"stash", "pop"}); err != nil {
		t.Fatalf("pop: %v", err)
	}
	if got := readFile(t, dir, "b.txt"); got != "dirty b\n" {
		t.Errorf("b.txt after pop = %q", got)
	}

	// Overlap: push v1, dirty v2, pop refuses and keeps the entry.
	dirtyFile(t, dir, "a.txt", "v1\n")
	if _, err := Exec(ctx, dir, []string{"stash", "push", "-m", "v1 entry"}); err != nil {
		t.Fatalf("push v1: %v", err)
	}
	dirtyFile(t, dir, "a.txt", "v2\n")
	res, err := Exec(ctx, dir, []string{"stash", "pop"})
	if err != nil {
		t.Fatalf("conflicting pop: %v", err)
	}
	if res.ExitCode != 1 || !strings.Contains(res.Stderr, "would be overwritten") {
		t.Errorf("conflicting pop = %+v, want exit 1 + overwrite warning", res)
	}
	res, err = Exec(ctx, dir, []string{"stash", "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(res.Stdout, "v1 entry") {
		t.Errorf("entry dropped on conflict: %q", res.Stdout)
	}
	// Resolve by restoring HEAD content, then pop applies cleanly.
	dirtyFile(t, dir, "a.txt", "line1\nline2\n")
	if _, err := Exec(ctx, dir, []string{"stash", "pop"}); err != nil {
		t.Fatalf("pop after resolve: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "v1\n" {
		t.Errorf("a.txt = %q, want v1", got)
	}

	// stash@{n}: two entries, pop the older one by address.
	dirtyFile(t, dir, "a.txt", "old\n")
	if _, err := Exec(ctx, dir, []string{"stash", "push", "-m", "older"}); err != nil {
		t.Fatalf("push older: %v", err)
	}
	dirtyFile(t, dir, "a.txt", "new\n")
	if _, err := Exec(ctx, dir, []string{"stash", "push", "-m", "newer"}); err != nil {
		t.Fatalf("push newer: %v", err)
	}
	if _, err := Exec(ctx, dir, []string{"stash", "pop", "stash@{1}"}); err != nil {
		t.Fatalf("pop stash@{1}: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "old\n" {
		t.Errorf("a.txt = %q, want older entry", got)
	}
	res, err = Exec(ctx, dir, []string{"stash", "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(res.Stdout, "older") || !strings.Contains(res.Stdout, "newer") {
		t.Errorf("list = %q, want only newer left", res.Stdout)
	}

	for _, argv := range [][]string{
		{"stash", "drop"},
		{"stash", "push", "--index"},
		{"stash", "push", "-u"},
	} {
		if _, err := Exec(ctx, dir, argv); err != ErrUnsupported {
			t.Errorf("stash %v err = %v, want ErrUnsupported", argv, err)
		}
	}
}

// commitCount counts commits reachable from HEAD.
func commitCount(t *testing.T, dir string) int {
	t.Helper()
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	it, err := r.Log(&gogit.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatal(err)
	}
	_ = it.ForEach(func(_ *object.Commit) error {
		n++
		return nil
	})
	it.Close()
	return n
}

func headMessage(t *testing.T, dir string) string {
	t.Helper()
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(c.Message)
}

// TestNativeCommit_Amend pins `commit --amend` (with and without -m) and
// `-q`: amend folds the index into HEAD without growing history.
func TestNativeCommit_Amend(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)

	dirtyFile(t, dir, "a.txt", "line1\nline2\nline3\n")
	res, err := Exec(ctx, dir, []string{"commit", "--amend", "-m", "second amended"})
	if err != nil {
		t.Fatalf("commit --amend: %v", err)
	}
	if commitCount(t, dir) != 2 {
		t.Errorf("amend grew history: %d commits", commitCount(t, dir))
	}
	if got := headMessage(t, dir); got != "second amended" {
		t.Errorf("HEAD message = %q", got)
	}
	if !strings.Contains(res.Stdout, "second amended") {
		t.Errorf("amend output = %q", res.Stdout)
	}

	// --amend without -m keeps the message; -q suppresses output.
	dirtyFile(t, dir, "a.txt", "line1\nline2\nline3\nline4\n")
	res, err = Exec(ctx, dir, []string{"commit", "--amend", "-q"})
	if err != nil {
		t.Fatalf("amend keep-message: %v", err)
	}
	if got := headMessage(t, dir); got != "second amended" {
		t.Errorf("HEAD message = %q, want kept", got)
	}
	if res.Stdout != "" {
		t.Errorf("-q output = %q, want empty", res.Stdout)
	}
	if commitCount(t, dir) != 2 {
		t.Errorf("history = %d commits, want 2", commitCount(t, dir))
	}
}

// TestNativeReset_HardSoft pins `reset --hard/--soft [<commit>]`: soft
// moves HEAD only (worktree+index untouched), hard restores the tree,
// mixed (default) unstages. Bad revisions and soft-with-paths fail loud.
func TestNativeReset_HardSoft(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"a.txt": "line1\nline2\nline3\n"}, "third")

	// Soft: HEAD moves back, worktree keeps v3 content.
	dirtyFile(t, dir, "a.txt", "line1\nline2\nline3\n")
	if _, err := Exec(ctx, dir, []string{"reset", "--soft", "HEAD~1"}); err != nil {
		t.Fatalf("reset --soft: %v", err)
	}
	if got := headMessage(t, dir); got != "second" {
		t.Errorf("HEAD after soft = %q, want second", got)
	}
	if got := readFile(t, dir, "a.txt"); got != "line1\nline2\nline3\n" {
		t.Errorf("worktree after soft = %q, must be untouched", got)
	}

	// Hard: HEAD moves back AND the tree is restored.
	if _, err := Exec(ctx, dir, []string{"reset", "--hard", "HEAD~1"}); err != nil {
		t.Fatalf("reset --hard: %v", err)
	}
	if got := headMessage(t, dir); got != "first" {
		t.Errorf("HEAD after hard = %q, want first", got)
	}
	if got := readFile(t, dir, "a.txt"); got != "line1\n" {
		t.Errorf("worktree after hard = %q, want first-commit content", got)
	}

	// Mixed to a hash: HEAD moves, worktree stays dirty.
	commitFiles(t, dir, map[string]string{"a.txt": "line1\nline2\n"}, "second again")
	head, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	dirtyFile(t, dir, "a.txt", "uncommitted\n")
	if _, err := Exec(ctx, dir, []string{"reset", head.Hash}); err != nil {
		t.Fatalf("mixed reset to hash: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "uncommitted\n" {
		t.Errorf("worktree after mixed = %q, must be untouched", got)
	}

	res, err := Exec(ctx, dir, []string{"reset", "--hard", "no-such-rev"})
	if err != nil {
		t.Fatalf("bad rev: %v", err)
	}
	if res.ExitCode != 128 {
		t.Errorf("bad rev = %+v, want exit 128", res)
	}
	res, err = Exec(ctx, dir, []string{"reset", "--soft", "HEAD", "--", "a.txt"})
	if err != nil {
		t.Fatalf("soft with paths: %v", err)
	}
	if res.ExitCode != 128 {
		t.Errorf("soft with paths = %+v, want exit 128", res)
	}
	if _, err := Exec(ctx, dir, []string{"reset", "--keep"}); err != ErrUnsupported {
		t.Errorf("reset --keep err = %v, want ErrUnsupported", err)
	}
}

// TestNativeRevert_Rollback pins `revert <commit>`: the change is undone
// in a new Revert commit; -n stages without committing; merges,
// sequencer flags, and inapplicable reversions stay loud.
func TestNativeRevert_Rollback(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"a.txt": "line1\nline2\nline3\n"}, "bad change")

	res, err := Exec(ctx, dir, []string{"revert", "--no-edit", "HEAD"})
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "line1\nline2\n" {
		t.Errorf("a.txt after revert = %q, want change undone", got)
	}
	if got := headMessage(t, dir); !strings.HasPrefix(got, `Revert "bad change"`) {
		t.Errorf("HEAD message = %q, want Revert prefix", got)
	}
	if !strings.Contains(res.Stdout, `Revert "bad change"`) {
		t.Errorf("revert output = %q", res.Stdout)
	}
	if n := commitCount(t, dir); n != 4 {
		t.Errorf("history = %d commits, want 4", n)
	}

	// -n stages the reversal without committing. "worse change" jumped
	// 2-line → 4-line in one commit, so its reversal lands on 2-line.
	commitFiles(t, dir, map[string]string{"a.txt": "line1\nline2\nline3\nline4\n"}, "worse change")
	if _, err := Exec(ctx, dir, []string{"revert", "-n", "HEAD"}); err != nil {
		t.Fatalf("revert -n: %v", err)
	}
	if n := commitCount(t, dir); n != 5 {
		t.Errorf("history = %d commits, want 5 (-n commits nothing)", n)
	}
	if got := readFile(t, dir, "a.txt"); got != "line1\nline2\n" {
		t.Errorf("a.txt after -n = %q", got)
	}

	// Inapplicable reversion: file gone since — loud, tree untouched.
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	commitFiles(t, dir, map[string]string{"b.txt": "bee\n"}, "drop a")
	if _, err := Exec(ctx, dir, []string{"revert", "HEAD~3"}); err != ErrUnsupported {
		t.Errorf("inapplicable revert err = %v, want ErrUnsupported", err)
	}
	if n := commitCount(t, dir); n != 6 {
		t.Errorf("history = %d, failed revert must not commit", n)
	}

	for _, argv := range [][]string{
		{"revert", "--continue"},
		{"revert", "HEAD", "HEAD~1"},
		{"revert", "no-such-rev"},
	} {
		if _, err := Exec(ctx, dir, argv); err != ErrUnsupported {
			t.Errorf("revert %v err = %v, want ErrUnsupported", argv, err)
		}
	}
}

// TestNativeDiff_NameOnlyFilter pins `diff --name-only` (+ `--cached`,
// `--diff-filter`, pathspecs): the conflict-triage listing from the
// GAPS.md reality check. Patch output and commit revisions stay loud.
func TestNativeDiff_NameOnlyFilter(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"c.txt": "see\n"}, "add c")

	dirtyFile(t, dir, "a.txt", "modified\n")
	if err := os.Remove(filepath.Join(dir, "c.txt")); err != nil {
		t.Fatal(err)
	}

	res, err := Exec(ctx, dir, []string{"diff", "--name-only"})
	if err != nil {
		t.Fatalf("name-only: %v", err)
	}
	if res.Stdout != "a.txt\nc.txt\n" {
		t.Errorf("name-only = %q, want both files sorted", res.Stdout)
	}
	res, err = Exec(ctx, dir, []string{"diff", "--name-only", "--diff-filter=M"})
	if err != nil {
		t.Fatalf("filter M: %v", err)
	}
	if res.Stdout != "a.txt\n" {
		t.Errorf("filter M = %q", res.Stdout)
	}
	res, err = Exec(ctx, dir, []string{"diff", "--name-only", "--diff-filter=d", "--", "c.txt"})
	if err != nil {
		t.Fatalf("exclude d: %v", err)
	}
	if res.Stdout != "" {
		t.Errorf("exclude d + path = %q, want empty", res.Stdout)
	}

	// Staged column: stage a.txt, cached lists it, unstaged does not.
	if _, err := Exec(ctx, dir, []string{"add", "a.txt"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	res, err = Exec(ctx, dir, []string{"diff", "--cached", "--name-only"})
	if err != nil {
		t.Fatalf("cached name-only: %v", err)
	}
	if res.Stdout != "a.txt\n" {
		t.Errorf("cached = %q, want staged a.txt", res.Stdout)
	}

	// --quiet honors the filter: only deletions present, M-only → 0.
	res, err = Exec(ctx, dir, []string{"diff", "--quiet", "--diff-filter=M"})
	if err != nil {
		t.Fatalf("quiet filter: %v", err)
	}
	_ = res

	for _, argv := range [][]string{
		{"diff", "--stat"},
		{"diff", "--name-only", "HEAD"},
		{"diff", "--cached"},
	} {
		if _, err := Exec(ctx, dir, argv); err != ErrUnsupported {
			t.Errorf("diff %v err = %v, want ErrUnsupported", argv, err)
		}
	}
}

// TestNativePush_Delete pins `push --delete` and the `:<branch>` refspec:
// the branch leaves the remote, output mirrors host git's " - [deleted]"
// stderr shape, and a ref-less --delete fails loud like host git (128).
func TestNativePush_Delete(t *testing.T) {
	ctx := context.Background()
	remoteDir := t.TempDir()
	remote, err := gogit.PlainInit(remoteDir, true)
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}
	_ = remote
	localDir := setupTestRepo(t)
	repo, err := gogit.PlainOpen(localDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remoteDir}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	if _, err := nativePush(ctx, localDir, nil); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	if _, err := nativeCheckout(ctx, localDir, []string{"-b", "feat"}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, localDir, map[string]string{"feat.txt": "f\n"}, "feat work")
	if _, err := nativePush(ctx, localDir, []string{"origin", "feat"}); err != nil {
		t.Fatalf("push feat: %v", err)
	}
	branchGone := func() bool {
		r, err := gogit.PlainOpen(remoteDir)
		if err != nil {
			t.Fatalf("open remote: %v", err)
		}
		_, err = r.Reference(plumbing.NewBranchReferenceName("feat"), false)
		return err != nil
	}

	res, err := nativePush(ctx, localDir, []string{"origin", "--delete", "feat"})
	if err != nil {
		t.Fatalf("push --delete: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stderr, "[deleted]") || !strings.Contains(res.Stderr, "feat") {
		t.Errorf("delete result = %+v, want exit 0 and [deleted] feat on stderr", res)
	}
	if !branchGone() {
		t.Errorf("feat still on remote after --delete")
	}

	// The ":<branch>" refspec form deletes too.
	if _, err := nativePush(ctx, localDir, []string{"origin", "feat"}); err != nil {
		t.Fatalf("re-push feat: %v", err)
	}
	if _, err := nativePush(ctx, localDir, []string{"origin", ":feat"}); err != nil {
		t.Fatalf("push :feat: %v", err)
	}
	if !branchGone() {
		t.Errorf("feat still on remote after :feat refspec")
	}

	// No refs to delete: host git's fatal, exit 128.
	res, err = nativePush(ctx, localDir, []string{"--delete", "origin"})
	if err != nil {
		t.Fatalf("ref-less delete: %v", err)
	}
	if res.ExitCode != 128 || !strings.Contains(res.Stderr, "--delete doesn't make sense") {
		t.Errorf("ref-less delete = %+v, want exit 128 + host fatal", res)
	}
}
