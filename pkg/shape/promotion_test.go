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
	"github.com/qiangli/yoke/pkg/src"
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
	found, err := promoted(repo, two, base, id)
	if err != nil || !found {
		t.Fatalf("promoted = %v, %v", found, err)
	}
}
func TestPromotionPushBareAndGHArgs(t *testing.T) {
	t.Setenv("WEAVE_ISSUE", "")
	s, caller, c, _ := submitFixtureNamed(t, "tester", "yoke")
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
	title, args := ghArgs(v, "owner/repo")
	joined := strings.Join(args, " ")
	if title == "" || !strings.Contains(joined, "--repo owner/repo") || !strings.Contains(joined, "Shape: "+v.App+"@") || !strings.Contains(joined, "Shaped-By:") || !strings.Contains(joined, v.Evidence) {
		t.Fatalf("gh args %q", joined)
	}
}

func TestCompareURL(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"https://github.com/acme/repo.git", "https://github.com/acme/repo/compare/shape-submit/id"},
		{"git@github.com:acme/repo.git", "https://github.com/acme/repo/compare/shape-submit/id"},
		{"/tmp/repo.git", ""},
	} {
		if got := compareURL(tc.remote, "shape-submit/id"); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.remote, got, tc.want)
		}
	}
}
func TestPromotionOutgoingMasterAndPush(t *testing.T) {
	t.Setenv("WEAVE_ISSUE", "")
	s, caller, c, _ := submitFixtureNamed(t, "tester", "yoke")
	v, err := s.Submit(context.Background(), caller, c.Rev, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "tier.git")
	remote, err := gogit.PlainInit(bare, true)
	if err != nil {
		t.Fatal(err)
	}
	local, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		t.Fatal(err)
	}
	base, err := local.CommitObject(plumbing.NewHash(v.BaseRevision))
	if err != nil {
		t.Fatal(err)
	}
	// The proposal base is the remote's master before the cherry-pick.
	if err := remote.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), base.Hash)); err != nil {
		t.Fatal(err)
	}
	if err := remote.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(context.Background(), "upstream", v.ID, false); err == nil || !strings.Contains(err.Error(), "shape remote set upstream <url>") {
		t.Fatalf("unconfigured push: %v", err)
	}
	if err := src.SetTier(s.Options.Home, s.App.Name, "upstream", bare); err != nil {
		t.Fatal(err)
	}
	changes, err := s.Outgoing(context.Background(), "upstream")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].ID != v.ID {
		t.Fatalf("outgoing: %+v", changes)
	}
	pushed, err := s.Push(context.Background(), "upstream", v.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if pushed.CompareURL != "" {
		t.Fatalf("local compare URL: %q", pushed.CompareURL)
	}
	ref, err := remote.Reference(plumbing.NewBranchReferenceName(v.Branch), true)
	if err != nil || ref.Hash().String() != v.ProposalRevision {
		t.Fatalf("push ref: %v %v", ref, err)
	}
	// A distinct commit with the same patch is already promoted.
	proposal, err := local.CommitObject(plumbing.NewHash(v.ProposalRevision))
	if err != nil {
		t.Fatal(err)
	}
	id, err := patchID(base, proposal)
	if err != nil {
		t.Fatal(err)
	}
	found, err := promoted(local, proposal.Hash, base.Hash, id)
	if err != nil || !found {
		t.Fatalf("patch presence: %v %v", found, err)
	}
	if err := remote.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), proposal.Hash)); err != nil {
		t.Fatal(err)
	}
	changes, err = s.Outgoing(context.Background(), "upstream")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("already promoted: %+v", changes)
	}
	// A broken submission is reported without hiding valid entries.
	if err := remote.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), base.Hash)); err != nil {
		t.Fatal(err)
	}
	bad := Submission{ID: "stale", App: s.App.Name, Complete: true, Evidence: "missing", ProposalRevision: v.ProposalRevision, BaseRevision: v.BaseRevision}
	if err := saveSubmission(s.Options.Home, bad); err != nil {
		t.Fatal(err)
	}
	changes, err = s.Outgoing(context.Background(), "upstream")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Status["upstream"] != "warning" && changes[1].Status["upstream"] != "warning" {
		t.Fatalf("stale: %+v", changes)
	}

}
