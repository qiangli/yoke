// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

// A FAMILY is a versioned, fixed binding configuration — the thing an instance
// runs AS. It is either one `tool:model`, or a predefined set of `tool:model`
// bindings plus the policy that selects among them (possibly across vendors).
//
// # Why this is not just "the agent"
//
// `claude:opus5.5` and `claude:sonnet5` are DIFFERENT families even though an
// operator says "claude" for both, and `Esme` is a display label on the first
// of them rather than a name for all claudes. Separating the configuration
// from the conversation is what lets two parallel contexts share one
// configuration without sharing one mailbox: the family is reusable and
// stateless, the instance (see instance.go) is the singleton that owns mail
// and ownership.
//
// # Versioning is derived, never declared alone
//
// The whole point of "fixed" is that a running instance's bindings cannot move
// under it. A declared version string alone cannot carry that guarantee —
// somebody edits the model set and forgets to bump it, and an instance keeps
// its identity across a configuration it never agreed to. So ID() hashes the
// CANONICAL CONFIGURATION (policy + sorted bindings + any declared version).
// Change the binding, the model set, or the selection policy and the ID
// changes whether or not anyone remembered to bump anything; leave them alone
// and the ID is the same on every host, forever, with no state to sync.
//
// Selecting a model that is ALREADY inside a composite's frozen set is not a
// configuration change — see Allows — so a cascade escalating from its base to
// an L4 keeps one identity, one conversation and one mailbox.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Selection policies. A single binding has nothing to select, which is why it
// gets its own name rather than an empty string that could also mean "unset".
const (
	// PolicySingle is one tool:model. There is nothing to choose.
	PolicySingle = "single"
	// PolicyCascade is the existing multi-model mechanism (Agent.Base +
	// Agent.Escalation): run cheap, escalate through the declared ladder. It is
	// reused here rather than reinvented — a composite family IS a cascade.
	PolicyCascade = "cascade"
)

// Family is a versioned, fixed binding configuration.
type Family struct {
	// Name is the configuration's catalog name — the agent entry it was
	// derived from. It is not the display label and not an address.
	Name string `yaml:"name" json:"name"`
	// Display is the label base a human says out loud ("Esme"). Instances draw
	// their labels from it (Esme, Esme-2, …); it is NOT an identity, so it is
	// deliberately not part of the configuration digest.
	Display string `yaml:"display,omitempty" json:"display,omitempty"`
	// Policy selects among Bindings. PolicySingle when there is one.
	Policy string `yaml:"policy" json:"policy"`
	// Bindings is the frozen tool:model set. One entry for a single family.
	Bindings []string `yaml:"bindings" json:"bindings"`
	// Version is an OPTIONAL operator-declared version. It participates in the
	// digest, so bumping it mints a new family — which is the escape hatch for
	// "same bindings, deliberately a new configuration".
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
}

// ErrBindingImmutable reports an attempt to move a live instance's bindings.
//
// It is a distinct error because the remedy is specific and non-obvious: you
// do not retry, and you do not force it. You open a NEW instance on the new
// family and hand off — the old conversation was had under the old
// configuration and silently continuing it under another one would make every
// earlier turn unattributable to the model that produced it.
var ErrBindingImmutable = errors.New("fleet: an instance's bindings are immutable; open a new instance on the new family and hand off")

// ErrFamilyEmpty reports a configuration with no bindings. A family with
// nothing to run is not a family.
var ErrFamilyEmpty = errors.New("fleet: a family needs at least one tool:model binding")

// ErrFamilyUnresolved reports a declared configuration that could not be
// resolved in full — a cascade rung naming an agent this catalog does not
// have, or a binding with no model.
//
// It is an ERROR rather than a shorter ladder. The frozen binding set is what
// later refuses a reconfiguration, so a silently skipped rung produces an
// instance whose set is missing a model it should have been allowed to select
// — and, worse, makes ID() depend on how complete the reader's catalog happened
// to be, so two hosts would compute two different identities for one declared
// family. Refusing keeps the configuration a fact about the declaration.
var ErrFamilyUnresolved = errors.New("fleet: family configuration does not resolve")

// configDigestBytes is the width of the configuration fingerprint in ID().
//
// 16 bytes (128 bits), not 4. The ID is a durable key — instances freeze it,
// and the ratings work in #1269 keys on it — so a collision would silently
// merge two different configurations' identities and histories. 32 bits is
// roughly even odds of a collision in ~77k configurations by the birthday
// bound, which is not a margin to hand a key that outlives the process.
const configDigestBytes = 16

// Composite reports a predefined multi-binding configuration.
func (f Family) Composite() bool { return len(f.Bindings) > 1 || f.Policy == PolicyCascade }

