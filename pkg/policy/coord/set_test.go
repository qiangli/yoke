// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"context"
	"testing"
)

// setProvider is a registry whose entries are SETS held under a declared
// kind — what a fleet resource is: "pool-a" and "pool-b" are name-matched
// model sets sharing one member, "tree" is a path set.
type setProvider struct{}

func (setProvider) Kind() Kind { return Kind{Name: "setw-s4", Match: MatchName, Domain: "setw-s4"} }

func (setProvider) Exists(string) bool { return true }

func (setProvider) Members(name string) ([]string, error) {
	switch name {
	case "pool-a":
		return []string{"m-one", "m-two"}, nil
	case "pool-b":
		return []string{"m-two", "m-three"}, nil
	case "tree":
		return []string{"/w/tree"}, nil
	}
	return []string{name}, nil
}

func (setProvider) EffectiveKind(name string) (Kind, bool) {
	if name == "tree" {
		k, ok := LookupKind("path")
		return k, ok
	}
	return Kind{Name: "setmodel-s4", Match: MatchName, Domain: "setmodel-s4"}, true
}

// A set claim covers its MEMBERS under the declared kind's rule, not its own
// name: a direct claim on a member is refused while the set is held, the set
// is refused while a member is held, and two sets sharing a member exclude
// each other. The set's own name is a label and matches nothing.
func TestSetClaimMatchesMembersNotName(t *testing.T) {
	ledger(t)
	RegisterProvider(setProvider{})
	RegisterKind(Kind{Name: "setmodel-s4", Match: MatchName, Domain: "setmodel-s4"})
	ctx := context.Background()
	set := Ref{Kind: "setw-s4", Name: "pool-a"}

	// Set first, member second.
	if _, err := AcquireRef(ctx, Request{Ref: set, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []Use{
		{Kind: "setmodel-s4", Name: "m-one"},
		{Kind: "setmodel-s4", Name: "m-two", Member: "m-two"},
		{Kind: "setw-s4", Name: "pool-b"}, // shares m-two
	} {
		if c := guardErr(t, Guard(ctx, agentB(), u)); c.Claim.Address() != set {
			t.Errorf("%+v: conflict = %v, want %v", u, c.Claim.Address(), set)
		}
		if err := Guard(ctx, agentA(), u); err != nil {
			t.Errorf("%+v: the holder was refused: %v", u, err)
		}
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{Kind: "setmodel-s4", Name: "m-one"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("member claim admitted over the set: %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{Kind: "setw-s4", Name: "pool-b"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("second set sharing a member admitted: %v", err)
	}
	// The set's name is not a member: nothing of the declared kind is called
	// "pool-a", and a member outside the set is free.
	for _, u := range []Use{
		{Kind: "setmodel-s4", Name: "pool-a"},
		{Kind: "setmodel-s4", Name: "m-three"},
	} {
		if err := Guard(ctx, agentB(), u); err != nil {
			t.Errorf("%+v: refused outside the set: %v", u, err)
		}
	}
	if err := ReleaseRef(ctx, set, agentA(), 0); err != nil {
		t.Fatal(err)
	}

	// Member first, set second: the set is refused while a member is held.
	member := Ref{Kind: "setmodel-s4", Name: "m-two"}
	if _, err := AcquireRef(ctx, Request{Ref: member, Holder: agentB()}); err != nil {
		t.Fatal(err)
	}
	if c := guardErr(t, Guard(ctx, agentA(), Use{Kind: "setw-s4", Name: "pool-a"})); c.Claim.Ref() != member {
		t.Fatalf("conflict = %v, want %v", c.Claim.Ref(), member)
	}
	if _, err := AcquireRef(ctx, Request{Ref: set, Holder: agentA()}); !isConflict(err) {
		t.Fatalf("set admitted over a held member: %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{Kind: "setw-s4", Name: "pool-b"}, Holder: agentA()}); !isConflict(err) {
		t.Fatalf("other set admitted over a held member: %v", err)
	}
	// Guard and AcquireRef agree on the free case too.
	if err := Guard(ctx, agentA(), Use{Kind: "setmodel-s4", Name: "m-one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{Kind: "setmodel-s4", Name: "m-one"}, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
}

// A path-kind set conflicts by containment over its members, in both
// directions.
func TestSetClaimPathContainment(t *testing.T) {
	ledger(t)
	RegisterProvider(setProvider{})
	ctx := context.Background()
	tree := Ref{Kind: "setw-s4", Name: "tree"}

	if _, err := AcquireRef(ctx, Request{Ref: tree, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	if c := guardErr(t, Guard(ctx, agentB(), Use{Kind: "path", Name: "/w/tree/sub/file.go"})); c.Claim.Address() != tree {
		t.Fatalf("conflict = %v, want %v", c.Claim.Address(), tree)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{Kind: "path", Name: "/w/tree/sub"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("path under the set admitted: %v", err)
	}
	if err := Guard(ctx, agentB(), Use{Kind: "path", Name: "/w/elsewhere"}); err != nil {
		t.Fatalf("path outside the set refused: %v", err)
	}
	if err := ReleaseRef(ctx, tree, agentA(), 0); err != nil {
		t.Fatal(err)
	}

	// A path claim ABOVE the set's members holds the set too.
	if _, err := AcquireRef(ctx, Request{Ref: Ref{Kind: "path", Name: "/w"}, Holder: agentB()}); err != nil {
		t.Fatal(err)
	}
	if err := Guard(ctx, agentA(), Use{Kind: "setw-s4", Name: "tree"}); !isConflict(err) {
		t.Fatalf("set admitted under a held parent path: %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: tree, Holder: agentA()}); !isConflict(err) {
		t.Fatalf("set claim admitted under a held parent path: %v", err)
	}
}
