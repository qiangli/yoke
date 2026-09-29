// Package blame classes ladder failures before any rating moves.
//
// Fleet evidence invariant: no rating movement without evidence; an
// unclassified failure is unrated, never scored 0. Only a valid
// agent-class attribution rates; environment failures open a fix and
// spec failures charge the estimator and author.
package blame

import (
	"fmt"
	"strings"
	"time"
)

// Class is the blame class of a failure.
type Class string

const (
	// ClassAgent marks a failure caused by the agent.
	ClassAgent Class = "agent"
	// ClassEnvironment marks a failure caused by the environment.
	ClassEnvironment Class = "environment"
	// ClassSpec marks a failure caused by the story spec.
	ClassSpec Class = "spec"
	// ClassUnclassified is the zero value: not yet attributed.
	ClassUnclassified Class = ""
)

// ParseClass parses s case-insensitively. The empty string maps to
// ClassUnclassified; anything else unknown is rejected.
func ParseClass(s string) (Class, error) {
	switch {
	case s == "":
		return ClassUnclassified, nil
	case strings.EqualFold(s, string(ClassAgent)):
		return ClassAgent, nil
	case strings.EqualFold(s, string(ClassEnvironment)):
		return ClassEnvironment, nil
	case strings.EqualFold(s, string(ClassSpec)):
		return ClassSpec, nil
	default:
		return "", fmt.Errorf("blame: unknown Class %q", s)
	}
}

// Evidence kinds per class.
const (
	// Agent-class evidence kinds.
	EvidenceGate      = "gate"
	EvidenceReview    = "review"
	EvidenceFalseDone = "false-done"

	// Environment-class evidence kinds.
	EvidenceQuota   = "quota"
	EvidenceAuth    = "auth"
	EvidenceNetwork = "network"
	EvidenceSandbox = "sandbox"
	EvidenceHung    = "hung-prompt"
	EvidenceHost    = "host"

	// Spec-class evidence kinds.
	EvidenceAmbiguity     = "ambiguity"
	EvidenceContradiction = "contradiction"
)

// Evidence backs a blame attribution: what kind of proof, a reference
// to it, and a note saying what it shows.
type Evidence struct {
	Kind string
	Ref  string
	Note string
}

// Attribution classes one failure with its supporting evidence.
type Attribution struct {
	Class    Class
	Evidence []Evidence
	By       string
	At       time.Time
}

// kindBelongs reports whether kind is admissible evidence for class.
func kindBelongs(class Class, kind string) bool {
	switch class {
	case ClassAgent:
		switch kind {
		case EvidenceGate, EvidenceReview, EvidenceFalseDone:
			return true
		}
	case ClassEnvironment:
		switch kind {
		case EvidenceQuota, EvidenceAuth, EvidenceNetwork,
			EvidenceSandbox, EvidenceHung, EvidenceHost:
			return true
		}
	case ClassSpec:
		switch kind {
		case EvidenceAmbiguity, EvidenceContradiction:
			return true
		}
	}
	return false
}

// Validate checks that the attribution carries the evidence its class
// requires. Unclassified is valid but unrated; every other class needs
// at least one evidence item whose Kind belongs to that class with a
// non-empty Ref; spec evidence also needs a non-empty Note; By must be
// non-empty for classified attributions.
func (a Attribution) Validate() error {
	switch a.Class {
	case ClassUnclassified:
		return nil
	case ClassAgent, ClassEnvironment, ClassSpec:
		// Handled below.
	default:
		return fmt.Errorf("blame: unknown Class %q", string(a.Class))
	}
	if len(a.Evidence) == 0 {
		return fmt.Errorf("blame: class %q needs >=1 evidence item", string(a.Class))
	}
	for i, ev := range a.Evidence {
		if !kindBelongs(a.Class, ev.Kind) {
			return fmt.Errorf("blame: evidence %d Kind %q does not belong to class %q", i, ev.Kind, string(a.Class))
		}
		if ev.Ref == "" {
			return fmt.Errorf("blame: evidence %d Ref must be non-empty", i)
		}
		if a.Class == ClassSpec && ev.Note == "" {
			return fmt.Errorf("blame: evidence %d Note must be non-empty for spec class", i)
		}
	}
	if a.By == "" {
		return fmt.Errorf("blame: By must be non-empty")
	}
	return nil
}

// Action is what follows from a blame attribution.
type Action string

const (
	// ActionRate moves the rating (agent failures only).
	ActionRate Action = "rate"
	// ActionOpenFix opens a fix (environment failures).
	ActionOpenFix Action = "open-fix"
	// ActionChargeEstimatorAndAuthor charges the estimator and author (spec failures).
	ActionChargeEstimatorAndAuthor Action = "charge-estimator-author"
	// ActionUnrated means no rating movement.
	ActionUnrated Action = "unrated"
)

// Consequence maps an attribution to its action: invalid or
// unclassified attributions are unrated; agent rates; environment
// opens a fix; spec charges the estimator and author.
func Consequence(a Attribution) Action {
	if err := a.Validate(); err != nil {
		return ActionUnrated
	}
	switch a.Class {
	case ClassAgent:
		return ActionRate
	case ClassEnvironment:
		return ActionOpenFix
	case ClassSpec:
		return ActionChargeEstimatorAndAuthor
	default:
		return ActionUnrated
	}
}

// Rates reports whether the attribution moves a rating.
func Rates(a Attribution) bool {
	return Consequence(a) == ActionRate
}
