package shape

import (
	"context"
	"errors"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
)

func programFixture(t *testing.T) (Service, Caller, string) {
	t.Helper()
	s, c, input, _, _ := testService(t)
	s.App.Shaping = &fleet.AppShaping{Kind: "cli", Build: []string{"builder", "{output}"}, Test: []string{filepath.Join(t.TempDir(), "checker"), "{artifact}"}}
	if e := os.WriteFile(s.App.Shaping.Test[0], []byte("trusted checker"), 0700); e != nil {
		t.Fatal(e)
	}
	s.Options.Verifier = nil
	s.Options.Activator = nil
	s.Options.ProgramRunner = shapeverify.ProgramRunnerFunc(func(_ context.Context, r shapeverify.ProgramRun) error {
		if r.Argv[0] == "builder" {
			return os.WriteFile(r.Argv[1], []byte("candidate binary"), 0700)
		}
		return nil
	})
	s.App.ReleasePath = filepath.Join(t.TempDir(), "released")
	os.WriteFile(s.App.ReleasePath, []byte("released binary"), 0700)
	base, e := s.active(c.Principal)
	if e != nil {
		t.Fatal(e)
	}
	s.App.ReleaseRevision = base
	repo, e := gogit.PlainOpen(s.App.SourceDir)
	if e != nil {
		t.Fatal(e)
	}
	if e = repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("pristine"), plumbing.NewHash(base))); e != nil {
		t.Fatal(e)
	}
	return s, c, input
}
func TestProgramHostLifecycleAndExactRelease(t *testing.T) {
	s, c, input := programFixture(t)
	ctx := context.Background()
	candidate, e := s.Import(ctx, c, Wish{Request: "native greeting"}, input)
	if e != nil {
		t.Fatal(e)
	}
	approved, e := s.Approve(ctx, c, candidate.ID)
	if e != nil {
		t.Fatal(e)
	}
	if approved.ActivationPrincipal != HostPrincipal || approved.Principal != c.Principal {
		t.Fatal(approved)
	}
	other := c
	other.Principal = "bob"
	status, e := s.HostStatus(ctx, other)
	if e != nil {
		t.Fatal(e)
	}
	if status.SourceRevision != approved.Rev || status.Slot.Active.Revision != approved.Rev {
		t.Fatal(status)
	}
	if _, e = s.Show(ctx, other, candidate.ID); !os.IsNotExist(e) {
		t.Fatalf("candidate crossed principal: %v", e)
	}
	restored, e := s.HostUndo(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	if restored.SourceRevision != s.App.ReleaseRevision || restored.Slot.Active.Digest != restored.Slot.Release.Digest {
		t.Fatal(restored)
	}
	bytes, e := os.ReadFile(restored.Slot.Active.Path)
	if e != nil || string(bytes) != "released binary" {
		t.Fatalf("release rebuilt/substituted: %q %v", bytes, e)
	}
}
func TestProgramUnknownReleaseRefusesActivation(t *testing.T) {
	s, c, input := programFixture(t)
	s.App.ReleaseRevision = ""
	candidate, e := s.Import(context.Background(), c, Wish{Request: "change"}, input)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Approve(context.Background(), c, candidate.ID)
	if e == nil || !strings.Contains(e.Error(), "release") {
		t.Fatalf("unknown installed source accepted: %v", e)
	}
	st := s.slotStore()
	state, e := st.Status()
	if e != nil {
		t.Fatal(e)
	}
	if state.Active != nil {
		t.Fatal("unknown release activated")
	}
}
func TestProgramArtifactAndPolicyCannotDrift(t *testing.T) {
	s, c, input := programFixture(t)
	ctx := context.Background()
	candidate, e := s.Import(ctx, c, Wish{Request: "change"}, input)
	if e != nil {
		t.Fatal(e)
	}
	verified, e := s.Verify(ctx, c, candidate.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !verified.Verdict.Pass {
		t.Fatal(verified.Verdict)
	}
	original := append([]string(nil), s.App.Shaping.Build...)
	s.App.Shaping.Build = append(s.App.Shaping.Build, "policy changed")
	if _, e = s.PreviewProgram(ctx, c, candidate.ID); e == nil {
		t.Fatal("changed trusted policy reused old pass")
	}
	s.App.Shaping.Build = original
	os.WriteFile(verified.ArtifactPath, []byte("tampered"), 0700)
	if _, e = s.PreviewProgram(ctx, c, candidate.ID); e == nil {
		t.Fatal("tampered bytes previewed")
	}
	s.Options.ProgramRunner = shapeverify.ProgramRunnerFunc(func(context.Context, shapeverify.ProgramRun) error { return errors.New("gate failed") })
	if _, e = s.Approve(ctx, c, candidate.ID); e == nil {
		t.Fatal("failed declared gate activated")
	}
}

func TestProgramJournalRecoversArtifactBeforeSourceAndFinalizesAfterSource(t *testing.T) {
	s, c, input := programFixture(t)
	ctx := context.Background()
	candidate, e := s.Import(ctx, c, Wish{Request: "interrupt transition"}, input)
	if e != nil {
		t.Fatal(e)
	}
	verified, e := s.Verify(ctx, c, candidate.ID)
	if e != nil {
		t.Fatal(e)
	}
	artifact, e := s.loadArtifact(verified.Rev)
	if e != nil {
		t.Fatal(e)
	}
	target, e := s.slotStore().Stage(artifact.Path, verified.Rev, artifact.Evidence)
	if e != nil {
		t.Fatal(e)
	}
	transition := programTransition{Candidate: verified, Artifact: artifact, PreviousSource: verified.BaseRev, TargetSlot: &target, ReleasePath: s.App.ReleasePath, Phase: "prepared"}
	if e = s.writeTransition(transition); e != nil {
		t.Fatal(e)
	}
	if e = s.applyProgramTarget(ctx, transition); e != nil {
		t.Fatal(e)
	}
	pending, e := s.HostStatus(ctx, c)
	if e != nil || !pending.Pending {
		t.Fatalf("pending %+v %v", pending, e)
	}
	if e = s.RecoverProgram(ctx); e != nil {
		t.Fatal(e)
	}
	st, e := s.slotStore().Status()
	if e != nil || st.Active == nil || st.Active.Digest != st.Release.Digest {
		t.Fatalf("rollback %+v %v", st, e)
	}
	if _, e = s.readTransition(); !os.IsNotExist(e) {
		t.Fatal("journal not cleared", e)
	}
	approved, e := s.Approve(ctx, c, candidate.ID)
	if e != nil {
		t.Fatal(e)
	}
	transition.Candidate = approved
	transition.Phase = "artifact"
	if e = s.writeTransition(transition); e != nil {
		t.Fatal(e)
	}
	if e = s.RecoverProgram(ctx); e != nil {
		t.Fatal(e)
	}
	status, e := s.HostStatus(ctx, c)
	if e != nil || status.Pending || status.SourceRevision != approved.Rev || status.Slot.Active.Revision != approved.Rev {
		t.Fatalf("finalized %+v %v", status, e)
	}
}
func TestProgramBashyTrustAdapterFrozenHelpLeafEditable(t *testing.T) {
	s, _, _, _, _ := testService(t)
	s.App.Name = "bashy"
	repo, e := gogit.PlainOpen(s.App.SourceDir)
	if e != nil {
		t.Fatal(e)
	}
	wt, _ := repo.Worktree()
	for _, name := range []string{"shape_help.go", "shape_slot.go", "shape_preview.go"} {
		p := filepath.Join(s.App.SourceDir, "internal", "agentos", name)
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, []byte("baseline"), 0600)
		wt.Add(filepath.ToSlash(filepath.Join("internal", "agentos", name)))
	}
	base, e := wt.Commit("baseline adapters", &gogit.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Now()}})
	if e != nil {
		t.Fatal(e)
	}
	guards, e := s.programGuard(s.App.SourceDir, base.String())
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"shape_help.go", "shape_slot.go", "shape_preview.go"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "work")
			r, e := CloneAt(context.Background(), s.App.SourceDir, dir, base.String())
			if e != nil {
				t.Fatal(e)
			}
			os.WriteFile(filepath.Join(dir, "internal", "agentos", name), []byte("changed"), 0600)
			rev, e := CommitCandidate(r, Candidate{Request: "change", Principal: "alice"})
			if e != nil {
				t.Fatal(e)
			}
			err := TouchesProtected(dir, base.String(), rev, guards)
			if (err == nil) != (name == "shape_help.go") {
				t.Fatalf("guard %s: %v", name, err)
			}
		})
	}
}
func TestProgramMismatchedReleaseSourceCannotActivate(t *testing.T) {
	s, c, input := programFixture(t)
	s.App.ReleaseRevision = strings.Repeat("f", 40)
	candidate, e := s.Import(context.Background(), c, Wish{Request: "change"}, input)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Approve(context.Background(), c, candidate.ID); e == nil {
		t.Fatal("mismatched installed source accepted")
	}
	active, e := s.hostSource()
	if e != nil || active != candidate.BaseRev {
		t.Fatal(active, e)
	}
}
func TestProgramNewCheckerFilesRefused(t *testing.T) {
	for _, name := range []string{"bypass_test.go", "testdata/replacement.txt", "scripts/check.sh", "go.mod"} {
		t.Run(name, func(t *testing.T) {
			s, c, input := programFixture(t)
			p := filepath.Join(input, filepath.FromSlash(name))
			os.MkdirAll(filepath.Dir(p), 0700)
			os.WriteFile(p, []byte("candidate checker"), 0600)
			candidate, e := s.Import(context.Background(), c, Wish{Request: "replace checker"}, input)
			if e != nil {
				t.Fatal(e)
			}
			verified, e := s.Verify(context.Background(), c, candidate.ID)
			if !errors.Is(e, ErrVerification) || verified.Verdict.Pass {
				t.Fatalf("checker accepted %+v %v", verified.Verdict, e)
			}
		})
	}
}
func TestProgramContainerBuildCriteriaFrozen(t *testing.T) {
	s, _, _, _, _ := testService(t)
	s.App.Shaping = &fleet.AppShaping{Kind: "service"}
	repo, e := gogit.PlainOpen(s.App.SourceDir)
	if e != nil {
		t.Fatal(e)
	}
	wt, _ := repo.Worktree()
	names := []string{"Dockerfile", "Dockerfile.build", "Containerfile", ".dockerignore", ".containerignore", "app/package.json", "app/yarn.lock", "app/pnpm-lock.yaml", "go.work"}
	for _, name := range names {
		p := filepath.Join(s.App.SourceDir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, []byte("trusted criterion"), 0600)
		wt.Add(name)
	}
	base, e := wt.Commit("trusted container build inputs", &gogit.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Now()}})
	if e != nil {
		t.Fatal(e)
	}
	guards, e := s.programGuard(s.App.SourceDir, base.String())
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "work")
			r, e := CloneAt(context.Background(), s.App.SourceDir, dir, base.String())
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte("candidate replaced criterion"), 0600); e != nil {
				t.Fatal(e)
			}
			rev, e := CommitCandidate(r, Candidate{Request: "tamper", Principal: "alice"})
			if e != nil {
				t.Fatal(e)
			}
			if e = TouchesProtected(dir, base.String(), rev, guards); e == nil {
				t.Fatal("candidate changed frozen build criterion")
			}
		})
	}
}
