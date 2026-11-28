package shape

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
)

// ErrNoSpecChange refuses a spec review of a candidate that keeps the base
// spec: there is nothing for the owner to review.
var ErrNoSpecChange = errors.New("shape: the candidate keeps the base spec; there is no spec change to review")

// SpecReview is the owner's record that a candidate's spec change was
// reviewed. It names the reviewed spec by digest, so the candidate is graded
// against exactly that spec and nothing else: a later revision carrying a
// different spec is not covered. It lives in the service's candidate record,
// outside the candidate tree, so a candidate cannot review its own change.
type SpecReview struct {
	// Spec is the digest of the reviewed candidate spec.
	Spec string `json:"spec"`
	// Base is the digest of the spec it replaces.
	Base string `json:"base"`
	// Changed lists the base checks the change edits or removes.
	Changed []string  `json:"changed"`
	By      string    `json:"by"`
	At      time.Time `json:"at"`
}

// reviewedSpec is the spec digest the owner reviewed for c, or empty.
func (c Candidate) reviewedSpec() string {
	if c.SpecReview == nil {
		return ""
	}
	return c.SpecReview.Spec
}

// canReviewSpec is the owner tier: the app owner when one is configured,
// else the principal whose line the candidate would activate on (the local
// operator for host kinds). A caller stamped with any tier other than owner
// never qualifies.
func (s Service) canReviewSpec(caller Caller, c Candidate) bool {
	if t := strings.TrimSpace(caller.Tier); t != "" && t != "owner" {
		return false
	}
	if s.Owner() != "" {
		return s.isOwner(caller)
	}
	if s.Kind() != "web" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(caller.Principal), strings.TrimSpace(s.candidateActivationPrincipal(c)))
}

// ReviewSpec records the owner's review of candidate id's spec change. The
// trusted verifier then grades the candidate against its own spec (under the
// base's effect cap); without this record a spec-changing candidate is
// refused. Approval activates code and spec together, and an undo grades
// the earlier version against the spec it carries.
func (s Service) ReviewSpec(ctx context.Context, caller Caller, id string) (Candidate, error) {
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
	if !s.canReviewSpec(caller, c) {
		return c, fmt.Errorf("%w: only the owner may review a spec change for %s", ErrUnauthorized, s.App.Name)
	}
	if c.Rev == "" || (c.Status != Ready && c.Status != Verified) {
		return c, ErrConflict
	}
	var delta shapeverify.SpecDelta
	work := filepath.Join(d, "work")
	e = withVerifyHead(work, c.Rev, func() error {
		var re error
		delta, re = shapeverify.ReadSpecDelta(work, c.BaseRev, c.Rev, nil)
		return re
	})
	if e != nil {
		return c, e
	}
	if len(delta.Changed) == 0 {
		return c, ErrNoSpecChange
	}
	by := strings.TrimSpace(caller.Actor)
	if by == "" {
		by = caller.Principal
	}
	c.SpecReview = &SpecReview{Spec: delta.Candidate, Base: delta.Base, Changed: delta.Changed, By: by, At: time.Now().UTC()}
	// A verdict reached before the review was graded without it.
	if c.Status == Verified {
		c.Status = Ready
	}
	if s.Options.Track != nil {
		if e = s.Options.Track(ctx, &c); e != nil {
			return c, e
		}
	}
	if e = SaveCandidate(d, c); e != nil {
		return c, e
	}
	dec := s.decision(c, "spec-review", "reviewed", joinDetail(c.ID, "by "+by+": "+strings.Join(delta.Changed, ", ")))
	dec.Spec = delta.Candidate
	s.writeDecision(dec)
	return c, nil
}
