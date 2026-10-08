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
	"sort"
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

// Composite reports a predefined multi-binding configuration.
func (f Family) Composite() bool { return len(f.Bindings) > 1 || f.Policy == PolicyCascade }

// Config is the canonical text of the configuration: order-independent in the
// bindings, because a set written in two orders is one set.
func (f Family) Config() string {
	b := make([]string, 0, len(f.Bindings))
	for _, s := range f.Bindings {
		if s = strings.TrimSpace(s); s != "" {
			b = append(b, s)
		}
	}
	sort.Strings(b)
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
	return kind + ":" + strings.TrimSpace(f.Name) + "@" + hex.EncodeToString(sum[:4])
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
// the base's binding followed by each rung's, in declared order, and its policy
// is PolicyCascade. That reuse is deliberate — the fleet already had a
// multi-model mechanism and a second one would be a second thing to keep
// honest.
//
// A rung that names no resolvable agent is SKIPPED rather than guessed at: a
// fabricated binding in a frozen set is worse than a shorter ladder, because
// the set is what later refuses a reconfiguration.
func (c *Catalog) FamilyOf(name string) (Family, bool) {
	a, ok := c.Agent(name)
	if !ok {
		return Family{}, false
	}
	f := Family{Name: a.Name, Display: a.NickName()}
	if !a.IsCascade() {
		f.Policy, f.Bindings = PolicySingle, []string{a.MatrixKey()}
		return f, true
	}
	f.Policy = PolicyCascade
	seen := map[string]bool{}
	add := func(agentName string) {
		rung, found := c.Agent(agentName)
		if !found {
			return
		}
		key := rung.MatrixKey()
		if key == ":" || seen[key] {
			return
		}
		seen[key] = true
		f.Bindings = append(f.Bindings, key)
	}
	add(a.Base)
	for _, rung := range a.Escalation {
		add(rung)
	}
	if len(f.Bindings) == 0 {
		return Family{}, false
	}
	return f, true
}
