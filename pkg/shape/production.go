package shape

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/coreutils/pkg/lockfile"
	shapegate "github.com/qiangli/yoke/pkg/shape/gate"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapestate "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Delegator runs the shaping agent in dir. The production implementation is
// `bashy delegate` (shaped_delegate.go); tests inject a fake.
type Delegator interface {
	Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error)
}

// DelegateRequest is what the agent receives: a working directory holding a
// clone of the app source and a task. Nothing else — no principal data, no
// principal credentials.
type DelegateRequest struct {
	Dir  string
	Task string
}

// DelegateResult names who did the work.
type DelegateResult struct {
	Agent    string `json:"agent"`
	Identity string `json:"identity,omitempty"`
	Receipt  string `json:"receipt,omitempty"`
	Output   string `json:"output,omitempty"`
}

// Verifier judges a candidate. The trusted verifier satisfies it.
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (Verdict, error)
}

// VerifyRequest binds a verdict to one candidate.
type VerifyRequest struct {
	App          string
	Principal    string
	SourceDir    string // the app checkout (base revisions live here)
	CandidateDir string // the candidate clone, detached at CandidateRev
	BaseRev      string
	CandidateRev string
	Agent        string
	// Spec is the owner-reviewed spec digest that authorizes grading a
	// spec-changing candidate against its own spec (verify.Options.Spec).
	Spec string
}

// Verdict is the verifier's answer. Only Pass permits activation.
type Verdict struct {
	Artifact     string `json:"artifact,omitempty"`
	ArtifactPath string `json:"artifact_path,omitempty"`
	Policy       string `json:"policy,omitempty"`
	Pass         bool   `json:"pass"`
	Evidence     string `json:"evidence,omitempty"`
	Detail       string `json:"detail,omitempty"`
	// Spec is the digest of the spec the candidate carries; SpecFrom is
	// "candidate" when it was graded against that spec rather than the
	// base's; SpecChange lists the base checks it edits or removes, comma
	// separated (a string keeps Verdict comparable).
	Spec       string `json:"spec,omitempty"`
	SpecFrom   string `json:"spec_from,omitempty"`
	SpecChange string `json:"spec_change,omitempty"`
}

// ProductionVerifier is the console's trusted verifier. Tests may supply a
// runner, but production leaves it nil so verify.Run uses TrustedRunner.
type ProductionVerifier struct {
	Home   string
	Runner shapeverify.Runner
}

// VerifyRunner exposes the check runner approvals verify through, so
// history restores seal through the same runner as approvals.
func (v ProductionVerifier) VerifyRunner() shapeverify.Runner { return v.Runner }

func (v ProductionVerifier) Verify(ctx context.Context, req VerifyRequest) (Verdict, error) {
	home := v.Home
	if home == "" {
		var err error
		home, err = shape.Home()
		if err != nil {
			return Verdict{}, err
		}
	}
	var report shapeverify.Report
	err := withVerifyHead(req.CandidateDir, req.CandidateRev, func() error {
		var rerr error
		report, rerr = shapeverify.Run(ctx, shapeverify.Options{
			Home: home, RepoDir: req.CandidateDir, BaseSHA: req.BaseRev,
			CandidateSHA: req.CandidateRev, Principal: req.Principal, Runner: v.Runner,
			Spec: req.Spec,
		})
		return rerr
	})
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{Pass: report.Passed(), Evidence: report.Path, Detail: report.Record.Reason,
		Spec: report.Record.Spec, SpecFrom: report.Record.SpecFrom, SpecChange: strings.Join(report.Record.SpecChange, ", ")}, nil
}

// withVerifyHead runs fn while the detached candidate workspace dir
// advertises rev on one temporary branch. Candidate workspaces are detached
// by design; go-git's clone transport cannot clone a repository whose HEAD
// is detached, and the verifier clones the workspace.
func withVerifyHead(dir, rev string, fn func() error) error {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return err
	}
	oldHead, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return err
	}
	verifyRef := plumbing.ReferenceName("refs/heads/bashy-shape-verify")
	if err := repo.Storer.SetReference(plumbing.NewHashReference(verifyRef, plumbing.NewHash(rev))); err != nil {
		return err
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, verifyRef)); err != nil {
		_ = repo.Storer.RemoveReference(verifyRef)
		return err
	}
	defer func() {
		_ = repo.Storer.SetReference(oldHead)
		_ = repo.Storer.RemoveReference(verifyRef)
	}()
	return fn()
}

