// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package fleetkinds registers one claim kind per fleet noun and installs
// the fleet write guard.
//
// It lives apart from pkg/fleet because the guard lives in
// pkg/policy/coord and coord already imports fleet for identity
// (coord.Self resolves the holder through the catalog): a provider inside
// fleet would close an import cycle. This package imports both and is
// blank-imported by the embedding shell's agentos wiring, which is where
// the kinds become active in production; importing it in tests wires the
// same behavior under test roots.
package fleetkinds

import (
	"context"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/policy/coord"
)

// nounProvider is a coord.KindProvider over one fleet noun: the kind names
// the singular noun, name-matched in its own domain. Existence and members
// come from the catalog rings, so a record added, renamed or removed is
// claimed as it stands, with no per-noun code.
type nounProvider struct{ noun fleet.RegistryNoun }

func (p nounProvider) Kind() coord.Kind {
	return coord.Kind{Name: p.noun.Name, Match: coord.MatchName, Domain: p.noun.Name}
}

func (p nounProvider) Exists(name string) bool {
	_, _, ok := fleet.ResolveEntry(p.noun.Name, name)
	return ok
}

func (p nounProvider) Members(name string) ([]string, error) {
	canon, aliases, ok := fleet.ResolveEntry(p.noun.Name, name)
	if !ok {
		return []string{name}, nil
	}
	out := []string{canon}
	seen := map[string]bool{canon: true}
	for _, a := range aliases {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out, nil
}

// Sync registers one provider per noun currently in the fleet kind table,
// plus one provider per resourcekind record. Providers derive from the
// table, never from a hand-kept list, so call it again after the table
// changes (a future noun, a test noun, a new resourcekind) to pick the new
// noun up. The resource noun gets the resource provider — its records name
// the kind they are held under — instead of the plain noun provider.
func Sync() {
	for _, n := range fleet.RegistryNouns() {
		if n.Name == fleet.KindResource {
			coord.RegisterProvider(resourceProvider{})
			continue
		}
		coord.RegisterProvider(nounProvider{noun: n})
	}
	// User kinds: every resourcekind record registers its coord kind (and
	// the provider that runs its hooks), so user kinds work wherever
	// builtin kinds do.
	if recs, _ := fleet.New().ResourceKinds(); len(recs) > 0 {
		for _, r := range recs {
			if r.Name == "" {
				continue
			}
			coord.RegisterProvider(kindHook{rec: r})
		}
	}
}

func init() {
	Sync()
	// Writes to an existing entry refuse against a live foreign claim. The
	// refusal surfaces unchanged: the Conflict names the holder and how to
	// reach them, and the fleet verb adds nothing to it.
	fleet.SetMutationGuard(func(ctx context.Context, kind, name string) error {
		return coord.Guard(ctx, coord.Self(), coord.Use{Kind: kind, Name: name})
	})
}
