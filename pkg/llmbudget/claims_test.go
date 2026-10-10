package llmbudget

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

func claimA() principal.Ref { return principal.Ref{Name: "claude-a", Episode: "ep-aaa", Host: "h"} }
func claimB() principal.Ref { return principal.Ref{Name: "codex-b", Episode: "ep-bbb", Host: "h"} }

// claimLedger isolates the coord registry and makes the gate's caller a fixed
// identity; it returns a setter for who the gate believes is asking.
func claimLedger(t *testing.T) func(principal.Ref) {
	t.Helper()
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
	prev := claimHolder
	t.Cleanup(func() { claimHolder = prev })
	return func(h principal.Ref) { claimHolder = func() principal.Ref { return h } }
}

func conflictOf(t *testing.T, err error) *coord.Conflict {
	t.Helper()
	var c *coord.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("err = %v, want wrapped *coord.Conflict", err)
	}
	return c
}

func TestClaimedModelRefusesNonHolderAdmitsHolder(t *testing.T) {
	ctx := context.Background()
	as := claimLedger(t)
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	if _, err := coord.AcquireRef(ctx, coord.Request{Ref: coord.Ref{Kind: "model", Name: "one"}, Holder: claimA(), Intent: "eval run"}); err != nil {
		t.Fatal(err)
	}
	as(claimB())
	a, err := g.Reserve(ctx, demand("a", o))
	c := conflictOf(t, err)
	if c.Claim.Holder.Name != "claude-a" || c.Claim.Ref() != (coord.Ref{Kind: "model", Name: "one"}) {
		t.Fatalf("conflict = %+v", c.Claim)
	}
	if a.Decision.Action != Block || a.Reservation != nil || a.Decision.Reason != err.Error() {
		t.Fatalf("admission = %+v", a)
	}
	if len(g.state.Reservations) != 0 {
		t.Fatal("refused request reserved capacity")
	}
	if _, e := os.Stat(g.cfg.StatePath); !os.IsNotExist(e) {
		t.Fatal("refused request published meter")
	}
	if _, err := g.Preview(ctx, demand("a", o)); !errors.As(err, new(*coord.Conflict)) {
		t.Fatalf("preview admitted a claimed model: %v", err)
	}
	// Another model is untouched by the claim.
	other := demand("b", o)
	other.Model = "two"
	if a, err := g.Reserve(ctx, other); err != nil || a.Reservation == nil {
		t.Fatalf("unclaimed model refused: %+v %v", a, err)
	}
	if err := g.Release(ctx, "b", o.ID()); err != nil {
		t.Fatal(err)
	}
	as(claimA())
	if a, err := g.Reserve(ctx, demand("a", o)); err != nil || a.Decision.Action != Allow || a.Reservation == nil {
		t.Fatalf("the holder was refused its own model: %+v %v", a, err)
	}
}

func TestClaimedMemberModelRefuses(t *testing.T) {
	ctx := context.Background()
	as := claimLedger(t)
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	// A pool kind in the model domain whose members are model names.
	coord.RegisterKind(coord.Kind{Name: "modelpool-test", Domain: "model", Match: coord.MatchMember})
	if _, err := coord.AcquireRef(ctx, coord.Request{Ref: coord.Ref{Kind: "modelpool-test", Name: "frontier"}, Members: []string{"one", "three"}, Holder: claimA()}); err != nil {
		t.Fatal(err)
	}
	as(claimB())
	_, err := g.Reserve(ctx, demand("a", o))
	if c := conflictOf(t, err); c.Claim.Ref() != (coord.Ref{Kind: "modelpool-test", Name: "frontier"}) {
		t.Fatalf("conflict = %+v", c.Claim)
	}
	if len(g.state.Reservations) != 0 {
		t.Fatal("refused request reserved capacity")
	}
	other := demand("b", o)
	other.Model = "two"
	if a, err := g.Reserve(ctx, other); err != nil || a.Reservation == nil {
		t.Fatalf("model outside the pool refused: %+v %v", a, err)
	}
	if err := g.Release(ctx, "b", o.ID()); err != nil {
		t.Fatal(err)
	}
	as(claimA())
	if a, err := g.Reserve(ctx, demand("a", o)); err != nil || a.Reservation == nil {
		t.Fatalf("the pool holder was refused: %+v %v", a, err)
	}
}

func TestClaimedProviderAccountAgentHostRefuse(t *testing.T) {
	ctx := context.Background()
	as := claimLedger(t)
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	for _, k := range []string{"provider", "account"} {
		if _, ok := coord.LookupKind(k); !ok {
			t.Fatalf("built-in kind %q not registered", k)
		}
	}
	as(claimB())
	// provider and account are resolved from the binding, agent and host come
	// with the request; each axis alone refuses.
	for _, ref := range []coord.Ref{{Kind: "provider", Name: "vendor"}, {Kind: "account", Name: "account"}, {Kind: "agent", Name: "nick"}, {Kind: "host", Name: "host-a"}} {
		grant, err := coord.AcquireRef(ctx, coord.Request{Ref: ref, Holder: claimA()})
		if err != nil {
			t.Fatal(err)
		}
		r := demand("a", o)
		r.Agent = "nick"
		_, err = g.Reserve(ctx, r)
		if c := conflictOf(t, err); c.Claim.Ref() != ref {
			t.Fatalf("%v: conflict = %+v", ref, c.Claim)
		}
		if err := coord.ReleaseRef(ctx, ref, claimA(), grant.Epoch); err != nil {
			t.Fatal(err)
		}
	}
	if a, err := g.Reserve(ctx, demand("a", o)); err != nil || a.Reservation == nil {
		t.Fatalf("released claims still refuse: %+v %v", a, err)
	}
}

func TestNoClaimLeavesAdmissionUnchanged(t *testing.T) {
	ctx := context.Background()
	as := claimLedger(t)
	as(claimB())
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	// Someone else's claim in another domain is not this request's business.
	if _, err := coord.AcquireRef(ctx, coord.Request{Ref: coord.Ref{Kind: "repo", Name: "yoke"}, Members: []string{"/w/yoke"}, Holder: claimA()}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.AcquireRef(ctx, coord.Request{Ref: coord.Ref{Kind: "model", Name: "one"}, Holder: claimA(), Mode: coord.ModeAnnounce}); err != nil {
		t.Fatal(err)
	}
	a, err := g.Reserve(ctx, demand("a", o))
	if err != nil || a.Decision.Action != Allow || a.Reservation == nil {
		t.Fatalf("reserve: %+v %v", a, err)
	}
	// Host-only work names no model and still passes.
	hostOnly := Request{ID: "h", Owner: o.ID(), Host: "host-a", HostSlots: 1}
	if a, err := g.Reserve(ctx, hostOnly); err != nil || a.Reservation == nil {
		t.Fatalf("host-only: %+v %v", a, err)
	}
	if uses := claimUses(Request{}); len(uses) != 0 {
		t.Fatalf("empty request has uses: %v", uses)
	}
}
