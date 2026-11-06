package shape

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/shape/evidence"
	state "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

const HostPrincipal = "host"

func (s Service) Kind() string {
	if s.App.Shaping == nil || s.App.Shaping.Kind == "" {
		return "web"
	}
	return s.App.Shaping.Kind
}
func (s Service) Scope() string {
	if s.Kind() == "web" {
		return "principal"
	}
	return "host"
}
func (s Service) ActivationPrincipal(principal string) string {
	if s.Kind() == "web" {
		return principal
	}
	return HostPrincipal
}
func (s Service) Verifier() Verifier {
	if s.Kind() == "web" {
		return s.Options.Verifier
	}
	return programVerifier{service: s}
}

// ProgramArtifact is verifier-owned metadata outside every candidate checkout.
type ProgramArtifact struct {
	App       string `json:"app"`
	Kind      string `json:"kind"`
	Revision  string `json:"revision"`
	Base      string `json:"base"`
	Principal string `json:"principal"`
	Digest    string `json:"digest"`
	Path      string `json:"path"`
	Policy    string `json:"policy"`
	Evidence  string `json:"evidence"`
}

func (s Service) artifactRecord(revision string) (string, error) {
	home, e := s.home()
	if e != nil {
		return "", e
	}
	if len(revision) != 40 {
		return "", errors.New("shape: artifact needs exact source revision")
	}
	if _, e = hex.DecodeString(revision); e != nil {
		return "", e
	}
	return filepath.Join(home, "src", ".shape-programs", s.App.Name, revision+".json"), nil
}
func (s Service) saveArtifact(v ProgramArtifact) error {
	path, e := s.artifactRecord(v.Revision)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".program-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	return syncProgramDir(filepath.Dir(path))
}
func (s Service) loadArtifact(revision string) (ProgramArtifact, error) {
	path, e := s.artifactRecord(revision)
	if e != nil {
		return ProgramArtifact{}, e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return ProgramArtifact{}, e
	}
	var v ProgramArtifact
	e = json.Unmarshal(b, &v)
	if e == nil && (v.App != s.App.Name || v.Revision != revision || v.Kind != s.Kind() || v.Principal != HostPrincipal) {
		e = errors.New("shape: artifact identity mismatch")
	}
	return v, e
}

// programGuard freezes the known Go/build gate sources and the existing trust
// manifest. This MVP refuses both edits and additions to native checker files.
func (s Service) programGuard(repoDir, base string) ([]string, error) {
	r, e := gogit.PlainOpen(repoDir)
	if e != nil {
		return nil, e
	}
	c, e := r.CommitObject(plumbing.NewHash(base))
	if e != nil {
		return nil, e
	}
	tree, e := c.Tree()
	if e != nil {
		return nil, e
	}
	guards := append([]string(nil), s.App.Protected...)
	for _, row := range src.ProtectedManifest() {
		if row.Repo == s.App.Name {
			guards = append(guards, row.Path)
		}
	}
	if s.App.Name == "bashy" {
		guards = append(guards, "internal/cli", "cmd/bashy", "cmd/bash", "internal/agentos/agentos.go", "internal/agentos/registered.go", "internal/agentos/shape.go", "internal/agentos/shape_local.go", "internal/agentos/shape_remote.go", "internal/agentos/shape_slot.go", "internal/agentos/shape_preview.go")
	}
	if s.App.Name == "yoke" {
		guards = append(guards, "pkg/shape", "pkg/src")
	}
	e = tree.Files().ForEach(func(f *object.File) error {
		p := f.Name
		criterion := programCriterion(p)
		if criterion {
			guards = append(guards, p)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	if s.App.Shaping != nil {
		for _, argv := range [][]string{s.App.Shaping.Build, s.App.Shaping.Test} {
			for _, a := range argv {
				p := strings.TrimPrefix(a, "./")
				if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "-") || strings.Contains(p, "{") {
					continue
				}
				// Working directories/package selectors are not gate files.
				// Explicit protected directories above remain protected.
				if entry, e := tree.FindEntry(filepath.ToSlash(p)); e == nil && entry.Mode.IsFile() {
					guards = append(guards, filepath.ToSlash(p))
				}
			}
		}
	}
	sort.Strings(guards)
	out := guards[:0]
	for _, p := range guards {
		if len(out) == 0 || out[len(out)-1] != p {
			out = append(out, p)
		}
	}
	return out, nil
}
func (s Service) programPolicy(ctx context.Context, repo, base string) (string, []string, error) {
	if s.App.Shaping == nil {
		return "", nil, errors.New("shape: no trusted program policy")
	}
	if e := s.App.Shaping.Validate(); e != nil {
		return "", nil, e
	}
	guards, e := s.programGuard(repo, base)
	if e != nil {
		return "", nil, e
	}
	var unit any
	if s.Kind() == "service" {
		u, e := s.serviceUnit(ctx)
		if e != nil {
			return "", nil, e
		}
		unit = servicePolicy(u)
	}
	b, e := json.Marshal(struct {
		Schema  string
		Config  any
		Guards  []string
		Service any
	}{"shape-program-policy-v1", s.App.Shaping, guards, unit})
	if e != nil {
		return "", nil, e
	}
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:]), guards, nil
}

