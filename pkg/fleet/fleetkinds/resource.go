// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package fleetkinds

import (
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/policy/coord"
)

// resourceProvider is the KindProvider for the "resource" fleet noun: the
// operator's names for things agents must not use at the same time.
//
// A resource carries no claim logic of its own — its record names the claim
// kind it is held under and the members those claims cover. Members come
// straight from the record, and EffectiveKind resolves the record's declared
// kind to the coord kind the engine matches under, so a resource:NAME claim
// conflicts exactly as a claim of its kind over its members would. MapBare
// lets a bare name that names a registered resource claim as it. There is no
// resource-specific code in the engine: this provider is the whole of it,
// registered through the same table every fleet noun uses.
type resourceProvider struct{}

func (resourceProvider) Kind() coord.Kind {
	return coord.Kind{Name: fleet.KindResource, Match: coord.MatchName, Domain: fleet.KindResource}
}

func (resourceProvider) Exists(name string) bool {
	_, _, ok := fleet.ResolveEntry(fleet.KindResource, name)
	return ok
}

func (resourceProvider) Members(name string) ([]string, error) {
	rec, ok := fleet.New().Resource(name)
	if !ok {
		return []string{name}, nil
	}
	if len(rec.Members) == 0 {
		return []string{rec.Name}, nil
	}
	return append([]string(nil), rec.Members...), nil
}

// EffectiveKind resolves a resource's declared kind to the coord kind the
// engine matches under. A resourcekind record of that name wins — it is the
// operator's live definition — then a registered coord kind (a builtin like
// "path", or a user kind registered at load), else a plain name-matched
// kind of that name, so a typo is still a claim rather than a crash. The
// resource's own TTL wins when set; its mode, when set, narrows the kind's
// permitted modes to that one.
func (resourceProvider) EffectiveKind(name string) (coord.Kind, bool) {
	cat := fleet.New()
	rec, ok := cat.Resource(name)
	if !ok || rec.Kind == "" {
		return coord.Kind{}, false
	}
	k := lookupResourceKind(cat, rec.Kind)
	if rec.TTL != "" {
		if d, err := time.ParseDuration(rec.TTL); err == nil {
			k.TTL = d
		}
	}
	if rec.Mode != "" {
		k.Modes = []string{rec.Mode}
	}
	return k, true
}

// MapBare claims a bare name that names a registered resource (by canonical
// name or alias) as that resource, canonically. Anything else is not ours.
func (resourceProvider) MapBare(name string) (coord.Ref, bool) {
	canon, _, ok := fleet.ResolveEntry(fleet.KindResource, name)
	if !ok {
		return coord.Ref{}, false
	}
	return coord.Ref{Kind: fleet.KindResource, Name: canon}, true
}

// lookupResourceKind maps a claim-kind name to its coord kind: the live
// resourcekind record first, then the registered kind, else a plain
// name-matched kind of that name.
func lookupResourceKind(cat *fleet.Catalog, name string) coord.Kind {
	if rec, ok := cat.ResourceKind(name); ok && rec.Name != "" {
		return mapResourceKind(rec)
	}
	if k, ok := coord.LookupKind(name); ok {
		return k
	}
	return coord.Kind{Name: name, Match: coord.MatchName}
}

// mapResourceKind projects a resourcekind record onto its coord kind.
func mapResourceKind(rec fleet.ResourceKind) coord.Kind {
	k := coord.Kind{
		Name:    rec.Name,
		Domain:  rec.Domain,
		Modes:   append([]string(nil), rec.Modes...),
		Resolve: rec.Resolve,
		Probe:   rec.Probe,
	}
	switch rec.Match {
	case "member":
		k.Match = coord.MatchMember
	case "path":
		k.Match = coord.MatchPath
	default:
		k.Match = coord.MatchName
	}
	if rec.TTL != "" {
		if d, err := time.ParseDuration(rec.TTL); err == nil {
			k.TTL = d
		}
	}
	return k
}

// kindHook is the KindProvider for one user kind: a resourcekind record.
// Members resolve through the record's resolve hook (or claim just the name
// when there is none); existence probes through its probe hook (or reports
// absent when there is none). Registering the provider registers the kind,
// so user kinds work wherever builtin kinds do.
type kindHook struct{ rec fleet.ResourceKind }

func (h kindHook) Kind() coord.Kind { return mapResourceKind(h.rec) }

func (h kindHook) Exists(name string) bool {
	if h.rec.Probe == "" {
		return false
	}
	st, err := runProbe(h.rec.Probe, name)
	if err != nil {
		return false
	}
	return st == ProbeFree || st == ProbeBusy
}

func (h kindHook) Members(name string) ([]string, error) {
	if h.rec.Resolve == "" {
		return []string{name}, nil
	}
	return runResolve(h.rec.Resolve, name)
}
