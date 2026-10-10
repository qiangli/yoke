package fleet

import (
	"context"
	"sync"
)

// RegistryNoun is one registry noun's claim identity: the singular kind a
// claim names, how a name or alias resolves to its canonical holder, and the
// alias list a record carries. Either func may be nil when the noun keeps no
// such thing (skills are folders held by pkg/skills, not records here).
type RegistryNoun struct {
	Name    string
	Lookup  func(*Catalog, string) (string, bool)
	Aliases func(*Catalog, string) []string
}

// RegistryNouns lists every noun in the kind table, in table order. Claim
// providers derive from this, never from a hand-kept list, so a noun added
// to the table is claimable without further wiring.
func RegistryNouns() []RegistryNoun {
	kindsMu.RLock()
	specs := append([]kindSpec(nil), kinds...)
	kindsMu.RUnlock()
	testAliasesMu.RLock()
	defer testAliasesMu.RUnlock()
	out := make([]RegistryNoun, 0, len(specs))
	for _, spec := range specs {
		n := RegistryNoun{Name: spec.Name, Lookup: spec.Lookup}
		switch {
		case spec.Record != nil && spec.Record.aliases != nil:
			rec := spec.Record
			n.Aliases = func(c *Catalog, canonical string) []string {
				r, ok := rec.get(c, canonical)
				if !ok {
					return nil
				}
				return append([]string(nil), *rec.aliases(r)...)
			}
		case testAliases[spec.Name] != nil:
			n.Aliases = testAliases[spec.Name]
		}
		out = append(out, n)
	}
	return out
}

// ResolveEntry resolves name or alias to its canonical holder plus the
// record's aliases, against the ambient (environment-rooted) catalog. It
// reports false when the noun is unknown or the name resolves to nothing.
func ResolveEntry(noun, name string) (canonical string, aliases []string, ok bool) {
	for _, n := range RegistryNouns() {
		if n.Name != noun {
			continue
		}
		if n.Lookup == nil {
			return "", nil, false
		}
		cat := New()
		canon, ok := n.Lookup(cat, name)
		if !ok {
			return "", nil, false
		}
		if n.Aliases != nil {
			aliases = n.Aliases(cat, canon)
		}
		return canon, aliases, true
	}
	return "", nil, false
}

// mutationGuard vets a write to an existing entry against live claims. It is
// a hook — not a direct call — because the guard lives in pkg/policy/coord
// and coord already imports this package for identity; a direct call would
// close an import cycle. The fleetkinds package installs the real guard at
// init; nil (tests, bare imports) means no guard.
var mutationGuard func(ctx context.Context, kind, name string) error

// SetMutationGuard installs the write guard. Called once, by fleetkinds.
func SetMutationGuard(fn func(ctx context.Context, kind, name string) error) {
	mutationGuard = fn
}

// guardExisting asks the guard whether the caller may rewrite the entry name
// resolves to, and returns its refusal unchanged. Names that resolve to
// nothing are new entries: nothing holds them, so they pass. When sameName is
// set, only an entry stored under name itself is guarded — add uses this, so
// minting under a name another entry merely aliases stays claimName's
// refusal instead of surfacing as a claim conflict.
func guardExisting(ctx context.Context, cat *Catalog, kind, name string, sameName bool) error {
	if mutationGuard == nil {
		return nil
	}
	spec, ok := kindByName(kind)
	if !ok || spec.Lookup == nil {
		return nil
	}
	canon, ok := spec.Lookup(cat, name)
	if !ok || (sameName && canon != name) {
		return nil
	}
	return mutationGuard(ctx, kind, canon)
}

var (
	testAliasesMu sync.RWMutex
	testAliases   = map[string]func(*Catalog, string) []string{}
)

// RegisterTestNoun adds a noun to the kind table for tests in packages that
// cannot reach registerKind (notably fleetkinds, which imports this
// package). UnregisterTestNoun removes it; call it deferred.
func RegisterTestNoun(name string, lookup func(*Catalog, string) (string, bool), aliases func(*Catalog, string) []string) {
	registerKind(kindSpec{Name: name, Plural: name + "s", Lookup: lookup})
	if aliases != nil {
		testAliasesMu.Lock()
		testAliases[name] = aliases
		testAliasesMu.Unlock()
	}
}

// UnregisterTestNoun removes a noun added by RegisterTestNoun.
func UnregisterTestNoun(name string) {
	unregisterKind(name)
	testAliasesMu.Lock()
	delete(testAliases, name)
	testAliasesMu.Unlock()
}
