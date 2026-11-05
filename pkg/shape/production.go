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
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
	"path/filepath"
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
}

// Verdict is the verifier's answer. Only Pass permits activation.
type Verdict struct {
	Pass     bool   `json:"pass"`
	Evidence string `json:"evidence,omitempty"`
	Detail   string `json:"detail,omitempty"`
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
	// Candidate workspaces are detached by design. Give the verifier's local
	// clone one temporary advertised branch; go-git's clone transport cannot
	// clone a repository whose HEAD is detached.
	repo, err := gogit.PlainOpen(req.CandidateDir)
	if err != nil {
		return Verdict{}, err
	}
	oldHead, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return Verdict{}, err
	}
	verifyRef := plumbing.ReferenceName("refs/heads/bashy-shape-verify")
	if err := repo.Storer.SetReference(plumbing.NewHashReference(verifyRef, plumbing.NewHash(req.CandidateRev))); err != nil {
		return Verdict{}, err
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, verifyRef)); err != nil {
		_ = repo.Storer.RemoveReference(verifyRef)
		return Verdict{}, err
	}
	defer func() {
		_ = repo.Storer.SetReference(oldHead)
		_ = repo.Storer.RemoveReference(verifyRef)
	}()

	report, err := shapeverify.Run(ctx, shapeverify.Options{
		Home: home, RepoDir: req.CandidateDir, BaseSHA: req.BaseRev,
		CandidateSHA: req.CandidateRev, Principal: req.Principal, Runner: v.Runner,
	})
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{Pass: report.Passed(), Evidence: report.Path, Detail: report.Record.Reason}, nil
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
	Home string
	mu   sync.Mutex
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
	gate := shapegate.New(home, req.Evidence)
	_, err = src.Activate(home, req.App, req.CandidateRev, req.Principal, src.ActivateOptions{ExpectedBase: req.BaseRev, Gate: gate, Request: req.Request, Trigger: req.Trigger})
	return err
}