// Activator makes an approved candidate the principal's active revision.
// The versioned activation store satisfies it.
type Activator interface {
	Activate(ctx context.Context, req ActivateRequest) error
}

// ActivateRequest carries everything activation records.
type ActivateRequest struct {
	App          string
	Principal    string
	Author       string
	SourceDir    string
	CandidateDir string
	BaseRev      string
	CandidateRev string
	Request      string
	Agent        string
	Evidence     string
	Trigger      string
}

// ProductionActivator imports the verified candidate into the canonical src
// checkout, then delegates the per-principal switch to pkg/src. The src gate
// reads the sealed verifier evidence again; the Verdict received over HTTP is
// never itself authority to activate.
type ProductionActivator struct {
	gate           shapestate.ActivationGate
	beforeActivate func() error
	Home           string
	mu             sync.Mutex
}

func (a *ProductionActivator) Activate(ctx context.Context, req ActivateRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	home := a.Home
	if home == "" {
		home = src.Home()
	}
	wantSource := filepath.Join(src.SrcDir(home), req.App)
	wantSource, err := filepath.Abs(wantSource)
	if err != nil {
		return err
	}
	gotSource, err := filepath.Abs(req.SourceDir)
	if err != nil {
		return err
	}
	if resolved, rerr := filepath.EvalSymlinks(wantSource); rerr == nil {
		wantSource = resolved
	}
	if resolved, rerr := filepath.EvalSymlinks(gotSource); rerr == nil {
		gotSource = resolved
	}
	if gotSource != wantSource {
		return fmt.Errorf("shaped app %s source is %s, want the managed src checkout %s", req.App, gotSource, wantSource)
	}

	// Every production path (candidate, draft or repair) shares imports into
	// this repository, even when its principal differs.
	if err := (Service{App: App{Name: req.App}}).validate(Caller{Principal: req.Principal}); err != nil {
		return err
	}
	importLock, err := lockfile.AcquireWithin(filepath.Join(src.SrcDir(home), ".shape-import-locks", req.App+".lock"), 30*time.Second, lockfile.Holder{Intent: "shape candidate import"})
	if err != nil {
		return err
	}
	defer importLock.Release()
	candidateRepo, err := gogit.PlainOpen(req.CandidateDir)
	if err != nil {
		return fmt.Errorf("open candidate: %w", err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(nonce[:])
	from := plumbing.ReferenceName("refs/heads/bashy-shape-import-" + id)
	to := plumbing.ReferenceName("refs/bashy-shape/import/" + id)
	if err := candidateRepo.Storer.SetReference(plumbing.NewHashReference(from, plumbing.NewHash(req.CandidateRev))); err != nil {
		return fmt.Errorf("publish candidate object: %w", err)
	}
	defer candidateRepo.Storer.RemoveReference(from)

	sourceRepo, err := gogit.PlainOpen(gotSource)
	if err != nil {
		return fmt.Errorf("open shaped source: %w", err)
	}
	remote := gogit.NewRemote(sourceRepo.Storer, &config.RemoteConfig{Name: "shape-candidate-" + id, URLs: []string{req.CandidateDir}})
	err = remote.FetchContext(ctx, &gogit.FetchOptions{RefSpecs: []config.RefSpec{
		config.RefSpec("+" + from.String() + ":" + to.String()),
	}})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return fmt.Errorf("import candidate: %w", err)
	}
	defer sourceRepo.Storer.RemoveReference(to)

	// The gate is the shared production gate: the sealed verifier
	// evidence is re-checked here, and the Verdict received over HTTP is
	// never itself authority to activate.
	gate := a.gate
	if gate == nil {
		gate = shapegate.New(home, req.Evidence)
	}
	_, err = src.Activate(home, req.App, req.CandidateRev, req.Principal, src.ActivateOptions{PreSwitchHook: a.beforeActivate, ExpectedBase: req.BaseRev, Gate: gate, Request: req.Request, Trigger: req.Trigger, Agent: req.Agent, Author: req.Author})
	return err
}
