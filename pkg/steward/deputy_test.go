// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

func deputyHolder(name, uuid string) principal.Ref {
	return principal.Ref{Kind: principal.KindAgent, Name: name, Episode: uuid, Host: "test-host"}
}

func mustDeputy(t *testing.T, s *Store, actor principal.Ref, epoch uint64, holder principal.Ref, handle string, scope DeputyScope, ttl time.Duration, when time.Time) Deputy {
	t.Helper()
	d, err := s.DeputyAdd(actor, epoch, holder, handle, scope, ttl, when)
	if err != nil {
		t.Fatalf("DeputyAdd: %v", err)
	}
	return d
}

// In-scope authority passes
func TestDeputyInScopeAuthorityPasses(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep := mustClaim(t, s, ste, at(0))
	holder := deputyHolder("deputyA", "uuid-a")
	dep := mustDeputy(t, s, ste, ep, holder, "deputyA", DeputyScope{Sprints: []int{331, 332}}, 24*time.Hour, at(time.Minute))

	if err := s.CheckDeputyAuthority(holder, DeputyActionFence, 331, "", at(2*time.Minute)); err != nil {
		t.Fatalf("fence in scope should pass, got %v", err)
	}
	if err := s.CheckDeputyAuthority(holder, DeputyActionJudge, 332, "", at(2*time.Minute)); err != nil {
		t.Fatalf("judge in scope should pass, got %v", err)
	}
	if err := s.CheckDeputyAuthority(holder, DeputyActionGate, 331, "", at(2*time.Minute)); err != nil {
		t.Fatalf("gate in scope should pass, got %v", err)
	}
	if err := s.CheckDeputyAuthority(holder, DeputyActionActivate, 332, "", at(2*time.Minute)); err != nil {
		t.Fatalf("activate in scope should pass, got %v", err)
	}
	// epic scope
	holder2 := deputyHolder("deputyB", "uuid-b")
	dep2 := mustDeputy(t, s, ste, ep, holder2, "deputyB", DeputyScope{Epic: "agent-comms"}, 24*time.Hour, at(3*time.Minute))
	_ = dep
	_ = dep2
	if err := s.CheckDeputyAuthority(holder2, DeputyActionJudge, 0, "agent-comms", at(4*time.Minute)); err != nil {
		t.Fatalf("epic in scope should pass, got %v", err)
	}
}

// Out-of-scope fails clearly
func TestDeputyOutOfScopeFails(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep := mustClaim(t, s, ste, at(0))
	holder := deputyHolder("deputyA", "uuid-a")
	mustDeputy(t, s, ste, ep, holder, "deputyA", DeputyScope{Sprints: []int{331, 332}}, 24*time.Hour, at(time.Minute))

	if err := s.CheckDeputyAuthority(holder, DeputyActionJudge, 999, "", at(2*time.Minute)); err == nil {
		t.Fatal("out-of-scope judge should fail")
	} else {
		if _, ok := err.(*ErrDeputyOutOfScope); !ok {
			t.Fatalf("want ErrDeputyOutOfScope, got %T: %v", err, err)
		}
	}
	// cross-scope allocation (not an allowed deputy action)
	if err := s.CheckDeputyAuthority(holder, "allocate", 331, "", at(2*time.Minute)); err == nil {
		t.Fatal("cross-scope allocate should fail")
	}
}

// Overlapping scopes fail
func TestDeputyOverlappingScopesFail(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep := mustClaim(t, s, ste, at(0))
	h1 := deputyHolder("d1", "uuid-1")
	mustDeputy(t, s, ste, ep, h1, "d1", DeputyScope{Sprints: []int{331, 332}}, 24*time.Hour, at(time.Minute))
	h2 := deputyHolder("d2", "uuid-2")
	_, err := s.DeputyAdd(ste, ep, h2, "d2", DeputyScope{Sprints: []int{332, 333}}, 24*time.Hour, at(2*time.Minute))
	if err == nil {
		t.Fatal("overlapping sprints should be refused")
	}
	if _, ok := err.(*ErrDeputyOverlap); !ok {
		t.Fatalf("want ErrDeputyOverlap, got %T: %v", err, err)
	}
	// epic overlap
	h3 := deputyHolder("d3", "uuid-3")
	_, err = s.DeputyAdd(ste, ep, h3, "d3", DeputyScope{Epic: "X"}, 24*time.Hour, at(3*time.Minute))
	if err != nil {
		t.Fatalf("epic X first grant: %v", err)
	}
	h4 := deputyHolder("d4", "uuid-4")
	_, err = s.DeputyAdd(ste, ep, h4, "d4", DeputyScope{Epic: "X"}, 24*time.Hour, at(4*time.Minute))
	if err == nil {
		t.Fatal("overlapping epic should be refused")
	}
}

