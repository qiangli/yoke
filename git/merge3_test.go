package git

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

func TestDiff3Merge(t *testing.T) {
	cases := []struct {
		name               string
		base, ours, theirs string
		wantMerged         string
		wantClean          bool
	}{
		{
			name:       "disjoint hunks on different lines",
			base:       "a\nb\nc\n",
			ours:       "a\nB\nc\n",
			theirs:     "a\nb\nC\n",
			wantMerged: "a\nB\nC\n",
			wantClean:  true,
		},
		{
			name:       "only ours changed",
			base:       "a\nb\nc\n",
			ours:       "a\nB\nc\n",
			theirs:     "a\nb\nc\n",
			wantMerged: "a\nB\nc\n",
			wantClean:  true,
		},
		{
			name:       "only theirs changed",
			base:       "a\nb\nc\n",
			ours:       "a\nb\nc\n",
			theirs:     "a\nb\nC\n",
			wantMerged: "a\nb\nC\n",
			wantClean:  true,
		},
		{
			name:       "identical edit on both sides",
			base:       "a\nb\nc\n",
			ours:       "a\nX\nc\n",
			theirs:     "a\nX\nc\n",
			wantMerged: "a\nX\nc\n",
			wantClean:  true,
		},
		{
			name:      "overlapping edit conflicts",
			base:      "a\nb\nc\n",
			ours:      "a\nX\nc\n",
			theirs:    "a\nY\nc\n",
			wantClean: false,
		},
		{
			name:      "delete vs modify conflicts",
			base:      "a\nb\nc\n",
			ours:      "a\nc\n",
			theirs:    "a\nY\nc\n",
			wantClean: false,
		},
		{
			name:       "both delete the same line",
			base:       "a\nb\nc\n",
			ours:       "a\nc\n",
			theirs:     "a\nc\n",
			wantMerged: "a\nc\n",
			wantClean:  true,
		},
		{
			name:       "disjoint additions at different points",
			base:       "a\nb\nc\n",
			ours:       "a\nb\nc\nd\n",
			theirs:     "z\na\nb\nc\n",
			wantMerged: "z\na\nb\nc\nd\n",
			wantClean:  true,
		},
		{
			name:       "no trailing newline preserved",
			base:       "a\nb",
			ours:       "a\nB",
			theirs:     "a\nb",
			wantMerged: "a\nB",
			wantClean:  true,
		},
		// Regression: an insertion abutting the other side's deletion must
		// NOT silently merge (it previously resurrected the deleted line).
		{
			name:      "insert adjacent to delete conflicts",
			base:      "a\nb\nc\n",
			ours:      "a\nc\n",       // delete b
			theirs:    "a\nX\nb\nc\n", // insert X before b
			wantClean: false,
		},
		// Regression: an insertion inside the other side's modify region.
		{
			name:      "insert adjacent to modify conflicts",
			base:      "a\nb\nc\n",
			ours:      "a\nb\nX\nc\n", // insert X after b
			theirs:    "a\nB\nc\n",    // modify b
			wantClean: false,
		},
		// A pure append on one side and a disjoint earlier edit on the
		// other stays clean (the append is far from the edit).
		{
			name:       "append plus disjoint edit stays clean",
			base:       "a\nb\nc\n",
			ours:       "a\nb\nc\nd\n", // append d
			theirs:     "A\nb\nc\n",    // modify a
			wantMerged: "A\nb\nc\nd\n",
			wantClean:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, ok := diff3Merge([]byte(tc.base), []byte(tc.ours), []byte(tc.theirs))
			if ok != tc.wantClean {
				t.Fatalf("clean=%v, want %v (merged=%q)", ok, tc.wantClean, string(merged))
			}
			if tc.wantClean && string(merged) != tc.wantMerged {
				t.Errorf("merged=%q, want %q", string(merged), tc.wantMerged)
			}
		})
	}
}

// TestMerge_DivergedSameFileCleanHunks exercises the full typed Merge on
// diverged branches that edit the SAME file on non-overlapping lines —
// the case where Phase B diff3 (not just tree-level) is required.
func TestMerge_DivergedSameFileCleanHunks(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"f.txt": "1\n2\n3\n4\n5\n"}, "base")
	main := currentBranch(t, dir)

	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, dir, map[string]string{"f.txt": "1\n2\nTHREE\n4\n5\n"}, "edit line 3")
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	commitFiles(t, dir, map[string]string{"f.txt": "ONE\n2\n3\n4\n5\n"}, "edit line 1")

	res, err := Merge(MergeOptions{RepoPath: dir, Ref: "feature"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !res.Success {
		t.Fatalf("merge not successful: %+v", res)
	}
	if got := readFile(t, dir, "f.txt"); got != "ONE\n2\nTHREE\n4\n5\n" {
		t.Errorf("f.txt = %q, want %q", got, "ONE\n2\nTHREE\n4\n5\n")
	}
}

// TestMerge_DivergedConflictAtomic verifies a conflicting merge returns a
// *ConflictError AND leaves the repository completely untouched (the
// merge / merge --abort contract collapsed into one atomic op).
func TestMerge_DivergedConflictAtomic(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"f.txt": "1\n2\n3\n"}, "base")
	main := currentBranch(t, dir)

	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, dir, map[string]string{"f.txt": "1\nTHEIRS\n3\n"}, "their line 2")
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	commitFiles(t, dir, map[string]string{"f.txt": "1\nOURS\n3\n"}, "our line 2")

	mainTip, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}

	_, err = Merge(MergeOptions{RepoPath: dir, Ref: "feature"})
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ConflictError, got %v", err)
	}
	if len(ce.Files) != 1 || ce.Files[0] != "f.txt" {
		t.Errorf("conflict files = %v, want [f.txt]", ce.Files)
	}

	// HEAD must be unchanged (no merge commit recorded).
	after, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse after: %v", err)
	}
	if after.Hash != mainTip.Hash {
		t.Errorf("HEAD moved to %s, want unchanged %s", after.Hash, mainTip.Hash)
	}
	// Worktree must be clean and still hold OURS.
	if got := readFile(t, dir, "f.txt"); got != "1\nOURS\n3\n" {
		t.Errorf("f.txt = %q, want ours preserved", got)
	}
	repo, _ := gogit.PlainOpen(dir)
	wt, _ := repo.Worktree()
	st, _ := wt.Status()
	if !st.IsClean() {
		t.Errorf("worktree not clean after aborted merge: %v", st)
	}
}

