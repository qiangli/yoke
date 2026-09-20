package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sprint 224 S3. Each case is one shape from the story's acceptance list; the
// last three reproduce the measured sh #248/#250/#253 reviewed-branch shapes
// (a hand-made `-reviewed` sibling whose patches landed, beside a canonical
// branch that did not).

func writeAndCommit(t *testing.T, dir, name, body, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", name)
	gitT(t, dir, "commit", "-qm", msg)
	return gitT(t, dir, "rev-parse", "HEAD")
}

func TestWeaveRetireBranchShapes(t *testing.T) {
	const canon = "agent/weave-issue-7"
	const reviewed = canon + "-reviewed"
	type check struct {
		deleted bool
		errHas  string
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, root string) (branch, salvage string)
		want  check
	}{
		{"ancestor", func(t *testing.T, root string) (string, string) {
			gitT(t, root, "checkout", "-qb", canon)
			writeAndCommit(t, root, "a.txt", "a\n", "feature")
			gitT(t, root, "checkout", "-q", "main")
			gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge", canon)
			return canon, ""
		}, check{deleted: true}},
		{"cherry-equivalent", func(t *testing.T, root string) (string, string) {
			gitT(t, root, "checkout", "-qb", canon)
			writeAndCommit(t, root, "b.txt", "b\n", "feature b")
			gitT(t, root, "checkout", "-q", "main")
			gitT(t, root, "cherry-pick", canon) // same patch, different sha
			return canon, ""
		}, check{deleted: true}},
		{"unique-patch", func(t *testing.T, root string) (string, string) {
			gitT(t, root, "checkout", "-qb", canon)
			writeAndCommit(t, root, "c.txt", "c\n", "never landed")
			gitT(t, root, "checkout", "-q", "main")
			return canon, ""
		}, check{errHas: "carries a patch not in main"}},
		{"attached-worktree", func(t *testing.T, root string) (string, string) {
			gitT(t, root, "checkout", "-qb", canon)
			writeAndCommit(t, root, "d.txt", "d\n", "feature d")
			gitT(t, root, "checkout", "-q", "main")
			gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge", canon)
			gitT(t, root, "worktree", "add", "-q", filepath.Join(t.TempDir(), "wt"), canon)
			return canon, ""
		}, check{errHas: "checked out in worktree"}},
		{"arbitrary-branch", func(t *testing.T, root string) (string, string) {
			gitT(t, root, "checkout", "-qb", "feature/mine")
			gitT(t, root, "checkout", "-q", "main")
			return "feature/mine", ""
		}, check{deleted: false}}, // never named → never considered
		{"salvaged-rejected", func(t *testing.T, root string) (string, string) {
			gitT(t, root, "checkout", "-qb", canon)
			sha := writeAndCommit(t, root, "e.txt", "e\n", "rejected work")
			gitT(t, root, "checkout", "-q", "main")
			gitT(t, root, "update-ref", "refs/salvage/abandoned-7", sha)
			return canon, "refs/salvage/abandoned-7"
		}, check{deleted: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := setupIsolationFixture(t)
			branch, salvage := tc.setup(t, root)
			it := &weaveItem{ID: 7, Branch: canon, SalvageRef: salvage}
			acts := weaveRetireRunBranches(root, "repo", it)
			var got check
			for _, a := range acts {
				if a.Target == branch {
					got.deleted = a.Done
					got.errHas = a.Err
				}
			}
			if tc.want.deleted != got.deleted || (tc.want.errHas != "" && !strings.Contains(got.errHas, tc.want.errHas)) {
				t.Fatalf("%s: got %+v want %+v (acts %+v)", tc.name, got, tc.want, acts)
			}
			_, exists := gitBranchTip(root, branch)
			if exists == tc.want.deleted && branch != "feature/mine" {
				t.Fatalf("%s: branch exists=%v after retirement", tc.name, exists)
			}
			if branch == "feature/mine" && !exists {
				t.Fatal("an arbitrary user branch was deleted")
			}
		})
	}
	t.Run("moved-tip", func(t *testing.T) {
		root := setupIsolationFixture(t)
		gitT(t, root, "checkout", "-qb", canon)
		writeAndCommit(t, root, "f.txt", "f\n", "feature f")
		gitT(t, root, "checkout", "-q", "main")
		gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge", canon)
		tip, _ := gitBranchTip(root, canon)
		// The proof was taken at `tip`; someone commits before the delete lands.
		gitT(t, root, "checkout", "-q", canon)
		writeAndCommit(t, root, "g.txt", "g\n", "late commit")
		gitT(t, root, "checkout", "-q", "main")
		out, err := gitOut(root, "update-ref", "-d", "refs/heads/"+canon, tip)
		if err == nil {
			t.Fatalf("CAS delete succeeded against a moved tip: %s", out)
		}
		if _, exists := gitBranchTip(root, canon); !exists {
			t.Fatal("moved branch was deleted")
		}
	})
	t.Run("reviewed-shapes-sh-248-250-253", func(t *testing.T) {
		root := setupIsolationFixture(t)
		// canonical: the agent's branch, one patch that was NOT taken as-is
		gitT(t, root, "checkout", "-qb", canon)
		writeAndCommit(t, root, "h.txt", "agent version\n", "agent fix")
		// reviewed: a human rebased/edited copy whose patch is what landed
		gitT(t, root, "checkout", "-qb", reviewed, "main")
		writeAndCommit(t, root, "h.txt", "reviewed version\n", "agent fix (reviewed)")
		gitT(t, root, "checkout", "-q", "main")
		gitT(t, root, "cherry-pick", reviewed)
		it := &weaveItem{ID: 7, Branch: canon}
		acts := weaveRetireRunBranches(root, "repo", it)
		byName := map[string]sprintPruneAction{}
		for _, a := range acts {
			byName[a.Target] = a
		}
		if !byName[reviewed].Done {
			t.Fatalf("integrated -reviewed sibling not retired: %+v", acts)
		}
		if byName[canon].Done || !strings.Contains(byName[canon].Err, "carries a patch") {
			t.Fatalf("canonical branch with a unique patch must be retained: %+v", acts)
		}
		// Once the canonical tip is preserved (abandon --disposition superseded),
		// it retires too.
		tip, _ := gitBranchTip(root, canon)
		gitT(t, root, "update-ref", "refs/salvage/abandoned-7", tip)
		it.SalvageRef = "refs/salvage/abandoned-7"
		acts = weaveRetireRunBranches(root, "repo", it)
		if len(acts) != 1 || !acts[0].Done || acts[0].Target != canon {
			t.Fatalf("superseded canonical not retired after salvage: %+v", acts)
		}
	})
}
