package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	gitindex "github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
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

// TestNativeCheckoutCreateBranchNoCheckoutClone makes sure checkout -b on an
// unborn index clone (--no-checkout) returns ErrUnsupported before any write so
// RunExternal host fallback performs the initial checkout cleanly with no
// half-written index or staged deletions.
func TestNativeCheckoutCreateBranchNoCheckoutClone(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	if _, err := Init(InitOptions{Path: src}); err != nil {
		t.Fatalf("init source: %v", err)
	}
	setLocalIdentity(t, src)

	// Build a real nested tree: several subdirectories, a binary file, ~200 files.
	files := make(map[string]string, 210)
	subdirs := []string{
		"cmd/yoke/cli",
		"pkg/weave/engine",
		"pkg/agent/runtime",
		"docs/specs/v1",
		"internal/tools/scripts",
		"assets/templates/nested",
	}
	for i := range 200 {
		dir := subdirs[i%len(subdirs)]
		files[fmt.Sprintf("%s/file_%03d.txt", dir, i)] = fmt.Sprintf("nested content %d\n", i)
	}
	commitFiles(t, src, files, "seed text files")

	// Binary file
	binPath := filepath.Join(src, "assets", "binary", "archive.tgz")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(binPath, []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0x00, 0x42}, 0o644); err != nil {
		t.Fatalf("write binary file: %v", err)
	}
	if _, err := Add(AddOptions{RepoPath: src, All: true}); err != nil {
		t.Fatalf("add binary: %v", err)
	}
	if _, err := Commit(CommitOptions{RepoPath: src, Message: "seed binary", AuthorName: "T", AuthorEmail: "t@e"}); err != nil {
		t.Fatalf("commit binary: %v", err)
	}

	sha, err := RunChecked(ctx, src, []string{"rev-parse", "HEAD"})
	if err != nil {
		t.Fatalf("resolve source HEAD: %v", err)
	}
	sha = strings.TrimSpace(sha)

	dst := filepath.Join(t.TempDir(), "clone")
	cmd := osexec.CommandContext(ctx, "git", "clone", "--local", "--no-hardlinks", "--no-checkout", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone --no-checkout: %v\n%s", err, out)
	}

	// Verify the clone has an unborn index (.git/index does not exist).
	idxFile := filepath.Join(dst, ".git", "index")
	if _, err := os.Stat(idxFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected unborn index (.git/index absent), got err: %v", err)
	}

	// Exec checkout -b must return ErrUnsupported BEFORE any write.
	branch := "agent/x"
	args := []string{"checkout", "-b", branch, sha}
	res, err := Exec(ctx, dst, args)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Exec on unborn index returned %v (res=%+v), want ErrUnsupported", err, res)
	}
	// Verify no .git/index was left behind by the native refusal.
	if _, err := os.Stat(idxFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native checkout wrote index on unborn clone: %v", err)
	}
	// Verify branch was not created.
	ref, err := Exec(ctx, dst, []string{"rev-parse", "--verify", "refs/heads/" + branch})
	if err == nil && ref.ExitCode == 0 {
		t.Fatalf("native refusal created branch %q", branch)
	}

	// RunExternal fallback must perform the initial checkout cleanly.
	fres, ferr := RunExternal(ctx, dst, args)
	if ferr != nil {
		t.Fatalf("RunExternal fallback failed: %v", ferr)
	}
	if fres.ExitCode != 0 {
		t.Fatalf("RunExternal exit %d: %s", fres.ExitCode, fres.Stderr)
	}

	// Assert git status --porcelain is empty and all files exist.
	statusOut, err := RunChecked(ctx, dst, []string{"status", "--porcelain"})
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(statusOut) != "" {
		t.Fatalf("git status --porcelain not empty:\n%s", statusOut)
	}

	for f := range files {
		full := filepath.Join(dst, filepath.FromSlash(f))
		if _, err := os.Stat(full); err != nil {
			t.Fatalf("file %s missing: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "assets", "binary", "archive.tgz")); err != nil {
		t.Fatalf("binary file missing: %v", err)
	}
}

func TestNativeCheckoutFailureRestoresOriginalIndex(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	if _, err := Init(InitOptions{Path: src}); err != nil {
		t.Fatalf("init source: %v", err)
	}
	setLocalIdentity(t, src)

	commitFiles(t, src, map[string]string{
		"a.txt": "file a\n",
		"b.txt": "file b\n",
	}, "initial commit")

	idxFile := filepath.Join(src, ".git", "index")

	// Create a second commit with a file in a subdirectory
	commitFiles(t, src, map[string]string{
		"sub/c.txt": "file c\n",
	}, "second commit")
	sha, err := RunChecked(ctx, src, []string{"rev-parse", "HEAD"})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}

	// Reset back to first commit so worktree and index are at commit 1
	if _, err := RunChecked(ctx, src, []string{"reset", "--hard", "HEAD~1"}); err != nil {
		t.Fatalf("reset hard: %v", err)
	}

	origResetBytes, err := os.ReadFile(idxFile)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}

	// Make the worktree prevent writing `sub/c.txt` by making `sub` a read-only directory
	subDir := filepath.Join(src, "sub")
	if err := os.MkdirAll(subDir, 0o500); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	defer os.Chmod(subDir, 0o755)

	// Attempt checkout -b to the second commit which needs to create sub/c.txt
	res, err := Exec(ctx, src, []string{"checkout", "-b", "agent/blocked", strings.TrimSpace(sha)})
	if err == nil && (res == nil || res.ExitCode == 0) {
		t.Fatalf("expected checkout to fail on unwritable dir")
	}

	// Verify that index file was restored to origResetBytes
	afterBytes, err := os.ReadFile(idxFile)
	if err != nil {
		t.Fatalf("read index after failure: %v", err)
	}
	if !bytes.Equal(afterBytes, origResetBytes) {
		t.Fatalf("index was not restored on checkout failure: got %d bytes, want %d bytes", len(afterBytes), len(origResetBytes))
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
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
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
		{"diff", "--cached", "--word-diff"},
	} {
		if _, err := Exec(ctx, dir, argv); err != ErrUnsupported {
			t.Errorf("diff %v err = %v, want ErrUnsupported", argv, err)
		}
	}
}

