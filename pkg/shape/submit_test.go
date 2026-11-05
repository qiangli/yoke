package shape

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/weave"
)

type submitVerifier struct {
	requests []VerifyRequest
	pass     bool
}

func (v *submitVerifier) Verify(_ context.Context, r VerifyRequest) (Verdict, error) {
	v.requests = append(v.requests, r)
	return Verdict{Pass: v.pass, Evidence: "focused-test-evidence"}, nil
}

func submitFixture(t *testing.T) (Service, Caller, Candidate, *submitVerifier) {
	return submitFixtureActor(t, "tester")
}
func submitFixtureActor(t *testing.T, actor string) (Service, Caller, Candidate, *submitVerifier) {
	t.Helper()
	s, caller, from, _, _ := testService(t)
	caller.Actor = actor
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", s.Options.Home)
	v := &submitVerifier{pass: true}
	s.Options.Verifier = v
	s.Options.Track = NewTracking(s.Options.Home)
	c, err := s.Import(context.Background(), caller, Wish{Request: "Change greeting", TrackingRoot: filepath.Join(s.Options.Home, "todo", "fixture")}, from)
	if err != nil {
		t.Fatal(err)
	}
	// The lifecycle fake activator does not import Git objects. Seed precisely
	// that side effect so this test focuses on proposal, tracking and run linkage.
	candidate, err := gogit.PlainOpen(c.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	source, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := candidate.Storer.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		t.Fatal(err)
	}
	if err = objects.ForEach(func(o plumbing.EncodedObject) error { _, err := source.Storer.SetEncodedObject(o); return err }); err != nil {
		t.Fatal(err)
	}
	for name, rev := range map[string]string{"pristine": c.BaseRev, "approved-fixture": c.Rev} {
		if err = source.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), plumbing.NewHash(rev))); err != nil {
			t.Fatal(err)
		}
	}
	c, err = s.Approve(context.Background(), caller, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, caller, c, v
}
func TestSubmitApprovedVersionIsVerifiedAndIdempotent(t *testing.T) {
	s, caller, c, v := submitFixture(t)
	caller.Actor = "reviewer"
	ctx := context.Background()
	result, err := s.Submit(ctx, caller, c.Rev, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || result.PR == "" || result.Run.ID == 0 || result.SourceRevision != c.Rev || result.ProposalRevision == c.Rev {
		t.Fatalf("incomplete submission %#v", result)
	}
	checked := v.requests[len(v.requests)-1]
	if checked.CandidateRev != result.ProposalRevision || checked.BaseRev != result.BaseRevision {
		t.Fatalf("wrong proposal verified: %#v", checked)
	}
	repo, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(result.ProposalRevision))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commit.Message, "Adhoc:") || strings.Contains(commit.Message, "Sprint:") || strings.Contains(commit.Message, result.TrackingRoot) {
		t.Fatalf("bad ordinary-todo provenance %q", commit.Message)
	}
	if _, err := src.MergePR(s.Options.Home, result.PR); err == nil {
		t.Fatal("submission bypassed normal merge evidence gates")
	}
	oldResolver := weave.ShapeSubmissionResolver
	weave.ShapeSubmissionResolver = ResolveSubmittedShapeItem
	defer func() { weave.ShapeSubmissionResolver = oldResolver }()
	if err = weave.ValidateShapeCommitMessage(commit.Message); err != nil {
		t.Fatal(err)
	}
	again, err := s.Submit(ctx, caller, c.ID, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != result.ID || again.PR != result.PR || again.Run.ID != result.Run.ID {
		t.Fatalf("duplicate submission %#v %#v", result, again)
	}
	// Recover after a final-record save was interrupted: the PR and run already
	// exist, and must be reused rather than allocated again.
	interrupted := result
	interrupted.Complete = false
	interrupted.PR = ""
	if err = saveSubmission(s.Options.Home, interrupted); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Submit(ctx, caller, c.Rev, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prs, err := src.ListPRs(s.Options.Home, c.App)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.PR != result.PR || recovered.Run.ID != result.Run.ID || len(prs) != 1 || prs[0].State != src.PRStateOpen || prs[0].Author != caller.Actor {
		t.Fatalf("retry duplicated or merged records: %#v %#v", recovered, prs)
	}
	store := &issue.Store{Root: result.TrackingRoot, Sub: "."}
	item, err := store.Resolve(strings.TrimPrefix(result.Item, "todo:"))
	if err != nil {
		t.Fatal(err)
	}
	if item.Sprint != 0 || item.Weave != result.Run.ID || strings.Count(item.Body, "Shape submission "+result.ID+":") != 1 {
		t.Fatalf("bad reciprocal item %#v", item)
	}
}
func TestSubmitStoryAndFailureBoundaries(t *testing.T) {
	s, caller, c, v := submitFixture(t)
	ctx := context.Background()
	store := &issue.Store{Root: c.TrackingRoot, Sub: "."}
	story := &issue.Issue{ID: "abc123def456", Title: "Review shape", Kind: issue.KindTask, Status: "todo", Seq: 8, Sprint: 564, SprintID: "0f09b646-9acd-5bec-849d-2cf3b0e4edef"}
	if _, err := store.Save(story); err != nil {
		t.Fatal(err)
	}
	wrong := caller
	wrong.Principal = "other-person"
	if _, err := s.Submit(ctx, wrong, c.Rev, SubmitOptions{}); err == nil {
		t.Fatal("another principal submitted version")
	}
	v.pass = false
	if _, err := s.Submit(ctx, caller, c.Rev, SubmitOptions{Story: story.ID}); !errors.Is(err, ErrVerification) {
		t.Fatalf("failed verification accepted: %v", err)
	}
	prs, err := src.ListPRs(s.Options.Home, c.App)
	if err != nil || len(prs) != 0 {
		t.Fatalf("failed proposal opened: %v %#v", err, prs)
	}
	v.pass = true
	got, err := s.Submit(ctx, caller, c.Rev, SubmitOptions{Story: story.ID})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := gogit.PlainOpen(s.App.SourceDir)
	commit, err := repo.CommitObject(plumbing.NewHash(got.ProposalRevision))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commit.Message, "Story-ID: "+story.ID) || !strings.Contains(commit.Message, "Sprint: #564") || strings.Contains(commit.Message, "Adhoc:") {
		t.Fatalf("bad story provenance: %s", commit.Message)
	}
	// Guard detects a proposal ref changed after its verification and PR record.
	if err = repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(got.Branch), plumbing.NewHash(c.BaseRev))); err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveSubmittedShapeItem(c.App, c.Rev, story.ID); err == nil {
		t.Fatal("changed proposal identity accepted")
	}
}