type programVerifier struct {
	service  Service
	retained string
}

func (p programVerifier) Verify(ctx context.Context, req VerifyRequest) (verdict Verdict, verifyErr error) {
	s := p.service
	home, e := s.home()
	if e != nil {
		return Verdict{}, e
	}
	policy, guards, e := s.programPolicy(ctx, req.CandidateDir, req.BaseRev)
	if e != nil {
		return Verdict{}, e
	}
	if e = TouchesProtected(req.CandidateDir, req.BaseRev, req.CandidateRev, guards); e != nil {
		return Verdict{Pass: false, Detail: e.Error()}, nil
	}
	baseRepo, e := gogit.PlainOpen(req.CandidateDir)
	if e != nil {
		return Verdict{}, e
	}
	baseCommit, e := baseRepo.CommitObject(plumbing.NewHash(req.BaseRev))
	if e != nil {
		return Verdict{}, e
	}
	baseTree, e := baseCommit.Tree()
	if e != nil {
		return Verdict{}, e
	}
	candidateCommit, e := baseRepo.CommitObject(plumbing.NewHash(req.CandidateRev))
	if e != nil {
		return Verdict{}, e
	}
	candidateTree, e := candidateCommit.Tree()
	if e != nil {
		return Verdict{}, e
	}
	if e = candidateTree.Files().ForEach(func(f *object.File) error {
		if programCriterion(f.Name) {
			if _, e := baseTree.FindEntry(f.Name); e != nil {
				return fmt.Errorf("native shaping cannot add checker/test files: %s; submit through ordinary upstream review", f.Name)
			}
		}
		return nil
	}); e != nil {
		return Verdict{Pass: false, Detail: e.Error()}, nil
	}
	// Match the web verifier's advertised temporary branch for detached clones.
	r, e := gogit.PlainOpen(req.CandidateDir)
	if e != nil {
		return Verdict{}, e
	}
	old, e := r.Storer.Reference(plumbing.HEAD)
	if e != nil {
		return Verdict{}, e
	}
	// Both revisions must be advertised: restoring an ancestor otherwise drops
	// its descendant baseline when the verifier makes another isolated clone.
	refs := []struct {
		name     plumbing.ReferenceName
		revision string
	}{
		{plumbing.NewBranchReferenceName("bashy-shape-program-verify"), req.CandidateRev},
		{plumbing.NewBranchReferenceName("bashy-shape-program-base"), req.BaseRev},
	}
	previous := make(map[plumbing.ReferenceName]*plumbing.Reference)
	defer func() {
		var restoreErr error
		restoreErr = errors.Join(restoreErr, r.Storer.SetReference(old))
		for name, ref := range previous {
			if ref != nil {
				restoreErr = errors.Join(restoreErr, r.Storer.SetReference(ref))
			} else {
				restoreErr = errors.Join(restoreErr, r.Storer.RemoveReference(name))
			}
		}
		if restoreErr != nil {
			verdict = Verdict{}
			verifyErr = errors.Join(verifyErr, fmt.Errorf("restore verifier references: %w", restoreErr))
		}
	}()
	for _, entry := range refs {
		prior, err := r.Storer.Reference(entry.name)
		if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return Verdict{}, err
		}
		previous[entry.name] = prior
		if e = r.Storer.SetReference(plumbing.NewHashReference(entry.name, plumbing.NewHash(entry.revision))); e != nil {
			return Verdict{}, e
		}
	}
	if e = r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, refs[0].name)); e != nil {
		return Verdict{}, e
	}
	var checkArtifact func(context.Context, string, string) error
	if s.Kind() == "service" {
		checkArtifact = func(ctx context.Context, digest, path string) error {
			l, e := s.hostLock()
			if e != nil {
				return e
			}
			defer l.Release()
			id := "verify-" + req.CandidateRev[:12]
			preview, e := s.startServicePreview(ctx, id, ProgramArtifact{Digest: digest, Path: path})
			if e != nil {
				return e
			}
			return s.cleanupProgramPreview(context.WithoutCancel(ctx), Candidate{ID: id, ProgramPreview: &preview})
		}
	}
	report, e := shapeverify.RunProgram(ctx, shapeverify.ProgramOptions{Home: home, App: s.App.Name, Kind: s.Kind(), RepoDir: req.CandidateDir, BaseSHA: req.BaseRev, CandidateSHA: req.CandidateRev, Principal: HostPrincipal, Policy: policy, Build: s.App.Shaping.Build, Test: s.App.Shaping.Test, GuardedPaths: guards, Runner: s.Options.ProgramRunner, RetainedArtifact: p.retained, CheckArtifact: checkArtifact})
	if e != nil {
		return Verdict{}, e
	}
	v := Verdict{Pass: report.Record.Verdict == evidence.VerdictPass, Evidence: report.Path, Detail: report.Record.Reason, Artifact: report.Artifact, ArtifactPath: report.ArtifactPath, Policy: policy}
	if v.Pass {
		e = s.saveArtifact(ProgramArtifact{App: s.App.Name, Kind: s.Kind(), Revision: req.CandidateRev, Base: req.BaseRev, Principal: HostPrincipal, Digest: report.Artifact, Path: report.ArtifactPath, Policy: policy, Evidence: report.Path})
	}
	return v, e
}
func (s Service) requireProgram(ctx context.Context, c Candidate) (ProgramArtifact, error) {
	a, e := s.loadArtifact(c.Rev)
	if e != nil {
		return a, e
	}
	policy, _, e := s.programPolicy(ctx, s.App.SourceDir, c.BaseRev)
	if e != nil {
		return a, e
	}
	digest, e := shapeverify.ProgramArtifactDigest(s.Kind(), a.Path)
	if e != nil {
		return a, e
	}
	if digest != a.Digest || a.Policy != policy {
		return a, errors.New("shape: artifact or trusted policy changed; verify again")
	}
	home, e := s.home()
	if e != nil {
		return a, e
	}
	_, e = evidence.Require(home, evidence.Binding{Candidate: c.Rev, Base: c.BaseRev, Verifier: shapeverify.BuildID(), Contract: policy, Policy: policy, Artifact: digest, Principal: HostPrincipal})
	return a, e
}
func (s Service) programGate(ctx context.Context, a ProgramArtifact) state.ActivationGate {
	return state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		if candidate != a.Revision || base != a.Base || principal != HostPrincipal {
			return state.EvidenceRef{}, errors.New("shape: program activation scope mismatch")
		}
		c := Candidate{Rev: candidate, BaseRev: base}
		if _, e := s.requireProgram(ctx, c); e != nil {
			return state.EvidenceRef{}, e
		}
		return state.EvidenceRef{Verdict: "pass", EvidenceID: a.Evidence}, nil
	})
}

func programCriterion(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(path, "_test.go") || base == "Makefile" || base == "dag.md" || shapeverify.IsDependencyManifest(base) || base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || base == "Containerfile" || strings.HasPrefix(base, "Containerfile.") || base == ".dockerignore" || base == ".containerignore" {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		switch part {
		case "scripts", "tools", "checks", "test", "tests", "testdata", "testutil", "testutils", "testhelper", "testhelpers", "testing", "vendor":
			return true
		}
	}
	return false
}

func (s Service) GenerationBrief(w Wish) string {
	if s.Kind() == "web" {
		return Brief(s.App.Name, w)
	}
	text := Brief(s.App.Name, w)
	text = strings.Replace(text, "You are changing the web app", "You are changing the "+s.Kind()+" app", 1)
	start := strings.Index(text, "Layout:")
	end := strings.Index(text, "Everything in this directory")
	if start >= 0 && end > start {
		text = text[:start] + "This is an installed native application's source, not a static web app. Make the smallest source change; the host's trusted registry owns its build/test and activation policy. Do not edit existing tests, build wrappers, dependency manifests, core or protected source. New native checker/test files are outside the MVP; submit those through ordinary upstream review.\n" + text[end:]
	}
	return text
}