// TestNativeClean_DryRunAndForce pins `clean -n/-f/-d`: dry-run lists
// without touching, -f removes untracked files, -d takes whole dirs,
// ignored files and tracked content always survive, nested repos are
// skipped, and a bare clean refuses like host git.
func TestNativeClean_DryRunAndForce(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{".gitignore": "ignored.log\n"}, "ignore log")

	dirtyFile(t, dir, "top.txt", "top\n")
	dirtyFile(t, dir, "sub/inner.txt", "inner\n")
	dirtyFile(t, dir, "ignored.log", "ignored\n")
	if _, err := Init(InitOptions{Path: filepath.Join(dir, "nested")}); err != nil {
		t.Fatalf("nested init: %v", err)
	}

	res, err := Exec(ctx, dir, []string{"clean", "-n"})
	if err != nil {
		t.Fatalf("clean -n: %v", err)
	}
	if !strings.Contains(res.Stdout, "Would remove top.txt") {
		t.Errorf("dry-run = %q, want top.txt listed", res.Stdout)
	}
	if strings.Contains(res.Stdout, "ignored.log") {
		t.Errorf("dry-run = %q, ignored files must not list", res.Stdout)
	}
	if _, serr := os.Stat(filepath.Join(dir, "top.txt")); serr != nil {
		t.Errorf("dry-run removed top.txt")
	}

	if _, err := Exec(ctx, dir, []string{"clean", "-f"}); err != nil {
		t.Fatalf("clean -f: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "top.txt")); !os.IsNotExist(serr) {
		t.Errorf("top.txt survived clean -f")
	}
	// No -d: untracked directories are never recursed into.
	if _, serr := os.Stat(filepath.Join(dir, "sub", "inner.txt")); serr != nil {
		t.Errorf("sub/inner.txt removed without -d: %v", serr)
	}
	if got := readFile(t, dir, "a.txt"); got != "line1\nline2\n" {
		t.Errorf("tracked a.txt = %q after clean", got)
	}
	if got := readFile(t, dir, "ignored.log"); got != "ignored\n" {
		t.Errorf("ignored.log = %q after clean, must survive", got)
	}

	res, err = Exec(ctx, dir, []string{"clean", "-fd"})
	if err != nil {
		t.Fatalf("clean -fd: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(serr) {
		t.Errorf("sub/ survived clean -fd")
	}
	if _, serr := os.Stat(filepath.Join(dir, "nested")); !os.IsNotExist(serr) {
		if !strings.Contains(res.Stdout, "Skipping repository nested") {
			t.Errorf("nested repo removed or skipped silently: %q", res.Stdout)
		}
	}

	res, err = Exec(ctx, dir, []string{"clean"})
	if err != nil {
		t.Fatalf("bare clean: %v", err)
	}
	if res.ExitCode != 128 {
		t.Errorf("bare clean = %+v, want exit 128", res)
	}
}

// craftUnmerged stages a host-style conflict for path: base/ours/theirs
// blobs in the index stages plus marker text in the worktree.
func craftUnmerged(t *testing.T, dir, path, base, ours, theirs string) {
	t.Helper()
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	put := func(content string) plumbing.Hash {
		obj := r.Storer.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		obj.SetSize(int64(len(content)))
		w, err := obj.Writer()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		h, err := r.Storer.SetEncodedObject(obj)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	idx, err := r.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	var rebuilt []*gitindex.Entry
	for _, e := range idx.Entries {
		if e.Name != path {
			rebuilt = append(rebuilt, e)
		}
	}
	rebuilt = append(rebuilt,
		&gitindex.Entry{Name: path, Hash: put(base), Mode: filemode.Regular, Stage: gitindex.Merged},
		&gitindex.Entry{Name: path, Hash: put(ours), Mode: filemode.Regular, Stage: gitindex.OurMode},
		&gitindex.Entry{Name: path, Hash: put(theirs), Mode: filemode.Regular, Stage: gitindex.TheirMode},
	)
	idx.Entries = rebuilt
	if ist, ok := r.Storer.(storer.IndexStorer); ok {
		if err := ist.SetIndex(idx); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal("index not writable")
	}
	markers := "<<<<<<< ours\n" + ours + "=======\n" + theirs + ">>>>>>> theirs\n"
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(path)), []byte(markers), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNativeCheckout_Sides pins `checkout --theirs/--ours` (conflict
// triage from unmerged index stages), `-f` branch switches that discard
// local changes, `branch -v` output, and loud unknown branch flags.
func TestNativeCheckout_Sides(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)

	if _, err := Exec(ctx, dir, []string{"branch", "--bogus"}); err != ErrUnsupported {
		t.Errorf("branch --bogus err = %v, want ErrUnsupported", err)
	}
	res, err := Exec(ctx, dir, []string{"branch", "-v"})
	if err != nil {
		t.Fatalf("branch -v: %v", err)
	}
	if !strings.Contains(res.Stdout, "second") {
		t.Errorf("branch -v = %q, want tip subject", res.Stdout)
	}

	base := currentBranch(t, dir)
	if _, err := Exec(ctx, dir, []string{"checkout", "-b", "other"}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, dir, map[string]string{"a.txt": "other branch\n"}, "other work")
	if _, err := Exec(ctx, dir, []string{"checkout", base}); err != nil {
		t.Fatalf("checkout back: %v", err)
	}
	dirtyFile(t, dir, "a.txt", "local edits\n")
	if _, err := Exec(ctx, dir, []string{"checkout", "-f", "other"}); err != nil {
		t.Fatalf("checkout -f: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "other branch\n" {
		t.Errorf("a.txt after -f checkout = %q", got)
	}

	// Crafted conflict: --theirs takes stage 3, --ours stage 2.
	craftUnmerged(t, dir, "a.txt", "other branch\n", "our side\n", "their side\n")
	if _, err := Exec(ctx, dir, []string{"checkout", "--theirs", "--", "a.txt"}); err != nil {
		t.Fatalf("checkout --theirs: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "their side\n" {
		t.Errorf("a.txt after --theirs = %q", got)
	}
	craftUnmerged(t, dir, "a.txt", "other branch\n", "our side\n", "their side\n")
	if _, err := Exec(ctx, dir, []string{"checkout", "--ours", "a.txt"}); err != nil {
		t.Fatalf("checkout --ours: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "our side\n" {
		t.Errorf("a.txt after --ours = %q", got)
	}

	// Merged paths are not triage candidates: loud exit 1.
	res, err = Exec(ctx, dir, []string{"checkout", "--theirs", "a.txt"})
	if err != nil {
		t.Fatalf("theirs on merged: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("theirs on merged = %+v, want exit 1", res)
	}
}

const applyModifyPatch = `diff --git a/a.txt b/a.txt
--- a/a.txt
+++ b/a.txt
@@ -1,2 +1,2 @@
 line1
-line2
+line2 changed
`

const applyNewPatch = `diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+hello
`

const applyDeletePatch = `diff --git a/c.txt b/c.txt
deleted file mode 100644
--- a/c.txt
+++ /dev/null
@@ -1 +0,0 @@
-see
`

// writePatch stores a patch file in dir and returns its name.
func writePatch(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

// TestNativeApply_Files pins `apply <patch>`: modify/new/delete hunks
// land byte-exact in the worktree (unstaged — plain apply never touches
// the index), context mismatches fail exit-1 with the tree untouched
// across ALL files, and binary patches stay loud.
func TestNativeApply_Files(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"c.txt": "see\n"}, "add c")

	patch := writePatch(t, dir, "fix.patch", applyModifyPatch+applyNewPatch+applyDeletePatch)

	// --check first, on the pristine tree: verifies without writing.
	if _, err := Exec(ctx, dir, []string{"apply", "--check", patch}); err != nil {
		t.Fatalf("apply --check: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "line1\nline2\n" {
		t.Errorf("apply --check wrote files")
	}
	if _, serr := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(serr) {
		t.Errorf("apply --check created new.txt")
	}

	if _, err := Exec(ctx, dir, []string{"apply", patch}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "line1\nline2 changed\n" {
		t.Errorf("a.txt = %q", got)
	}
	if got := readFile(t, dir, "new.txt"); got != "hello\n" {
		t.Errorf("new.txt = %q", got)
	}
	if _, serr := os.Stat(filepath.Join(dir, "c.txt")); !os.IsNotExist(serr) {
		t.Errorf("c.txt survived apply-delete")
	}
	// Plain apply stages nothing.
	res, err := Exec(ctx, dir, []string{"diff", "--cached", "--name-only"})
	if err != nil {
		t.Fatalf("cached: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		t.Errorf("apply staged files: %q", res.Stdout)
	}

	// Mismatched context: exit 1, and the multi-file run applies
	// nothing (the new-file hunk must not land either). Reset the
	// tree first so only the modify hunk is broken.
	dirtyFile(t, dir, "a.txt", "different\n")
	if err := os.Remove(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	dirtyFile(t, dir, "c.txt", "see\n")
	res, err = Exec(ctx, dir, []string{"apply", patch})
	if err != nil {
		t.Fatalf("bad-context apply: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("bad-context = %+v, want exit 1", res)
	}
	if _, serr := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(serr) {
		t.Errorf("new.txt landed despite failed sibling hunk")
	}
	if got := readFile(t, dir, "c.txt"); got != "see\n" {
		t.Errorf("c.txt = %q after failed apply, delete must not run", got)
	}
	if got := readFile(t, dir, "a.txt"); got != "different\n" {
		t.Errorf("a.txt = %q after failed apply", got)
	}

	bin := writePatch(t, dir, "bin.patch", "diff --git a/x.bin b/x.bin\nBinary files a/x.bin and b/x.bin differ\n")
	if _, err := Exec(ctx, dir, []string{"apply", bin}); err != ErrUnsupported {
		t.Errorf("binary apply err = %v, want ErrUnsupported", err)
	}
	res, err = Exec(ctx, dir, []string{"apply", "no-such.patch"})
	if err != nil {
		t.Fatalf("missing patch: %v", err)
	}
	if res.ExitCode != 128 {
		t.Errorf("missing patch = %+v, want exit 128", res)
	}
	for _, argv := range [][]string{
		{"apply"},
		{"apply", "-R", patch},
		{"apply", "-"},
	} {
		if _, err := Exec(ctx, dir, argv); err != ErrUnsupported {
			t.Errorf("apply %v err = %v, want ErrUnsupported", argv, err)
		}
	}
}

// TestNativeRemote_ListAdd pins bare `remote`, `remote -v` and
// `remote add`: the template-bootstrap verbs weave calls per start.
func TestNativeRemote_ListAdd(t *testing.T) {
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)

	if _, err := Exec(ctx, dir, []string{"remote", "add", "origin", "https://example.com/r.git"}); err != nil {
		t.Fatalf("remote add: %v", err)
	}
	res, err := Exec(ctx, dir, []string{"remote"})
	if err != nil {
		t.Fatalf("remote: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "origin" {
		t.Errorf("remote = %q, want origin", res.Stdout)
	}
	res, err = Exec(ctx, dir, []string{"remote", "-v"})
	if err != nil {
		t.Fatalf("remote -v: %v", err)
	}
	if !strings.Contains(res.Stdout, "origin\thttps://example.com/r.git (fetch)") {
		t.Errorf("remote -v = %q", res.Stdout)
	}
	res, err = Exec(ctx, dir, []string{"remote", "add", "origin", "https://example.com/other.git"})
	if err != nil {
		t.Fatalf("duplicate add: %v", err)
	}
	if res.ExitCode != 128 {
		t.Errorf("duplicate add = %+v, want exit 128", res)
	}
}

// TestRunExternal_OneDoor pins the S252.6 contract: routed verbs answer
// natively (byte-identical to Exec), unrouted verbs replay against the
// host binary with its exit code preserved. Skipped where no host git
// exists — the native half is covered by every other test here.
func TestRunExternal_OneDoor(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("no host git on PATH")
	}
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)

	want, err := Exec(ctx, dir, []string{"status"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got, err := RunExternal(ctx, dir, []string{"status"})
	if err != nil {
		t.Fatalf("RunExternal: %v", err)
	}
	if got.Stdout != want.Stdout {
		t.Errorf("native mismatch: Exec %q vs RunExternal %q", want.Stdout, got.Stdout)
	}

	res, err := RunExternal(ctx, dir, []string{"--version"})
	if err != nil {
		t.Fatalf("host --version: %v", err)
	}
	if res.ExitCode != 0 || !strings.HasPrefix(strings.TrimSpace(res.Stdout), "git version ") {
		t.Errorf("host --version = %+v", res)
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

// TestNativePush_TagRefspec pins full-refspec passthrough: `push origin
// refs/tags/<t>` must land the tag, not a mangled refs/heads path.
// (Caught live by sdlc's deploy idempotency via the S252.6 door.)
func TestNativePush_TagRefspec(t *testing.T) {
	ctx := context.Background()
	remoteDir := t.TempDir()
	if _, err := gogit.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	localDir := setupTestRepo(t)
	repo, err := gogit.PlainOpen(localDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remoteDir}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if _, err := repo.CreateTag("v9.9.9", head.Hash(), nil); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if _, err := nativePush(ctx, localDir, []string{"-f", "origin", "refs/tags/v9.9.9"}); err != nil {
		t.Fatalf("push tag refspec: %v", err)
	}
	remote, err := gogit.PlainOpen(remoteDir)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	if _, err := remote.Reference(plumbing.NewTagReferenceName("v9.9.9"), false); err != nil {
		t.Errorf("tag not on remote after push: %v", err)
	}
	if _, err := remote.Reference(plumbing.NewBranchReferenceName("refs/tags/v9.9.9"), false); err == nil {
		t.Errorf("mangled refs/heads/refs/tags ref landed on remote")
	}
}