func TestSubmitPersistsAgentAttributionBeforeVerification(t *testing.T) {
	fleetRoot := t.TempDir()
	t.Setenv("BASHY_FLEET_DIR", fleetRoot)
	t.Setenv("BASHY_AGENTS_DIR", filepath.Join(fleetRoot, "agents"))
	cat := fleet.New(fleet.WithRoot(fleetRoot), fleet.WithoutCloudOverlay())
	if err := cat.SaveAgent(fleet.Agent{Name: "submit-agent", Tool: "submit-tool", Model: "submit-model"}); err != nil {
		t.Fatal(err)
	}
	s, caller, c, v := submitFixtureActor(t, "submit-agent")
	store := &issue.Store{Root: c.TrackingRoot, Sub: "."}
	item := &issue.Issue{ID: "abc123def456", Title: "Assigned review", Kind: issue.KindTask, Status: "doing", Seq: 7, Sprint: 564, Assignee: "submit-agent"}
	if _, err := store.Save(item); err != nil {
		t.Fatal(err)
	}
	got, err := s.Submit(context.Background(), caller, c.Rev, SubmitOptions{Story: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := gogit.PlainOpen(s.App.SourceDir)
	commit, err := repo.CommitObject(plumbing.NewHash(got.ProposalRevision))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commit.Message, "Agent: submit-agent\n") {
		t.Fatalf("missing stored creator attribution: %s", commit.Message)
	}
	checked := v.requests[len(v.requests)-1]
	if checked.CandidateRev != commit.Hash.String() {
		t.Fatal("attribution added after verification")
	}
}