// Config is the canonical text of the configuration, in DECLARED ORDER.
//
// The order is part of the configuration and must NOT be normalised away. For
// a cascade the sequence IS the selection policy — base first, then each rung
// in the order it escalates — so [sonnet,opus] and [opus,sonnet] are two
// different behaviours that happen to name the same two models. An earlier
// draft sorted the bindings on the theory that "a set written in two orders is
// one set"; that is true of a set and false of a ladder, and it made reordering
// an escalation ladder invisible to ID(). A live instance would then have kept
// its identity across a policy change it never agreed to, which is exactly the
// silent reconfiguration this type exists to prevent.
func (f Family) Config() string {
	b := make([]string, 0, len(f.Bindings))
	for _, s := range f.Bindings {
		if s = strings.TrimSpace(s); s != "" {
			b = append(b, s)
		}
	}
	policy := f.Policy
	if policy == "" {
		policy = PolicySingle
	}
	return policy + "|" + strings.Join(b, ",") + "|" + strings.TrimSpace(f.Version)
}

// ID is the family's stable identity: kind, name, and a digest of the
// configuration. Two hosts reading the same configuration compute the same ID;
// any change to the bindings or the policy computes a different one.
func (f Family) ID() string {
	kind := "single"
	if f.Composite() {
		kind = "composite"
	}
	sum := sha256.Sum256([]byte(f.Config()))
	return kind + ":" + strings.TrimSpace(f.Name) + "@" + hex.EncodeToString(sum[:configDigestBytes])
}

// Allows reports whether a binding is inside the frozen set.
//
// This is the whole of the "selection preserves identity" rule: inside the set
// is a SELECTION (allowed, same instance), outside it is a RECONFIGURATION
// (ErrBindingImmutable, new instance plus handoff).
func (f Family) Allows(binding string) bool {
	want := strings.ToLower(strings.TrimSpace(binding))
	for _, b := range f.Bindings {
		if strings.ToLower(strings.TrimSpace(b)) == want {
			return true
		}
	}
	return false
}

// Select picks a binding for the next turn. Inside the frozen set it succeeds
// and the instance keeps its UUID, mail and ownership; outside it refuses.
func (f Family) Select(binding string) (string, error) {
	b := strings.TrimSpace(binding)
	if b == "" {
		if len(f.Bindings) == 0 {
			return "", ErrFamilyEmpty
		}
		return f.Bindings[0], nil
	}
	if !f.Allows(b) {
		return "", ErrBindingImmutable
	}
	return b, nil
}

// FamilyOf derives the family a catalog agent entry declares.
//
// A plain entry is one binding. A CASCADE entry (BandSource "cascade", with a
// Base and an Escalation ladder) is the predefined composite: its bindings are
// the base's binding followed by each rung's, IN DECLARED ORDER, and its policy
// is PolicyCascade. That reuse is deliberate — the fleet already had a
// multi-model mechanism and a second one would be a second thing to keep
// honest.
//
// A rung that names no resolvable agent is an ERROR (ErrFamilyUnresolved), not
// a skipped rung: see that error for why a shortened ladder is worse than a
// refusal. ok=false still means "no such agent", which is a question, not a
// malformed answer.
func (c *Catalog) FamilyOf(name string) (Family, bool, error) {
	a, ok := c.Agent(name)
	if !ok {
		return Family{}, false, nil
	}
	f := Family{Name: a.Name, Display: a.NickName()}
	if !a.IsCascade() {
		key := a.MatrixKey()
		if key == "" || key == ":" {
			return Family{}, true, fmt.Errorf("%w: agent %s declares no tool:model binding", ErrFamilyUnresolved, a.Name)
		}
		f.Policy, f.Bindings = PolicySingle, []string{key}
		return f, true, nil
	}
	f.Policy = PolicyCascade
	seen := map[string]bool{}
	add := func(rungName, role string) error {
		rungName = strings.TrimSpace(rungName)
		if rungName == "" {
			return nil
		}
		rung, found := c.Agent(rungName)
		if !found {
			return fmt.Errorf("%w: %s %s names agent %q, which this catalog does not have", ErrFamilyUnresolved, a.Name, role, rungName)
		}
		key := rung.MatrixKey()
		if key == "" || key == ":" {
			return fmt.Errorf("%w: %s %s %q declares no tool:model binding", ErrFamilyUnresolved, a.Name, role, rungName)
		}
		// A repeated rung is dropped rather than refused: naming the same
		// tool:model twice in a ladder is redundant, not unresolved, and the
		// set it describes is unambiguous.
		if seen[key] {
			return nil
		}
		seen[key] = true
		f.Bindings = append(f.Bindings, key)
		return nil
	}
	if err := add(a.Base, "base"); err != nil {
		return Family{}, true, err
	}
	for i, rung := range a.Escalation {
		if err := add(rung, "escalation rung "+strconv.Itoa(i+1)); err != nil {
			return Family{}, true, err
		}
	}
	if len(f.Bindings) == 0 {
		return Family{}, true, fmt.Errorf("%w: cascade %s resolves to no bindings", ErrFamilyUnresolved, a.Name)
	}
	return f, true, nil
}
