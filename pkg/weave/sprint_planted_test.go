package weave

import (
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"os"
	"strings"
	"testing"
)

func TestPlantPrivateOutcomesAndScorecard(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", "")
	rec := sprintPlantedRecord{Run: "repo#1", Kind: "logic", File: "code.go", Line: 20, PatchDigest: "digest"}
	if err := sprintPlantedAdd(331, rec); err != nil {
		t.Fatal(err)
	}
	if err := sprintPlantedReview(331, "repo#1", "agent-a", "request-changes", []string{"code.go:23"}, false); err != nil {
		t.Fatal(err)
	}
	if err := sprintPlantedAdd(331, sprintPlantedRecord{Run: "repo#2", Kind: "logic", File: "code.go", Line: 20, PatchDigest: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := sprintPlantedReview(331, "repo#2", "agent-a", "request-changes", []string{"code.go:24"}, false); err != nil {
		t.Fatal(err)
	}
	if err := sprintPlantedReview(331, "repo#3", "agent-a", "request-changes", nil, false); err != nil {
		t.Fatal(err)
	}
	got, err := sprintPlantedCounts(331)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlantedDefects != 2 || got.PlantedCaught != 1 || got.FalseRejections != 0 {
		t.Fatalf("counts: %+v", got)
	}
	if err := sprintPlantedReview(331, "repo#3", "panel", "approve", nil, true); err != nil {
		t.Fatal(err)
	}
	got, err = sprintPlantedCounts(331)
	if err != nil || got.FalseRejections != 1 {
		t.Fatalf("overturn: %+v %v", got, err)
	}
	path, err := sprintPlantedReviewPath(331)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "digest") {
		t.Fatal("private record missing digest")
	}
	if mode := func() os.FileMode { info, _ := os.Stat(path); return info.Mode().Perm() }(); mode != 0600 {
		t.Fatalf("mode %o", mode)
	}
	s := &weaveStory{ID: 331}
	in, _ := sprintScorecardEvidence(nil, s, 1)
	if in.PlantedDefects != 2 || in.PlantedCaught != 1 || in.FalseRejections != 1 {
		t.Fatalf("scorecard: %+v", in)
	}
}

func TestPlantSeparateRef(t *testing.T) {
	dir, _, head := sprintGradeFixture(t)
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName("refs/sprint-grade/attempt"), plumbing.NewHash(head))); err != nil {
		t.Fatal(err)
	}
	patch := []byte("--- a/code.go\n+++ b/code.go\n@@ -1 +1 @@\n-implementation\n+defect\n")
	ref, err := sprintPlantedRef(sprintGradeEvent{Checkout: dir, Commit: head, Verdict: "pass"}, patch, "code.go")
	if err != nil {
		t.Fatal(err)
	}
	bits := strings.SplitN(ref, ":", 2)
	if len(bits) != 2 {
		t.Fatalf("ref %q", ref)
	}
	planted, err := gogit.PlainOpen(bits[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := planted.Reference(plumbing.ReferenceName(bits[1]), true)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := planted.CommitObject(got.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if commit.ParentHashes[0].String() != head {
		t.Fatal("plant must descend from graded commit")
	}
	orig, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if orig.Hash().String() != head {
		t.Fatal("graded attempt was modified")
	}
}
