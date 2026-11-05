package shape

import (
	"context"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/src"
)

func TestNativeSubmitNormalMergeProof(t *testing.T) {
	s, caller, input := programFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", s.Options.Home)
	apps := t.TempDir()
	t.Setenv("BASHY_APPS_DIR", apps)
	s.Options.Track = NewTracking(s.Options.Home)
	body, err := fleet.Marshal(fleet.App{Name: s.App.Name, Kind: fleet.KindApp, Type: "shaped", Source: s.App.SourceDir, Shaping: s.App.Shaping})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(apps, s.App.Name+".yaml")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.Import(context.Background(), caller, Wish{Request: "native proposal"}, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Approve(context.Background(), caller, candidate.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.Submit(context.Background(), caller, candidate.ID, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := src.GetPR(s.Options.Home, result.PR)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Shape.Kind != "cli" {
		t.Fatal(pr.Shape)
	}
	// The ordinary merge entry point must consume the native seal and recheck
	// current trusted policy, rather than recomputing a web proof.
	saved := src.ShapeProgramEvidence
	src.ShapeProgramEvidence = nil
	if _, err = src.MergePR(s.Options.Home, pr.ID); err == nil {
		t.Fatal("missing resolver accepted")
	}
	src.ShapeProgramEvidence = saved
	defer func() { src.ShapeProgramEvidence = saved }()
	artifact, err := s.loadArtifact(result.ProposalRevision)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(artifact.Path, []byte("tampered"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = src.MergePR(s.Options.Home, pr.ID); err == nil {
		t.Fatal("tampered final proposal artifact accepted")
	}
	if err = os.WriteFile(artifact.Path, original, 0700); err != nil {
		t.Fatal(err)
	}
	changed := append([]string(nil), s.App.Shaping.Build...)
	s.App.Shaping.Build = append(changed, "changed-policy")
	drift, err := fleet.Marshal(fleet.App{Name: s.App.Name, Kind: fleet.KindApp, Type: "shaped", Source: s.App.SourceDir, Shaping: s.App.Shaping})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, drift, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = src.MergePR(s.Options.Home, pr.ID); err == nil {
		t.Fatal("changed trusted policy accepted")
	}
	oldVerify := src.ShapeProgramVerify
	src.ShapeProgramVerify = nil
	if _, err = src.VerifyPR(context.Background(), s.Options.Home, pr.ID, nil); err == nil {
		t.Fatal("missing refresh resolver accepted")
	}
	src.ShapeProgramVerify = oldVerify
	defer func() { src.ShapeProgramVerify = oldVerify }()
	oldFactory := programSubmissionService
	programSubmissionService = func(app App, home string) Service {
		return Service{App: app, Options: Options{Home: home, ProgramRunner: s.Options.ProgramRunner}}
	}
	defer func() { programSubmissionService = oldFactory }()
	// A valid PR cannot refresh after its durable source identity changes.
	changedSubmission := result
	changedSubmission.SourceRevision = result.BaseRevision
	if err = saveSubmission(s.Options.Home, changedSubmission); err != nil {
		t.Fatal(err)
	}
	if _, err = src.VerifyPR(context.Background(), s.Options.Home, pr.ID, nil); err == nil {
		t.Fatal("changed source identity refreshed")
	}
	if err = saveSubmission(s.Options.Home, result); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		t.Fatal(err)
	}
	local := plumbing.NewBranchReferenceName("local")
	if err = repo.Storer.SetReference(plumbing.NewHashReference(local, plumbing.NewHash(result.SourceRevision))); err != nil {
		t.Fatal(err)
	}
	if _, err = src.VerifyPR(context.Background(), s.Options.Home, pr.ID, nil); err == nil {
		t.Fatal("advanced integration base refreshed")
	}
	if err = repo.Storer.SetReference(plumbing.NewHashReference(local, plumbing.NewHash(result.BaseRevision))); err != nil {
		t.Fatal(err)
	}
	if _, err = src.VerifyPR(context.Background(), s.Options.Home, pr.ID, nil); err != nil {
		t.Fatalf("normal native proof refresh: %v", err)
	}

	if _, err = src.MergePR(s.Options.Home, pr.ID); err != nil {
		t.Fatalf("normal native merge: %v", err)
	}
}
