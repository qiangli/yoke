// Package shape provides the common candidate lifecycle used by the console and CLI.
package shape

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/shape/appserver"
	state "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
)

const (
	Running   = "running"
	Ready     = "ready"
	Verified  = "verified"
	Approved  = "approved"
	Rejected  = "rejected"
	Failed    = "failed"
	Requested = "requested"

	KindRequest = "request"
)

var ErrConflict = errors.New("shape: conflicting candidate state")
var ErrVerification = errors.New("shape: verification failed")
var ErrUnauthorized = errors.New("shape: caller is not authorized")

type Caller struct {
	Principal       string
	Actor           string
	RepresentedUser string
	Tier            string
	Circle          string
	Groups          []string
	Email           string
}
type App struct {
	Shaping         *fleet.AppShaping
	Scopes          *appserver.Scopes
	Owner           string
	ReleasePath     string
	ReleaseRevision string
	Name, SourceDir string
	Protected       []string
	Seed            func() error
}
type Options struct {
	Home, WorkRoot   string
	InstalledVersion string // Caller must set this for Incoming release comparisons.
	Delegate         Delegator
	Verifier         Verifier
	Activator        Activator
	Track            func(context.Context, *Candidate) error
	ProgramRunner    shapeverify.ProgramRunner
	ServiceRuntime   ServiceRuntime
}

func ProductionOptions(home string) Options {
	return Options{Home: home, Verifier: ProductionVerifier{Home: home}, Activator: &ProductionActivator{Home: home}, Track: NewTracking(home)}
}

type Service struct {
	App     App
	Options Options
}