// TestMerge_NoSystemGit proves the diverged-merge path has no system-git
// dependency: the entire scenario runs with PATH emptied, so any attempt
// to exec a `git` binary would fail.
func TestMerge_NoSystemGit(t *testing.T) {
	t.Setenv("PATH", "")

	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"a.txt": "base\n"}, "base")
	main := currentBranch(t, dir)

	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, dir, map[string]string{"b.txt": "feature\n"}, "feature")
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	commitFiles(t, dir, map[string]string{"c.txt": "main\n"}, "main")

	res, err := Merge(MergeOptions{RepoPath: dir, Ref: "feature"})
	if err != nil {
		t.Fatalf("merge without system git: %v", err)
	}
	if !res.Success {
		t.Fatalf("merge not successful: %+v", res)
	}
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.ReadFile(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s missing after merge: %v", f, err)
		}
	}
}

// TestMerge_PreservesTheirsModeChange verifies a theirs-only mode change
// (exec bit) survives to the worktree — O_TRUNC alone would have kept the
// old permissions.
func TestMerge_PreservesTheirsModeChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the executable bit does not exist on Windows, so a mode-only change is a no-op")
	}
	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	setLocalIdentity(t, dir)
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	commitFiles(t, dir, nil, "base") // stages run.sh at 0644
	main := currentBranch(t, dir)

	// feature: make run.sh executable (mode-only change), commit.
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	commitFiles(t, dir, nil, "make executable")

	// main: unrelated change, commit (forces a real 3-way merge).
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	commitFiles(t, dir, map[string]string{"other.txt": "x\n"}, "unrelated")

	if _, err := Merge(MergeOptions{RepoPath: dir, Ref: "feature"}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("run.sh mode = %v, want executable bit set after merge", info.Mode())
	}
}

// TestMerge_CrissCrossRefused builds a criss-cross history (two merge
// bases) and confirms the merge refuses rather than guessing a base.
func TestMerge_CrissCrossRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	setLocalIdentity(t, dir)
	commitFiles(t, dir, map[string]string{"o.txt": "o\n"}, "O")
	main := currentBranch(t, dir)

	// A from O, commit a1 (A stays here).
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "A", Create: true}); err != nil {
		t.Fatalf("checkout A: %v", err)
	}
	commitFiles(t, dir, map[string]string{"a.txt": "a\n"}, "a1")

	// B from O, commit b1 (B stays here).
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "B", Create: true}); err != nil {
		t.Fatalf("checkout B: %v", err)
	}
	commitFiles(t, dir, map[string]string{"b.txt": "b\n"}, "b1")

	// X branches off A (a1) and merges B (b1): merge commit M1 = parents
	// {a1, b1}, single base O. A and B tips never move.
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "A"}); err != nil {
		t.Fatalf("checkout A: %v", err)
	}
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "X", Create: true}); err != nil {
		t.Fatalf("checkout X: %v", err)
	}
	if _, err := Merge(MergeOptions{RepoPath: dir, Ref: "B"}); err != nil {
		t.Fatalf("merge B into X: %v", err)
	}

	// Y branches off B (b1) and merges A (a1): merge commit M2 = parents
	// {b1, a1}, single base O.
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "B"}); err != nil {
		t.Fatalf("checkout B: %v", err)
	}
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "Y", Create: true}); err != nil {
		t.Fatalf("checkout Y: %v", err)
	}
	if _, err := Merge(MergeOptions{RepoPath: dir, Ref: "A"}); err != nil {
		t.Fatalf("merge A into Y: %v", err)
	}

	// Merging X and Y now has two merge bases {a1, b1}: criss-cross.
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "X"}); err != nil {
		t.Fatalf("checkout X: %v", err)
	}
	_, err := Merge(MergeOptions{RepoPath: dir, Ref: "Y"})
	if err == nil {
		t.Fatalf("expected criss-cross merge to be refused")
	}
	if !strings.Contains(err.Error(), "criss-cross") && !strings.Contains(err.Error(), "multiple merge base") {
		t.Errorf("error = %v, want criss-cross/multiple-merge-base refusal", err)
	}
}
