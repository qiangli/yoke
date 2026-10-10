// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"context"
	"testing"
)

// effProvider is a provider whose names claim under another kind: the
// generic mechanism a registry (fleet resources, skills, herald peers) uses
// to give its entries the claim semantics of a declared kind, with no
// per-registry code in the engine.
type effProvider struct{}

func (effProvider) Kind() Kind { return Kind{Name: "effw-s2", Match: MatchName, Domain: "effw-s2"} }

func (effProvider) Exists(string) bool { return true }

func (effProvider) Members(string) ([]string, error) { return []string{"/w/eff"}, nil }

func (effProvider) EffectiveKind(name string) (Kind, bool) {
	k, ok := LookupKind("path")
	if !ok {
		return Kind{}, false
	}
	return k, true
}

func (effProvider) MapBare(name string) (Ref, bool) {
	if name != "effbare-s2" {
		return Ref{}, false
	}
	return Ref{Kind: "effw-s2", Name: "w"}, true
}

// A provider's effective kind governs matching while the claim stays stored
// under the requested ref, and a mapped bare name claims as its full ref.
func TestEffectiveKindAndBareMapping(t *testing.T) {
	ledger(t)
	RegisterProvider(effProvider{})
	ctx := context.Background()

	g, err := AcquireRef(ctx, Request{Ref: Ref{Kind: "effw-s2", Name: "w"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	if g.Claim.Kind != "path" {
		t.Fatalf("stored kind = %q, want the effective kind", g.Claim.Kind)
	}
	// A path claim beneath the provider's members conflicts under path rules.
	if _, err := AcquireRef(ctx, Request{Ref: Ref{Kind: "path", Name: "/w/eff/x"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("path claim under effective members admitted: %v", err)
	}
	// The mapped bare name claims as the full ref: same key, same conflict.
	if _, err := AcquireRef(ctx, Request{Ref: ParseRef("effbare-s2"), Holder: agentB()}); !isConflict(err) {
		t.Fatalf("mapped bare name admitted over a live claim: %v", err)
	}
	// Names no provider claims keep their meaning: a plain bare claim works
	// and is stored under its own kind.
	g2, err := AcquireRef(ctx, Request{Ref: ParseRef("plain-s2-xyz"), Holder: agentB()})
	if err != nil {
		t.Fatal(err)
	}
	if g2.Claim.Kind != KindName {
		t.Fatalf("stored kind = %q, want %q", g2.Claim.Kind, KindName)
	}
	if err := Guard(ctx, agentB(), Use{Kind: "effw-s2", Name: "w"}); !isConflict(err) {
		t.Fatalf("guard admitted over a live effective-kind claim: %v", err)
	}
}
