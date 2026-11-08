package shape

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BashyTarget is the reserved, built-in shape target for bashy itself. bashy
// is not a registered app, so a defect or unfitting behavior in a bashy
// command is recorded against this name instead. The target only RECORDS:
// it never builds, activates or replaces the running bashy binary — bashy
// changes land through its source repo (story/weave). Records stay in the
// local candidate store on this host (host scope, never submitted).
const BashyTarget = "bashy"

// BashyKind is the app kind recorded on bashy-target candidates.
const BashyKind = "bashy"

// Reported is the terminal-for-now status of a bashy-target record: captured,
// awaiting a source-repo change.
const Reported = "reported"

// ErrBashyActivation refuses every activation-side transition for bashy.
var ErrBashyActivation = errors.New("shape: bashy is not live-activated; bashy changes land through its source repo (story/weave), not shape approve/activate")

// IsBashyTarget reports whether name is the reserved bashy target; a
// registered app may not take this name.
func IsBashyTarget(name string) bool { return name == BashyTarget }

// BashyApp is the built-in App for the reserved target. It has no source
// directory: nothing is cloned, built or activated for it.
func BashyApp() App { return App{Name: BashyTarget} }

// A registered app may carry the bashy name with a real source (the
// source-tier promotion path); only the sourceless built-in is record-only.
func (s Service) isBashy() bool { return IsBashyTarget(s.App.Name) && s.App.SourceDir == "" }

// bashyHost describes the target's limits in status output.
func bashyHost() *HostStatus {
	return &HostStatus{Kind: BashyKind, Scope: "host", ActivationPrincipal: HostPrincipal, Detail: "local-only report target; no activation — changes land through the bashy source repo (story/weave)"}
}

func (s Service) bashyStatus(ctx context.Context, c Caller) (Status, error) {
	out := Status{App: s.App.Name, Principal: c.Principal, Candidates: []Candidate{}, Host: bashyHost()}
	r, e := s.root()
	if e != nil {
		return out, e
	}
	entries, e := os.ReadDir(filepath.Join(r, s.App.Name, PrincipalKey(c.Principal)))
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

// bashyRecord stores a request with its reproduction, expected behavior and
// evidence as an ordinary candidate record, without a workspace or revision.
func (s Service) bashyRecord(ctx context.Context, caller Caller, w Wish) (Candidate, error) {
	l, e := s.lock(caller)
	if e != nil {
		return Candidate{}, e
	}
	defer l.Release()
	if strings.TrimSpace(w.Request) == "" {
		return Candidate{}, errors.New("shape: request is required")
	}
	if w.Story != "" && w.Todo != "" {
		return Candidate{}, errors.New("shape: choose story or todo")
	}
	var id [8]byte
	if _, e = rand.Read(id[:]); e != nil {
		return Candidate{}, e
	}
	c := Candidate{ID: hex.EncodeToString(id[:]), App: s.App.Name, Principal: caller.Principal, Actor: caller.Actor, RepresentedUser: caller.RepresentedUser, Request: strings.TrimSpace(w.Request), Reproduction: strings.TrimSpace(w.Reproduction), Expected: strings.TrimSpace(w.Expected), Evidence: strings.TrimSpace(w.Evidence), Page: w.Page, Element: w.Element, Story: w.Story, Todo: w.Todo, TrackingRoot: w.TrackingRoot, Status: Reported, AppKind: BashyKind, Scope: "host", Created: time.Now().UTC()}
	if c.RepresentedUser == "" {
		c.RepresentedUser = c.Principal
	}
	dir, e := s.CandidateDir(c.Principal, c.ID)
	if e != nil {
		return c, e
	}
	if s.Options.Track != nil {
		if e = s.Options.Track(ctx, &c); e != nil {
			return c, e
		}
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		return c, e
	}
	return c, SaveCandidate(dir, c)
}
