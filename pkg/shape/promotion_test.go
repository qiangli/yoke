package shape

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestPromotionPatchIDIgnoresCommitIdentity(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(msg, contents string) plumbing.Hash {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "change.txt"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add("change.txt"); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := commit("base", "base\n")
	one := commit("first", "changed\n")
	first, _ := repo.CommitObject(one)
	parent, _ := repo.CommitObject(base)
	id, err := patchID(parent, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: base}); err != nil {
		t.Fatal(err)
	}
	two := commit("second", "changed\n")
	second, _ := repo.CommitObject(two)
	id2, err := patchID(parent, second)
	if err != nil || id != id2 {
		t.Fatalf("patch ids differ: %s %s %v", id, id2, err)
	}
	found, err := promoted(repo, two, id)
	if err != nil || !found {
		t.Fatalf("promoted = %v, %v", found, err)
	}
}
func TestPromotionPushBareAndGHArgs(t *testing.T) {
	t.Setenv("WEAVE_ISSUE", "")
	s, caller, c, _ := submitFixture(t)
	v, err := s.Submit(context.Background(), caller, c.Rev, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "tier.git")
	if _, err := gogit.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	local, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := pushBranch(context.Background(), local, bare, v.Branch); err != nil {
		t.Fatal(err)
	}
	remote, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := remote.Reference(plumbing.NewBranchReferenceName(v.Branch), true)
	if err != nil || ref.Hash().String() != v.ProposalRevision {
		t.Fatalf("pushed ref %v %v", ref, err)
	}
	title, args := ghArgs(v)
	joined := strings.Join(args, " ")
	if title == "" || !strings.Contains(joined, "Shape: "+v.App+"@") || !strings.Contains(joined, "Shaped-By:") || !strings.Contains(joined, v.Evidence) {
		t.Fatalf("gh args %q", joined)
	}
}
