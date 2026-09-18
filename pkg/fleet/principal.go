// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

import (
	"errors"
	"strings"
)

// CanonicalPrincipal resolves a name to a registered principal that can OWN
// work — an agent or a person — and reports which kind it turned out to be.
//
// # Why this exists
//
// Four surfaces each answered "is this a valid owner?" separately, and every
// one of them answered it with `Catalog.Agent` alone: todo's assignee, weave's
// sprint manager, weave's reachability check, and meet's seat. So a human was
// accepted by the message board — `bus.BoardIdentity` takes any non-empty name —
// and refused everywhere else, which is how `todo add --owner <person>` and
// `sprint take --owner <person>` both failed with "is not a registered agent"
// while the same name posted on mb perfectly well.
//
// That is one defect wearing four error messages, and the fix is one resolver
// rather than four loosened checks: a surface that decides membership on its own
// is a surface that will drift from the others again.
//
// # What it deliberately does NOT decide
//
// Whether a principal can be given a TURN. A meet seat is invoked — something
// has to run and produce text — so meet keeps requiring an agent for a
// participant, and humans attend through `State.Human`/`Observers`. Ownership
// and turn-taking are different questions, and collapsing them would seat a
// human the room then waits on forever.
//
// # Case
//
// Agents already resolve case-insensitively; people did not, because
// `Catalog.Person` compares names exactly. Owning work should not depend on how
// a name was capitalised, so the person lookup is retried folded here. The
// underlying `Person` is left alone: its callers ask a narrower question, and
// widening it is a separate decision.
func (c *Catalog) CanonicalPrincipal(name string) (canonical, kind string, ok bool) {
	canonical, kind, err := c.ResolvePrincipal(name)
	return canonical, kind, err == nil
}

// ErrPrincipalUnknown and ErrPrincipalAmbiguous are the two ways a name can fail
// to name one owner, and they are SEPARATE on purpose.
//
// "Unknown" means register it. "Ambiguous" means qualify it — the name is real
// and matches several principals. Collapsing them tells somebody whose name
// collides to go and create it again, which is the opposite of the fix, and it
// is the same "fail with choices rather than guess" rule whois already follows.
var (
	ErrPrincipalUnknown   = errors.New("names no registered principal")
	ErrPrincipalAmbiguous = errors.New("names more than one registered principal")
)

// ResolvePrincipal is CanonicalPrincipal with the reason attached.
func (c *Catalog) ResolvePrincipal(name string) (canonical, kind string, err error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", "", ErrPrincipalUnknown
	}
	if n := c.principalNameMatches(n); n > 1 {
		return "", "", ErrPrincipalAmbiguous
	}
	if a, found := c.Agent(n); found {
		return a.Name, KindAgent, nil
	}
	if p, found := c.Person(n); found {
		return p.Handle, KindPerson, nil
	}
	// Fold case for people only — see the doc comment.
	folded := strings.ToLower(n)
	people, _ := c.People()
	for _, p := range people {
		for _, alias := range p.Names() {
			if strings.ToLower(strings.TrimSpace(alias)) == folded {
				return p.Handle, KindPerson, nil
			}
		}
	}
	return "", "", ErrPrincipalUnknown
}

// principalNameMatches counts registered principals a name could mean. It is a
// signal for the ERROR only — resolution stays with Agent/Person, which know
// about family aliases and bindings. Counting here would be a second matcher and
// would eventually disagree with the first.
func (c *Catalog) principalNameMatches(name string) int {
	folded := strings.ToLower(strings.TrimSpace(name))
	hit := func(names []string) bool {
		for _, alias := range names {
			if strings.ToLower(strings.TrimSpace(alias)) == folded {
				return true
			}
		}
		return false
	}
	// Count matching ENTRIES, not distinct names. Two agents that share one
	// canonical Name are the commonest ambiguity in this tree, and keying the
	// count by name would fold those two back into one and report the collision
	// as "unknown" — telling the operator to register a name that already exists
	// twice. An entry matching on several of its own aliases still counts once.
	n := 0
	agents, _ := c.agentEntries()
	for _, a := range agents {
		if hit(a.Names()) {
			n++
		}
	}
	people, _ := c.People()
	for _, p := range people {
		if hit(p.Names()) {
			n++
		}
	}
	return n
}

// UnknownPrincipalHint is the one message every surface uses when a name owns
// nothing. It names BOTH registries, because the commonest cause of the old
// "is not a registered agent" was a human who was never registered at all and
// was being pointed at a list they would never appear in.
func UnknownPrincipalHint(name string) string {
	return "choose an agent from `bashy agent list` or a person from `bashy person list`" +
		" (`bashy person add " + strings.TrimSpace(name) + "` registers a human)"
}
