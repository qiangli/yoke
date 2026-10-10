// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"context"
	"testing"
)

func TestRegisterRegistryKindClaimsByName(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	known := map[string][]string{"gpu": {"gpu", "gpu-0"}}
	RegisterRegistryKind("test-widget", func(name string) bool {
		_, ok := known[name]
		return ok
	}, func(name string) ([]string, error) { return known[name], nil })

	k, ok := LookupKind("test-widget")
	if !ok || k.Match != MatchName || k.Domain != "test-widget" {
		t.Fatalf("kind = %+v ok=%v", k, ok)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"test-widget", "gpu"}, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	g, err := AcquireRef(ctx, Request{Ref: Ref{"test-widget", "gpu"}, Holder: agentA()})
	if err != nil || len(g.Claim.Members) != 2 {
		t.Fatalf("reuse = %+v err=%v", g.Claim, err)
	}
	if err := Guard(ctx, agentB(), Use{Kind: "test-widget", Name: "gpu"}); !isConflict(err) {
		t.Fatalf("other holder admitted: %v", err)
	}
	if err := Guard(ctx, agentA(), Use{Kind: "test-widget", Name: "gpu"}); err != nil {
		t.Fatalf("holder refused: %v", err)
	}
	if err := Guard(ctx, agentB(), Use{Kind: "test-widget", Name: "other"}); err != nil {
		t.Fatalf("unrelated name refused: %v", err)
	}
}

func TestRegisterRegistryKindDefaults(t *testing.T) {
	ledger(t)
	RegisterRegistryKind("test-bare", nil, nil)
	k, ok := LookupKind("test-bare")
	if !ok || k.Match != MatchName {
		t.Fatalf("kind = %+v ok=%v", k, ok)
	}
	p, ok := providerFor("test-bare")
	if !ok {
		t.Fatal("no provider")
	}
	if p.Exists("x") {
		t.Fatal("nil exists reported present")
	}
	m, err := p.Members("x")
	if err != nil || len(m) != 1 || m[0] != "x" {
		t.Fatalf("members = %v err=%v", m, err)
	}
}
