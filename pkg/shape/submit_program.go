package shape

import (
	"context"
	"errors"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/coreutils/pkg/lockfile"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/src"
)

var programSubmissionService = func(app App, home string) Service { return Service{App: app, Options: Options{Home: home}} }

func programSubmissionContext(home, dir string, pr src.PR, base string) (Service, Submission, error) {
	if pr.Shape == nil {
		return Service{}, Submission{}, errors.New("shape: missing submission identity")
	}
	v, err := loadSubmission(home, pr.Shape.Submission)
	if err != nil {
		return Service{}, Submission{}, err
	}
	if v.App != pr.Repo || v.ProposalRevision != pr.HeadSHA || v.BaseRevision != base || v.PR != pr.ID || filepath.Clean(v.SourceDir) != filepath.Clean(dir) {
		return Service{}, Submission{}, errors.New("shape: proposal binding changed")
	}
	if _, err = submittedItem(home, v); err != nil {
		return Service{}, Submission{}, err
	}
	registered, ok := fleet.New().App(v.App)
	if !ok || registered.Shaping == nil || registered.Shaping.Kind != pr.Shape.Kind || filepath.Clean(registered.Source) != filepath.Clean(dir) {
		return Service{}, Submission{}, errors.New("shape: trusted native app registration changed or unavailable")
	}
	s := programSubmissionService(App{Name: registered.Name, SourceDir: registered.Source, Shaping: registered.Shaping}, home)
	return s, v, nil
}

func resolveProgramSubmissionEvidence(ctx context.Context, home, dir string, pr src.PR, base string) (evidence.Record, error) {
	s, v, err := programSubmissionContext(home, dir, pr, base)
	if err != nil {
		return evidence.Record{}, err
	}
	a, err := s.requireProgram(ctx, Candidate{Rev: pr.HeadSHA, BaseRev: base})
	if err != nil {
		return evidence.Record{}, err
	}
	if a.Evidence != v.Evidence || pr.Shape.Evidence != v.Evidence {
		return evidence.Record{}, errors.New("shape: proposal evidence identity changed")
	}
	return evidence.Require(home, evidence.Binding{Candidate: pr.HeadSHA, Base: base, Principal: HostPrincipal, Policy: a.Policy, Artifact: a.Digest})
}

func refreshProgramSubmissionEvidence(ctx context.Context, home, dir string, pr src.PR, base string) (evidence.Record, error) {
	lock, err := lockfile.AcquireWithin(filepath.Join(submissionRoot(home), ".locks", pr.Repo+".lock"), 30*time.Second, lockfile.Holder{Intent: "refresh shaped proposal proof"})
	if err != nil {
		return evidence.Record{}, err
	}
	defer lock.Release()
	s, v, err := programSubmissionContext(home, dir, pr, base)
	if err != nil {
		return evidence.Record{}, err
	}
	// Merge base alone can stay old after local advances. Refresh only while
	// the proposal's integration parent remains the current local tip.
	repo, err := programLocalRevision(dir)
	if err != nil {
		return evidence.Record{}, err
	}
	if repo != base {
		return evidence.Record{}, errors.New("shape: integration base advanced; rebase proposal before verification")
	}
	temp, err := os.MkdirTemp("", "shape-proposal-verify-")
	if err != nil {
		return evidence.Record{}, err
	}
	defer os.RemoveAll(temp)
	work := filepath.Join(temp, "work")
	if _, err = CloneAt(ctx, dir, work, pr.HeadSHA); err != nil {
		return evidence.Record{}, err
	}
	verdict, err := s.Verifier().Verify(ctx, VerifyRequest{App: v.App, Principal: HostPrincipal, SourceDir: dir, CandidateDir: work, BaseRev: base, CandidateRev: pr.HeadSHA})
	if err != nil {
		return evidence.Record{}, err
	}
	rec, err := evidence.Require(home, evidence.Binding{Candidate: pr.HeadSHA, Base: base, Principal: HostPrincipal, Verifier: shapeverify.BuildID(), Policy: verdict.Policy, Artifact: verdict.Artifact})
	if err != nil {
		return evidence.Record{}, err
	}
	if _, _, err = programSubmissionContext(home, dir, pr, base); err != nil {
		return evidence.Record{}, err
	}
	if current, e := programLocalRevision(dir); e != nil || current != base {
		return evidence.Record{}, errors.New("shape: integration base changed during verification")
	}
	v.Evidence = verdict.Evidence
	if err = saveSubmission(home, v); err != nil {
		return evidence.Record{}, err
	}
	return rec, nil
}

func programLocalRevision(dir string) (string, error) {
	r, e := gogit.PlainOpen(dir)
	if e != nil {
		return "", e
	}
	ref, e := r.Reference(plumbing.NewBranchReferenceName("local"), true)
	if e != nil {
		return "", e
	}
	return ref.Hash().String(), nil
}