// Self-judging/conducting fails
func TestDeputySelfConductFails(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep := mustClaim(t, s, ste, at(0))
	holder := deputyHolder("deputyA", "uuid-a")
	mustDeputy(t, s, ste, ep, holder, "deputyA", DeputyScope{Sprints: []int{331}}, 24*time.Hour, at(time.Minute))
	if err := s.CheckDeputyAuthority(holder, DeputyActionConduct, 331, "", at(2*time.Minute)); err == nil {
		t.Fatal("deputy conducting its own scope should fail")
	} else {
		if _, ok := err.(*ErrDeputySelfConduct); !ok {
			t.Fatalf("want ErrDeputySelfConduct, got %T: %v", err, err)
		}
	}
	// conducting outside scope is allowed (not self)
	if err := s.CheckDeputyAuthority(holder, DeputyActionConduct, 999, "", at(2*time.Minute)); err != nil {
		t.Fatalf("conducting outside own scope should pass, got %v", err)
	}
}

// Revoke fences
func TestDeputyRevokeFences(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep := mustClaim(t, s, ste, at(0))
	holder := deputyHolder("deputyA", "uuid-a")
	dep := mustDeputy(t, s, ste, ep, holder, "deputyA", DeputyScope{Sprints: []int{331}}, 24*time.Hour, at(time.Minute))
	if err := s.DeputyRevoke(ste, ep, dep.ID, at(2*time.Minute)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := s.CheckDeputyAuthority(holder, DeputyActionJudge, 331, "", at(3*time.Minute)); err == nil {
		t.Fatal("revoked deputy should be fenced")
	}
}

// Expiry fences
func TestDeputyExpiryFences(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep := mustClaim(t, s, ste, at(0))
	holder := deputyHolder("deputyA", "uuid-a")
	mustDeputy(t, s, ste, ep, holder, "deputyA", DeputyScope{Sprints: []int{331}}, time.Hour, at(time.Minute))
	// after 2h should be expired
	if err := s.CheckDeputyAuthority(holder, DeputyActionJudge, 331, "", at(2*time.Hour)); err == nil {
		t.Fatal("expired deputy should be fenced")
	}
}

// Changed steward epoch fences
func TestDeputyEpochFencing(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep1 := mustClaim(t, s, ste, at(0))
	holder := deputyHolder("deputyA", "uuid-a")
	mustDeputy(t, s, ste, ep1, holder, "deputyA", DeputyScope{Sprints: []int{331}}, 24*time.Hour, at(time.Minute))
	// steward takeover bumps epoch
	ep2 := mustTakeover(t, s, agent("steward2"), at(5*time.Minute))
	if ep2 == ep1 {
		t.Fatal("epoch should bump")
	}
	if err := s.CheckDeputyAuthority(holder, DeputyActionJudge, 331, "", at(6*time.Minute)); err == nil {
		t.Fatal("deputy from old epoch should be fenced after steward takeover")
	} else {
		if _, ok := err.(*ErrDeputyFenced); !ok {
			t.Fatalf("want ErrDeputyFenced, got %T: %v", err, err)
		}
	}
}

// Reused handle receives no old grant
func TestDeputyReusedHandleReceivesNoOldGrant(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep := mustClaim(t, s, ste, at(0))
	// resolver that returns different UUID for same handle on successive calls
	call := 0
	resolver := &countingResolver{handle: "Esme-2", uuids: []string{"uuid-old", "uuid-new"}, calls: &call}
	storeWithResolver := func() *Store {
		// reopen same underlying store dir with resolver
		// For test isolation we use same store but inject resolver via field
		s.deputyResolver = resolver
		return s
	}
	storeWithResolver()
	hOld := deputyHolder("Esme-2", "uuid-old")
	// grant to old UUID
	mustDeputy(t, s, ste, ep, hOld, "Esme-2", DeputyScope{Sprints: []int{331}}, 24*time.Hour, at(time.Minute))
	// revoke old deputy to allow same scope with new holder (non-overlap check would otherwise block)
	deps, _ := s.DeputyList(at(2 * time.Minute))
	s.DeputyRevoke(ste, ep, deps[0].ID, at(2*time.Minute))
	// Now grant to same handle but resolved to new UUID
	hNew := deputyHolder("Esme-2", "uuid-new")
	depNew := mustDeputy(t, s, ste, ep, hNew, "Esme-2", DeputyScope{Sprints: []int{331}}, 24*time.Hour, at(3*time.Minute))
	// Old holder should not have authority via new deputy's scope (different UUID)
	if err := s.CheckDeputyAuthority(hOld, DeputyActionJudge, 331, "", at(4*time.Minute)); err == nil {
		t.Fatal("old handle holder (uuid-old) should not gain authority from new grant to uuid-new")
	}
	// New holder should have authority
	if err := s.CheckDeputyAuthority(hNew, DeputyActionJudge, 331, "", at(4*time.Minute)); err != nil {
		t.Fatalf("new holder should have authority, got %v", err)
	}
	_ = depNew
}

// No deputies of deputies
func TestDeputyCannotGrantDeputy(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep := mustClaim(t, s, ste, at(0))
	holder := deputyHolder("deputyA", "uuid-a")
	mustDeputy(t, s, ste, ep, holder, "deputyA", DeputyScope{Sprints: []int{331}}, 24*time.Hour, at(time.Minute))
	// deputy trying to grant another deputy should fail
	other := deputyHolder("deputyB", "uuid-b")
	_, err := s.DeputyAdd(holder, ep, other, "deputyB", DeputyScope{Sprints: []int{332}}, 24*time.Hour, at(2*time.Minute))
	if err == nil {
		t.Fatal("deputy should not be able to grant deputies")
	}
	if _, ok := err.(*ErrDeputyIsDeputy); !ok {
		t.Fatalf("want ErrDeputyIsDeputy, got %T: %v", err, err)
	}
}

// Durable role mail survives authorized holder handoff
func TestDeputyMailSurvivesHandoff(t *testing.T) {
	s := newStore(t)
	ste := agent("steward")
	ep := mustClaim(t, s, ste, at(0))
	h1 := deputyHolder("d", "uuid-1")
	dep1 := mustDeputy(t, s, ste, ep, h1, "d", DeputyScope{Sprints: []int{331, 332}}, 24*time.Hour, at(time.Minute))
	topic := DeputyTopicForScope(dep1.Scope)
	label := DeputyLabelForScope(dep1.Scope)
	if topic != "deputy.331,332" {
		t.Fatalf("topic %q", topic)
	}
	if label != "deputy:331,332" {
		t.Fatalf("label %q", label)
	}
	// handoff: revoke old, grant new holder with same scope
	s.DeputyRevoke(ste, ep, dep1.ID, at(2*time.Minute))
	h2 := deputyHolder("d2", "uuid-2")
	dep2 := mustDeputy(t, s, ste, ep, h2, "d2", DeputyScope{Sprints: []int{331, 332}}, 24*time.Hour, at(3*time.Minute))
	if DeputyTopicForScope(dep2.Scope) != topic {
		t.Fatalf("topic should survive handoff")
	}
	if DeputyLabelForScope(dep2.Scope) != label {
		t.Fatalf("label should survive handoff")
	}
	// old holder fenced via revoke
	if err := s.CheckDeputyAuthority(h1, DeputyActionJudge, 331, "", at(4*time.Minute)); err == nil {
		t.Fatal("old holder should not have authority after revoke")
	}
	// new holder has authority and same durable address
	if err := s.CheckDeputyAuthority(h2, DeputyActionJudge, 331, "", at(4*time.Minute)); err != nil {
		t.Fatalf("new holder should have authority, got %v", err)
	}
}

type countingResolver struct {
	handle string
	uuids  []string
	calls  *int
}

func (r *countingResolver) Resolve(handle string) (principal.Ref, error) {
	idx := *r.calls
	*r.calls++
	if idx >= len(r.uuids) {
		idx = len(r.uuids) - 1
	}
	return principal.Ref{Kind: principal.KindAgent, Name: r.handle, Episode: r.uuids[idx], Host: "test-host"}, nil
}
