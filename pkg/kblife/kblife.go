// Package kblife is the lifecycle semantics for kb pages AND revisioned
// document records: validity intervals, supersede, forget, and the resolution
// of a record's state at an instant.
//
// Two rules run through it. Resolve at consumption, flag never edit: a record
// carries its interval and, when forgotten, a tombstone; whether it is visible
// is decided by the reader at its as-of instant, never by rewriting the record.
// Nothing is deleted: supersede closes an interval, forget adds a tombstone,
// and the audit trail keeps both.
//
// It imports only the standard library so every store — the kb page store and
// the dbdoc-backed records alike — can share this ONE source of the semantics
// without an import cycle.
package kblife

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Validity is a record's half-open interval [ValidFrom, ValidTo). A zero
// bound is open.
type Validity struct {
	ValidFrom, ValidTo time.Time
}

// At reports whether t falls inside the interval.
func (v Validity) At(t time.Time) bool {
	if !v.ValidFrom.IsZero() && t.Before(v.ValidFrom) {
		return false
	}
	if !v.ValidTo.IsZero() && !t.Before(v.ValidTo) {
		return false
	}
	return true
}

// Check rejects an inverted range: a set ValidTo at or before ValidFrom.
func (v Validity) Check() error {
	if !v.ValidTo.IsZero() && !v.ValidFrom.IsZero() && !v.ValidTo.After(v.ValidFrom) {
		return fmt.Errorf("kblife: inverted validity: valid_to %s is not after valid_from %s",
			v.ValidTo.UTC().Format(time.RFC3339), v.ValidFrom.UTC().Format(time.RFC3339))
	}
	return nil
}

// Supersede closes old at `at` and opens replacement at the same instant:
// old.ValidTo = at, replacement.ValidFrom = at. Every check runs before any
// mutation, so on error both inputs are unchanged — a caller committing both
// records in one batch never sees a half-applied supersede.
func Supersede(old, replacement *Validity, at time.Time) error {
	if old == nil || replacement == nil {
		return errors.New("kblife: supersede needs both the old and the replacement validity")
	}
	if at.IsZero() {
		return errors.New("kblife: supersede needs a non-zero instant")
	}
	if err := old.Check(); err != nil {
		return fmt.Errorf("kblife: old record: %w", err)
	}
	if err := replacement.Check(); err != nil {
		return fmt.Errorf("kblife: replacement record: %w", err)
	}
	if !old.ValidFrom.IsZero() && at.Before(old.ValidFrom) {
		return fmt.Errorf("kblife: supersede at %s is before the old record's valid_from %s",
			at.UTC().Format(time.RFC3339), old.ValidFrom.UTC().Format(time.RFC3339))
	}
	if !old.ValidTo.IsZero() && !old.ValidTo.After(at) {
		return fmt.Errorf("kblife: old record already closed at %s", old.ValidTo.UTC().Format(time.RFC3339))
	}
	if !replacement.ValidTo.IsZero() && !replacement.ValidTo.After(at) {
		return fmt.Errorf("kblife: replacement record's valid_to %s is not after %s",
			replacement.ValidTo.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339))
	}
	old.ValidTo = at
	replacement.ValidFrom = at
	return nil
}

// Tombstone records that a record was forgotten. Ref is the record's stable
// ref, preserved verbatim; By is the caller principal. The record itself is
// never deleted — the tombstone is what hides it.
type Tombstone struct {
	Ref     string    `json:"ref"`
	Reason  string    `json:"reason"`
	By      string    `json:"by"`
	Episode string    `json:"episode,omitempty"`
	At      time.Time `json:"at"`
}

// NewTombstone validates and builds a tombstone. ref, reason and by are
// required; ref is kept byte-for-byte.
func NewTombstone(ref, reason, by, episode string, at time.Time) (Tombstone, error) {
	switch {
	case strings.TrimSpace(ref) == "":
		return Tombstone{}, errors.New("kblife: forget needs a ref")
	case strings.TrimSpace(reason) == "":
		return Tombstone{}, errors.New("kblife: forget needs a reason")
	case strings.TrimSpace(by) == "":
		return Tombstone{}, errors.New("kblife: forget needs the caller principal (by)")
	case at.IsZero():
		return Tombstone{}, errors.New("kblife: forget needs a non-zero instant")
	}
	return Tombstone{Ref: ref, Reason: strings.TrimSpace(reason), By: strings.TrimSpace(by), Episode: episode, At: at}, nil
}

// State is a record's resolved lifecycle state at an instant.
type State string

const (
	StateValid       State = "valid"
	StateSuperseded  State = "superseded"
	StateExpired     State = "expired"
	StateForgotten   State = "forgotten"
	StateNotYetValid State = "not-yet-valid"
)

// Resolve decides a record's state at asOf. A tombstone at or before asOf
// wins; then a superseded record whose interval has closed (a superseded
// record with no recorded valid_to predates intervals and stays superseded);
// then expiry and not-yet-valid by the interval alone.
func Resolve(v Validity, supersededBy string, tomb *Tombstone, asOf time.Time) State {
	if tomb != nil && !tomb.At.After(asOf) {
		return StateForgotten
	}
	closed := !v.ValidTo.IsZero() && !asOf.Before(v.ValidTo)
	if supersededBy != "" && (v.ValidTo.IsZero() || closed) {
		return StateSuperseded
	}
	if closed {
		return StateExpired
	}
	if !v.ValidFrom.IsZero() && asOf.Before(v.ValidFrom) {
		return StateNotYetValid
	}
	return StateValid
}

// Visible reports whether a record in state s is shown by default recall.
func Visible(s State) bool { return s == StateValid }