// Source identifies the trigger for a wish. An empty Kind means human.
type Source struct {
	Kind string `json:"kind,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

func (s Source) Validate() error {
	switch s.Kind {
	case "", "human", "incident", "feed", "schedule":
		return nil
	default:
		return fmt.Errorf("shape: unknown source kind %q", s.Kind)
	}
}

type Wish struct {
	patch        PatchMetadata
	Source       Source `json:"source,omitempty,omitzero"`
	Request      string `json:"request"`
	Page         string `json:"page,omitempty"`
	Element      string `json:"element,omitempty"`
	Story        string `json:"story,omitempty"`
	Todo         string `json:"todo,omitempty"`
	TrackingRoot string `json:"-"`
	// Reproduction, Expected and Evidence describe a defect report; the
	// reserved bashy target records them (see BashyTarget).
	Reproduction string `json:"reproduction,omitempty"`
	Expected     string `json:"expected,omitempty"`
	Evidence     string `json:"evidence,omitempty"`
}

func (w Wish) validate() error {
	if strings.TrimSpace(w.Request) == "" {
		return errors.New("shape: request is required")
	}
	if w.Story != "" && w.Todo != "" {
		return errors.New("shape: choose story or todo")
	}
	return w.Source.Validate()
}

type CoverageSummary struct {
	Covered    []string `json:"covered"`
	NotCovered []string `json:"not_covered"`
}
type Candidate struct {
	Why                 string           `json:"why,omitempty"`
	Expect              string           `json:"expect,omitempty"`
	Source              Source           `json:"source,omitempty,omitzero"`
	ProgramPreview      *ProgramPreview  `json:"program_preview,omitempty"`
	ActivationPrincipal string           `json:"activation_principal,omitempty"`
	ArtifactPath        string           `json:"artifact_path,omitempty"`
	ProgramPolicy       string           `json:"program_policy,omitempty"`
	ID                  string           `json:"id"`
	App                 string           `json:"app"`
	Principal           string           `json:"principal"`
	Request             string           `json:"request"`
	Reproduction        string           `json:"reproduction,omitempty"`
	Expected            string           `json:"expected,omitempty"`
	Evidence            string           `json:"evidence,omitempty"`
	Page                string           `json:"page,omitempty"`
	Element             string           `json:"element,omitempty"`
	Kind                string           `json:"kind,omitempty"`
	Status              string           `json:"status"`
	Error               string           `json:"error,omitempty"`
	BaseRev             string           `json:"base_rev,omitempty"`
	Rev                 string           `json:"rev,omitempty"`
	Agent               string           `json:"agent,omitempty"`
	Identity            string           `json:"agent_identity,omitempty"`
	Receipt             string           `json:"agent_receipt,omitempty"`
	Coverage            *CoverageSummary `json:"coverage,omitempty"`
	Next                []string         `json:"next,omitempty"`
	Verdict             *Verdict         `json:"verdict,omitempty"`
	SpecReview          *SpecReview      `json:"spec_review,omitempty"`
	NewApp              json.RawMessage  `json:"new_app,omitempty"`
	Deciding            bool             `json:"deciding,omitempty"`
	Created             time.Time        `json:"created"`
	Actor               string           `json:"actor,omitempty"`
	RepresentedUser     string           `json:"represented_user,omitempty"`
	Scope               string           `json:"scope,omitempty"`
	AppKind             string           `json:"app_kind,omitempty"`
	Story               string           `json:"story,omitempty"`
	Todo                string           `json:"todo,omitempty"`
	TrackingItem        string           `json:"tracking_item,omitempty"`
	TrackingRoot        string           `json:"tracking_root,omitempty"`
	Workspace           string           `json:"workspace,omitempty"`
	Artifact            string           `json:"artifact,omitempty"`
}
type Status struct {
	Host       *HostStatus `json:"host,omitempty"`
	App        string      `json:"app"`
	Principal  string      `json:"principal"`
	Active     string      `json:"active"`
	Candidates []Candidate `json:"candidates"`
}

func (s Service) home() (string, error) {
	if s.Options.Home != "" {
		return filepath.Abs(s.Options.Home)
	}
	return state.Home()
}
func (s Service) root() (string, error) {
	if s.Options.WorkRoot != "" {
		return filepath.Abs(s.Options.WorkRoot)
	}
	h, e := s.home()
	return filepath.Join(h, "shape-work"), e
}
func PrincipalKey(p string) string { h := sha256.Sum256([]byte(p)); return hex.EncodeToString(h[:12]) }
func ValidID(id string) bool {
	if len(id) != 16 {
		return false
	}
	_, e := hex.DecodeString(id)
	return e == nil
}
func (s Service) validate(c Caller) error {
	if strings.TrimSpace(c.Principal) == "" {
		return errors.New("shape: a principal is required")
	}
	if s.App.Name == "" || filepath.Base(s.App.Name) != s.App.Name || strings.ContainsAny(s.App.Name, "\\/") || s.App.Name == "." || s.App.Name == ".." {
		return errors.New("shape: invalid app")
	}
	return nil
}
func (s Service) Owner() string { return strings.TrimSpace(s.App.Owner) }

func (s Service) QueuePrincipal(p string) string {
	if s.Kind() == "web" && s.Owner() != "" {
		return s.Owner()
	}
	return p
}

func (s Service) candidateQueuePrincipal(c Candidate) string {
	if s.Kind() == "web" && strings.TrimSpace(c.ActivationPrincipal) != "" {
		return c.ActivationPrincipal
	}
	return s.QueuePrincipal(c.Principal)
}

func (s Service) candidateActivationPrincipal(c Candidate) string {
	if s.Kind() != "web" {
		return HostPrincipal
	}
	if strings.TrimSpace(c.ActivationPrincipal) != "" {
		return c.ActivationPrincipal
	}
	return s.ActivationPrincipal(c.Principal)
}

func (s Service) isOwner(c Caller) bool {
	owner := s.Owner()
	return owner != "" && strings.EqualFold(strings.TrimSpace(c.Principal), owner)
}

func isStrangerCaller(c Caller) bool {
	p := strings.TrimSpace(c.Principal)
	t := strings.TrimSpace(c.Tier)
	return p == "" || p == "anonymous" || t == "anonymous" || t == "account" || t == "signed-in" || strings.HasPrefix(p, "visitor:")
}

func callerEmail(c Caller) string {
	if e := strings.TrimSpace(c.Email); e != "" {
		return e
	}
	return strings.TrimPrefix(strings.TrimSpace(c.Principal), "cloud:")
}

func (s Service) Scopes() (*appserver.Scopes, error) {
	if s.App.Scopes != nil {
		return s.App.Scopes, nil
	}
	if s.App.SourceDir == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(s.App.SourceDir, appserver.File))
	if err == nil {
		m, perr := appserver.Parse(b)
		if perr != nil {
			return nil, perr
		}
		return m.Scopes, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	r, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		return nil, nil
	}
	head, err := r.Head()
	if err != nil {
		return nil, nil
	}
	commit, err := r.CommitObject(head.Hash())
	if err != nil {
		return nil, nil
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, nil
	}
	f, err := tree.File(appserver.File)
	if err != nil {
		return nil, nil
	}
	body, err := f.Contents()
	if err != nil {
		return nil, err
	}
	m, err := appserver.Parse([]byte(body))
	if err != nil {
		return nil, err
	}
	return m.Scopes, nil
}

func (s Service) AdmitsScope(c Caller, scope string) (bool, error) {
	if !appserver.ValidScope(scope) {
		return false, fmt.Errorf("shape: unknown scope %q", scope)
	}
	if s.Owner() == "" {
		return true, nil
	}
	if isStrangerCaller(c) {
		return false, nil
	}
	if s.isOwner(c) {
		return true, nil
	}
	scopes, err := s.Scopes()
	if err != nil {
		return false, err
	}
	return scopes.AdmitsMember(scope, c.Circle, c.Groups, callerEmail(c)), nil
}

func (s Service) canAccessCandidate(callerPrincipal string, c Candidate) bool {
	if c.App != s.App.Name {
		return false
	}
	if c.Principal == callerPrincipal {
		return true
	}
	if s.Kind() == "web" {
		if owner := s.Owner(); owner != "" && strings.EqualFold(strings.TrimSpace(callerPrincipal), owner) {
			return true
		}
		if c.ActivationPrincipal != "" && strings.EqualFold(strings.TrimSpace(callerPrincipal), c.ActivationPrincipal) {
			return true
		}
	}
	return false
}

func (s Service) CandidateDir(p, id string) (string, error) {
	if !ValidID(id) {
		return "", fs.ErrNotExist
	}
	r, e := s.root()
	return filepath.Join(r, s.App.Name, PrincipalKey(s.QueuePrincipal(p)), id), e
}
func (s Service) lock(c Caller) (*lockfile.Lock, error) {
	if e := s.validate(c); e != nil {
		return nil, e
	}
	r, e := s.root()
	if e != nil {
		return nil, e
	}
	return lockfile.AcquireWithin(filepath.Join(r, ".locks", s.App.Name+"-"+PrincipalKey(s.QueuePrincipal(c.Principal))+".lock"), 30*time.Second, lockfile.Holder{Name: c.Actor, Intent: "shape candidate transition"})
}
func SaveCandidate(dir string, c Candidate) error {
	b, e := json.MarshalIndent(c, "", "  ")
	if e != nil {
		return e
	}
	tmp, e := os.CreateTemp(dir, ".candidate-*.tmp")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(append(b, '\n'))
	}
	ce := tmp.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(name, filepath.Join(dir, "candidate.json"))
}
func (s Service) load(p, id string) (Candidate, string, error) {
	d, e := s.CandidateDir(p, id)
	if e != nil {
		return Candidate{}, "", e
	}
	b, e := os.ReadFile(filepath.Join(d, "candidate.json"))
	if os.IsNotExist(e) {
		if ad, ae := s.approvedDir(p, id); ae == nil {
			b, e = os.ReadFile(filepath.Join(ad, "candidate.json"))
		}
	}
	if e != nil {
		return Candidate{}, d, e
	}
	var c Candidate
	if e = json.Unmarshal(b, &c); e != nil {
		return c, d, e
	}
	if !s.canAccessCandidate(p, c) {
		return Candidate{}, d, fs.ErrNotExist
	}
	// Older web candidates predate actor/represented-user metadata.
	if c.Actor == "" {
		c.Actor = c.Principal
	}
	if c.RepresentedUser == "" {
		c.RepresentedUser = c.Principal
	}
	return c, d, nil
}
func (s Service) Show(ctx context.Context, caller Caller, id string) (Candidate, error) {
	if e := s.validate(caller); e != nil {
		return Candidate{}, e
	}
	c, _, e := s.load(caller.Principal, id)
	return c, e
}
func (s Service) active(p string) (string, error) {
	if s.App.Seed != nil {
		if e := s.App.Seed(); e != nil {
			return "", e
		}
	}
	r, e := gogit.PlainOpen(s.App.SourceDir)
	if e != nil {
		return "", e
	}
	head, e := r.Head()
	if e != nil {
		return "", e
	}
	home, e := s.home()
	if e != nil {
		return "", e
	}
	st, e := state.OpenAt(home, s.App.Name, s.ActivationPrincipal(p))
	if e != nil {
		return "", e
	}
	v, e := st.Read()
	if e != nil {
		return "", e
	}
	for _, rev := range []string{v.ActiveRevision, v.LastKnownGood, head.Hash().String()} {
		if rev == "" {
			continue
		}
		if _, e = r.CommitObject(plumbing.NewHash(rev)); e == nil {
			if e = TouchesProtected(s.App.SourceDir, head.Hash().String(), rev, s.App.Protected); e == nil {
				return rev, nil
			}
		}
	}
	return "", errors.New("shape: no available active revision")
}
func (s Service) Status(ctx context.Context, c Caller) (Status, error) {
	if e := s.validate(c); e != nil {
		return Status{}, e
	}
	if s.isBashy() {
		return s.bashyStatus(ctx, c)
	}
	active, e := s.active(s.QueuePrincipal(c.Principal))
	if e != nil {
		return Status{}, e
	}
	out := Status{App: s.App.Name, Principal: c.Principal, Active: active, Candidates: []Candidate{}}
	if s.Kind() != "web" {
		host, e := s.HostStatus(ctx, c)
		if e != nil {
			return out, e
		}
		out.Host = &host
	}
	r, e := s.root()
	if e != nil {
		return out, e
	}
	entries, e := os.ReadDir(filepath.Join(r, s.App.Name, PrincipalKey(s.QueuePrincipal(c.Principal))))
	if e != nil && !os.IsNotExist(e) {
		return out, e
	}
	for _, v := range entries {
		if v, e := s.Show(ctx, c, v.Name()); e == nil {
			out.Candidates = append(out.Candidates, v)
		}
	}
	return out, nil
}
func (s Service) Begin(ctx context.Context, caller Caller, w Wish) (Candidate, error) {
	return s.begin(ctx, caller, w, false, "")
}
func (s Service) begin(ctx context.Context, caller Caller, w Wish, recovery bool, expectedBase string) (Candidate, error) {
	if s.isBashy() {
		return s.bashyRecord(ctx, caller, w)
	}
	if e := s.admitAuto(w.Source); e != nil {
		return Candidate{}, e
	}
	if s.Kind() == "web" && s.Owner() != "" {
		if isStrangerCaller(caller) {
			return Candidate{}, fmt.Errorf("%w: caller %s is not authorized to shape %s", ErrUnauthorized, caller.Principal, s.App.Name)
		}
		ok, err := s.AdmitsScope(caller, appserver.ScopeShape)
		if err != nil {
			return Candidate{}, err
		}
		if !ok {
			return Candidate{}, fmt.Errorf("%w: caller %s lacks shape scope on %s", ErrUnauthorized, caller.Principal, s.App.Name)
		}
	}
	if s.Kind() != "web" {
		if e := s.RecoverProgram(ctx); e != nil {
			return Candidate{}, e
		}
		if !recovery {
			st, e := s.HostStatus(ctx, caller)
			if e != nil {
				return Candidate{}, e
			}
			if st.Pending {
				return Candidate{}, errors.New("shape: reconcile host source/artifact with restore before creating a candidate")
			}
		}
	}
	l, e := s.lock(caller)
	if e != nil {
		return Candidate{}, e
	}
	defer l.Release()
	if err := w.validate(); err != nil {
		return Candidate{}, err
	}
	actPrincipal := s.ActivationPrincipalFor(caller)
	if expectedBase != "" && actPrincipal != s.Owner() {
		return Candidate{}, ErrUnauthorized
	}
	base, e := s.active(actPrincipal)
	if e != nil {
		return Candidate{}, e
	}
	if expectedBase != "" && expectedBase != base {
		return Candidate{}, fmt.Errorf("%w: active revision changed (base %s, active %s)", ErrConflict, expectedBase, base)
	}
	if e = shapeverify.RequireCompleteSource(s.App.SourceDir); e != nil {
		return Candidate{}, e
	}
	var id [8]byte
	if _, e = rand.Read(id[:]); e != nil {
		return Candidate{}, e
	}
	c := Candidate{ID: hex.EncodeToString(id[:]), App: s.App.Name, Principal: caller.Principal, Actor: caller.Actor, RepresentedUser: caller.RepresentedUser, Source: w.Source, Request: strings.TrimSpace(w.Request), Page: w.Page, Element: w.Element, Story: w.Story, Todo: w.Todo, TrackingRoot: w.TrackingRoot, BaseRev: base, Status: Running, AppKind: s.Kind(), Scope: s.Scope(), ActivationPrincipal: actPrincipal, Created: time.Now().UTC()}
	c.Why, c.Expect, c.Agent = w.patch.Why, w.patch.Expect, w.patch.Agent
	if c.Actor == "" {
		c.Actor = c.Principal
	}
	if c.RepresentedUser == "" {
		c.RepresentedUser = c.Principal
	}
	dir, e := s.CandidateDir(s.candidateQueuePrincipal(c), c.ID)
	if e != nil {
		return c, e
	}
	c.Workspace = filepath.Join(dir, "work")
	if s.Options.Track != nil {
		if e = s.Options.Track(ctx, &c); e != nil {
			return c, e
		}
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		return c, e
	}
	if e = SaveCandidate(dir, c); e != nil {
		return c, e
	}
	s.LogDecision(c, "trigger", "wish", joinDetail(c.ID, c.Request))
	return c, nil
}

// FileRequest records an out-of-scope or protected wish as a request on the
// owner's queue (or the caller's own queue when the app has no separate owner).
// No workspace is created and no shaping agent runs.
func (s Service) FileRequest(ctx context.Context, caller Caller, w Wish) (Candidate, error) {
	if s.isBashy() {
		return s.bashyRecord(ctx, caller, w)
	}
	if s.Owner() != "" && isStrangerCaller(caller) {
		return Candidate{}, fmt.Errorf("%w: caller %s is not authorized to file a request on %s", ErrUnauthorized, caller.Principal, s.App.Name)
	}
	if e := s.admitAuto(w.Source); e != nil {
		return Candidate{}, e
	}
	l, e := s.lock(caller)
	if e != nil {
		return Candidate{}, e
	}
	defer l.Release()
	if err := w.validate(); err != nil {
		return Candidate{}, err
	}
	var id [8]byte
	if _, e = rand.Read(id[:]); e != nil {
		return Candidate{}, e
	}
	c := Candidate{
		ID:                  hex.EncodeToString(id[:]),
		App:                 s.App.Name,
		Principal:           caller.Principal,
		Actor:               caller.Actor,
		RepresentedUser:     caller.RepresentedUser,
		ActivationPrincipal: s.QueuePrincipal(caller.Principal),
		Source:              w.Source,
		Request:             strings.TrimSpace(w.Request),
		Page:                w.Page,
		Element:             w.Element,
		Story:               w.Story,
		Todo:                w.Todo,
		TrackingRoot:        w.TrackingRoot,
		Kind:                KindRequest,
		Status:              Requested,
		AppKind:             s.Kind(),
		Scope:               s.Scope(),
		Created:             time.Now().UTC(),
	}
	if c.Actor == "" {
		c.Actor = c.Principal
	}
	if c.RepresentedUser == "" {
		c.RepresentedUser = c.Principal
	}
	if s.Options.Track != nil {
		if e = s.Options.Track(ctx, &c); e != nil {
			return c, e
		}
	}
	dir, e := s.CandidateDir(s.candidateQueuePrincipal(c), c.ID)
	if e != nil {
		return c, e
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		return c, e
	}
	if e = SaveCandidate(dir, c); e != nil {
		return c, e
	}
	s.LogDecision(c, "trigger", "request", joinDetail(c.ID, c.Request))
	return c, nil
}
func (s Service) complete(ctx context.Context, caller Caller, c Candidate, runErr error) (Candidate, error) {
	l, e := s.lock(caller)
	if e != nil {
		return c, e
	}
	defer l.Release()
	now, d, e := s.load(c.Principal, c.ID)
	if e != nil {
		if os.IsNotExist(e) {
			if dir, de := s.CandidateDir(s.candidateQueuePrincipal(c), c.ID); de == nil {
				_ = os.RemoveAll(dir)
			}
		}
		return c, e
	}
	if now.Status != Running {
		return now, ErrConflict
	}
	if runErr != nil {
		c.Status = Failed
		c.Error = runErr.Error()
	} else {
		c.Status = Ready
		if s.Kind() == "web" {
			c.Artifact = c.Rev
		}
	}
	if s.Options.Track != nil {
		if e = s.Options.Track(ctx, &c); e != nil {
			c.Status = Failed
			c.Error = e.Error()
			runErr = e
		}
	}
	if e = SaveCandidate(d, c); e != nil {
		return c, e
	}
	if runErr != nil {
		s.LogDecision(c, "verdict", "failed", joinDetail(c.ID, runErr.Error()))
	}
	return c, runErr
}
func (s Service) Wish(ctx context.Context, caller Caller, w Wish) (Candidate, error) {
	if s.isBashy() {
		return s.bashyRecord(ctx, caller, w)
	}
	if s.Kind() == "web" && s.Owner() != "" {
		if isStrangerCaller(caller) {
			return Candidate{}, fmt.Errorf("%w: caller %s is not authorized to shape %s", ErrUnauthorized, caller.Principal, s.App.Name)
		}
		ok, err := s.AdmitsScope(caller, appserver.ScopeShape)
		if err != nil {
			return Candidate{}, err
		}
		if !ok {
			return s.FileRequest(ctx, caller, w)
		}
	}
	if s.Options.Delegate == nil {
		return Candidate{}, errors.New("shape: no shaping agent configured")
	}
	c, e := s.Begin(ctx, caller, w)
	if e != nil {
		return c, e
	}
	r, e := CloneAt(ctx, s.App.SourceDir, c.Workspace, c.BaseRev)
	if e != nil {
		return s.complete(ctx, caller, c, e)
	}
	home, _ := s.home()
	ledger := PrepareWorkLedger(c.Workspace, home, s.App.Name, c.BaseRev, s.isOwner(caller), caller.Principal, c.ID)
	res, e := s.Options.Delegate.Delegate(ctx, DelegateRequest{Dir: c.Workspace, Task: s.generationBriefWithLedger(w, ledger)})
	c.Agent, c.Identity, c.Receipt = res.Agent, res.Identity, res.Receipt
	c.Coverage = ParseCoverage(res.Output)
	c.Why, c.Expect = ParseReasons(res.Output)
	c.Next = ParseNext(res.Output)
	relativizeCoverage(&c)
	if e == nil {
		c.Rev, e = CommitCandidate(r, c)
	}
	return s.complete(ctx, caller, c, e)
}
func (s Service) Import(ctx context.Context, caller Caller, w Wish, workspace string) (Candidate, error) {
	return s.importChanges(ctx, caller, w, "", func(dest string) error { return CopyWorkspace(workspace, dest) })
}

func (s Service) importChanges(ctx context.Context, caller Caller, w Wish, base string, apply func(string) error) (Candidate, error) {
	if s.isBashy() {
		return Candidate{}, ErrBashyActivation
	}
	c, e := s.begin(ctx, caller, w, false, base)
	if e != nil {
		return c, e
	}
	r, e := CloneAt(ctx, s.App.SourceDir, c.Workspace, c.BaseRev)
	if e == nil {
		e = apply(c.Workspace)
	}
	if e == nil {
		c.Rev, e = CommitCandidate(r, c)
	}
	return s.complete(ctx, caller, c, e)
}
func (s Service) Diff(ctx context.Context, caller Caller, id string) (string, error) {
	c, e := s.Show(ctx, caller, id)
	if e != nil {
		return "", e
	}
	if c.Rev == "" {
		return "", ErrConflict
	}
	d, e := s.CandidateDir(s.candidateQueuePrincipal(c), c.ID)
	if e != nil {
		return "", e
	}
	work := filepath.Join(d, "work")
	if c.Status == Approved {
		work = s.App.SourceDir
	}
	return Diff(work, c.BaseRev, c.Rev)
}
func (s Service) verify(ctx context.Context, c *Candidate, dir string, recordVerdict func(Candidate)) error {
	if c.Rev == "" || (c.Status != Ready && c.Status != Verified) {
		return ErrConflict
	}
	if e := TouchesProtected(filepath.Join(dir, "work"), c.BaseRev, c.Rev, s.App.Protected); e != nil {
		c.Verdict = &Verdict{Pass: false, Detail: e.Error()}
		c.Status = Ready
		recordVerdict(*c)
		if s.Options.Track != nil {
			if te := s.Options.Track(ctx, c); te != nil {
				return te
			}
		}
		return SaveCandidate(dir, *c)
	}
	if s.Verifier() == nil {
		return errors.New("shape: no verifier configured")
	}
	v, e := s.Verifier().Verify(ctx, VerifyRequest{App: c.App, Principal: s.candidateActivationPrincipal(*c), SourceDir: s.App.SourceDir, CandidateDir: filepath.Join(dir, "work"), BaseRev: c.BaseRev, CandidateRev: c.Rev, Agent: c.Agent, Spec: c.reviewedSpec()})
	if e != nil {
		return e
	}
	c.Verdict = &v
	recordVerdict(*c)
	if v.Artifact != "" {
		c.Artifact = v.Artifact
		c.ArtifactPath = v.ArtifactPath
		c.ProgramPolicy = v.Policy
	}
	if v.Pass {
		c.Status = Verified
	} else {
		c.Status = Ready
	}
	if s.Options.Track != nil {
		if e = s.Options.Track(ctx, c); e != nil {
			return e
		}
	}
	return SaveCandidate(dir, *c)
}
func (s Service) Verify(ctx context.Context, caller Caller, id string) (Candidate, error) {
	if s.isBashy() {
		return Candidate{}, ErrBashyActivation
	}
	l, e := s.lock(caller)
	if e != nil {
		return Candidate{}, e
	}
	defer l.Release()
	c, d, e := s.load(caller.Principal, id)
	if e != nil {
		return c, e
	}
	if e = s.verify(ctx, &c, d, s.logAutoVerdict); e != nil {
		return c, e
	}
	if c.Verdict == nil || !c.Verdict.Pass {
		return c, ErrVerification
	}
	return c, nil
}
func (s Service) Approve(ctx context.Context, caller Caller, id string) (Candidate, error) {
	c, e := s.approve(ctx, caller, id)
	// Log only decisions that name a candidate.
	switch {
	case c.Status == Approved:
		s.LogDecision(c, "activation", "activated", joinDetail(c.ID, c.Rev))
	case e != nil && c.ID != "":
		s.LogDecision(c, "activation", "failed", joinDetail(c.ID, e.Error()))
	}
	return c, e
}
func (s Service) approve(ctx context.Context, caller Caller, id string) (Candidate, error) {
	if s.isBashy() {
		return Candidate{}, ErrBashyActivation
	}
	l, e := s.lock(caller)
	if e != nil {
		return Candidate{}, e
	}
	defer l.Release()
	c, d, e := s.load(caller.Principal, id)
	if e != nil {
		return c, e
	}
	actPrincipal := s.candidateActivationPrincipal(c)
	if s.Kind() == "web" && !strings.EqualFold(strings.TrimSpace(caller.Principal), strings.TrimSpace(actPrincipal)) {
		return c, fmt.Errorf("%w: only %s may approve candidates for %s", ErrUnauthorized, actPrincipal, s.App.Name)
	}
	root, e := s.root()
	if e != nil {
		return c, e
	}
	appLock, e := lockfile.AcquireWithin(filepath.Join(root, ".locks", s.App.Name+".activate.lock"), 30*time.Second, lockfile.Holder{Name: caller.Actor, Intent: "shape activation"})
	if e != nil {
		return c, e
	}
	defer appLock.Release()
	if s.Kind() != "web" {
		if e = s.RecoverProgram(ctx); e != nil {
			return c, e
		}
	}
	active, e := s.active(actPrincipal)
	if e != nil {
		return c, e
	}
	if c.Status == Approved {
		return s.finishApproval(ctx, c, d)
	}
	if c.Status == "activating" {
		if active == c.Rev {
			c.Status = Approved
			return s.finishApproval(ctx, c, d)
		}
		if active != c.BaseRev {
			return c, fmt.Errorf("%w: unresolved interrupted activation (base %s, active %s)", ErrConflict, c.BaseRev, active)
		}
		c.Status = Ready
	}
	if active != c.BaseRev {
		return c, fmt.Errorf("%w: active revision changed (base %s, active %s); create a fresh candidate", ErrConflict, c.BaseRev, active)
	}
	if e = s.verify(ctx, &c, d, s.logAutoVerdict); e != nil {
		return c, e
	}
	if c.Verdict == nil || !c.Verdict.Pass {
		return c, ErrVerification
	}
	if s.Kind() == "web" && s.Options.Activator == nil {
		return c, errors.New("shape: no activator configured")
	}
	message := OneLine(c.Request)
	if c.Coverage != nil && len(c.Coverage.Covered) > 0 {
		message += " — COVERED: " + strings.Join(c.Coverage.Covered, "; ")
	}
	if s.Options.Track != nil {
		if e = s.Options.Track(ctx, &c); e != nil {
			return c, e
		}
	}
	c.Status = "activating"
	if e = SaveCandidate(d, c); e != nil {
		return c, e
	}
	if s.Kind() == "web" {
		author := c.RepresentedUser
		if author == "" {
			author = c.Principal
		}
		e = s.Options.Activator.Activate(ctx, ActivateRequest{App: c.App, Principal: actPrincipal, Author: author, SourceDir: s.App.SourceDir, CandidateDir: filepath.Join(d, "work"), BaseRev: c.BaseRev, CandidateRev: c.Rev, Request: message, Agent: c.Agent, Evidence: c.Verdict.Evidence})
	} else {
		e = s.activateProgram(ctx, c)
	}
	if e != nil {
		current, readErr := s.active(actPrincipal)
		if readErr == nil && current == c.Rev {
			if s.Kind() != "web" {
				if recoverErr := s.RecoverProgram(ctx); recoverErr != nil {
					return c, errors.Join(e, recoverErr)
				}
			}
			c.Status = Approved
			return s.finishApproval(ctx, c, d)
		}
		if readErr == nil && current == c.BaseRev {
			if s.Kind() != "web" {
				if _, pending := s.readTransition(); pending == nil {
					return c, fmt.Errorf("shape: rollback pending; retry recovery: %w", e)
				} else if !os.IsNotExist(pending) {
					return c, errors.Join(e, pending)
				}
			}
			c.Status = Verified
			_ = SaveCandidate(d, c)
			return c, e
		}
		// An uncertain switch stays recoverable and cannot be rejected.
		return c, fmt.Errorf("shape: activation outcome unresolved; inspect status and retry approval: %w", e)
	}
	c.Status = Approved
	return s.finishApproval(ctx, c, d)
}
func (s Service) finishApproval(ctx context.Context, c Candidate, d string) (Candidate, error) {
	var e error
	if c.ProgramPreview != nil {
		if e = s.cleanupProgramPreview(ctx, c); e != nil {
			return c, fmt.Errorf("shape: activated; preview cleanup pending: %w", e)
		}
		c.ProgramPreview = nil
	}
	// Preserve truthful active state even if the durable archive write fails.
	if _, se := os.Stat(d); se == nil {
		_ = SaveCandidate(d, c)
	}
	// Keep the identity record outside disposable candidate directories.
	qp := s.candidateQueuePrincipal(c)
	ad, e := s.approvedDir(qp, c.ID)
	if e != nil {
		return c, fmt.Errorf("shape: activated, but archive unavailable: %w", e)
	}
	if e = os.MkdirAll(ad, 0700); e != nil {
		return c, fmt.Errorf("shape: activated, but archive unavailable: %w", e)
	}
	if e = SaveCandidate(ad, c); e != nil {
		return c, fmt.Errorf("shape: activated, but archive unavailable: %w", e)
	}
	if s.Options.Track != nil {
		if e = s.Options.Track(ctx, &c); e != nil {
			return c, fmt.Errorf("shape: activated, but tracking update failed: %w", e)
		}
	}
	if e = s.cleanPreview(qp, c.ID); e == nil {
		e = os.RemoveAll(d)
	}
	if e != nil {
		return c, fmt.Errorf("shape: activated; disposable cleanup failed: %w", e)
	}
	return c, nil
}

func (s Service) Reject(ctx context.Context, caller Caller, id string) (Candidate, error) {
	return s.RejectWithReason(ctx, caller, id, "")
}

func (s Service) RejectWithReason(ctx context.Context, caller Caller, id, reason string) (Candidate, error) {
	l, e := s.lock(caller)
	if e != nil {
		return Candidate{}, e
	}
	defer l.Release()
	c, d, e := s.load(caller.Principal, id)
	if e != nil {
		return c, e
	}
	if c.Status == Approved || c.Status == "activating" {
		return c, ErrConflict
	}
	if e = s.cleanPreview(s.candidateQueuePrincipal(c), c.ID); e != nil {
		return c, e
	}
	dlog := s.decision(c, "reject", "rejected", joinDetail(c.Request, reason))
	dlog.Principal, dlog.By = caller.Principal, "person"
	s.writeDecision(dlog)
	if e = os.RemoveAll(d); e != nil {
		return c, e
	}
	c.Status = Rejected
	return c, nil
}

func (s Service) Complete(ctx context.Context, caller Caller, c Candidate, err error) (Candidate, error) {
	return s.complete(ctx, caller, c, err)
}

func (s Service) approvedDir(p, id string) (string, error) {
	r, e := s.root()
	if e != nil {
		return "", e
	}
	return filepath.Join(r, ".approved", s.App.Name, PrincipalKey(s.QueuePrincipal(p)), id), nil
}
func (s Service) cleanPreview(p, id string) error {
	if s.isBashy() {
		return nil
	}
	if s.Kind() != "web" {
		if c, _, e := s.load(p, id); e == nil {
			if e = s.cleanupProgramPreview(context.Background(), c); e != nil {
				return e
			}
		}
	}
	home, e := s.home()
	if e != nil {
		return e
	}
	if st, e := state.OpenAt(home, s.App.Name, s.QueuePrincipal(s.ActivationPrincipal(p))); e == nil {
		if e = os.RemoveAll(filepath.Join(st.Dir(), "preview", id)); e != nil {
			return e
		}
	}
	return nil
}

// VerifyRecord reuses the same verifier for console draft saves, whose record
// is owned by the draft store rather than the ordinary candidate listing.
func (s Service) VerifyRecord(ctx context.Context, caller Caller, c *Candidate, dir string) error {
	return s.verifyRecord(ctx, caller, c, dir, s.logAutoVerdict)
}

// VerifyRepairRecord leaves verdict logging to the repair loop, which records
// the attempt outcome and then its verdict together.
func (s Service) VerifyRepairRecord(ctx context.Context, caller Caller, c *Candidate, dir string) error {
	return s.verifyRecord(ctx, caller, c, dir, func(Candidate) {})
}
func (s Service) verifyRecord(ctx context.Context, caller Caller, c *Candidate, dir string, recordVerdict func(Candidate)) error {

	if s.isBashy() {
		return ErrBashyActivation
	}
	l, e := s.lock(caller)
	if e != nil {
		return e
	}
	defer l.Release()
	// Legacy web records predate tracking identity. Fill only missing fields
	// from its authenticated owner after checking the record belongs to that scope.
	if c == nil || c.App != s.App.Name || c.Principal != caller.Principal {
		return fs.ErrNotExist
	}
	if strings.TrimSpace(caller.Actor) == "" {
		return errors.New("shape: candidate verification needs an authenticated actor")
	}
	if c.Actor == "" {
		c.Actor = c.Principal
	}
	if c.RepresentedUser == "" {
		c.RepresentedUser = caller.Principal
	}
	return s.verify(ctx, c, dir, recordVerdict)
}

// Update serializes web-specific terminal metadata changes with CLI decisions.
// The callback runs only while the candidate still exists; it must not run a model.
func (s Service) Update(ctx context.Context, caller Caller, id string, fn func(*Candidate, string) error) (Candidate, error) {
	l, e := s.lock(caller)
	if e != nil {
		return Candidate{}, e
	}
	defer l.Release()
	c, d, e := s.load(caller.Principal, id)
	if e != nil {
		return c, e
	}
	if e = fn(&c, d); e != nil {
		return c, e
	}
	if s.Options.Track != nil {
		if e = s.Options.Track(ctx, &c); e != nil {
			return c, e
		}
	}
	return c, SaveCandidate(d, c)
}
