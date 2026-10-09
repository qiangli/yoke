// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
)

// Every test here drives the REAL mutations — DeputyAdd/DeputyRevoke, Act and
// the cobra commands — over temporary journal and instance stores.

type deputyFixture struct {
	s     *Store
	is    *fleet.InstanceStore
	ste   principal.Ref
	epoch uint64
}

func newDeputyFixture(t *testing.T, opts ...Option) *deputyFixture {
	t.Helper()
	is := fleet.NewInstanceStore(t.TempDir()).WithCap(-1)
	s := newStore(t, append([]Option{WithDeputyResolver(InstanceResolver{Store: is})}, opts...)...)
	ste := agent("steward")
	return &deputyFixture{s: s, is: is, ste: ste, epoch: mustClaim(t, s, ste, at(0))}
}

func (f *deputyFixture) instance(t *testing.T, label string) fleet.Instance {
	t.Helper()
	inst, err := f.is.Open(fleet.Family{Name: "cfg-" + strings.ToLower(label), Display: label, Policy: fleet.PolicySingle, Bindings: []string{"claude:test"}}, fleet.OpenOptions{Label: label})
	if err != nil {
		t.Fatalf("instance %s: %v", label, err)
	}
	return inst
}

// grant resolves handle through the real InstanceResolver, then grants.
func (f *deputyFixture) grant(t *testing.T, handle string, scope DeputyScope, ttl time.Duration, when time.Time) (Deputy, error) {
	t.Helper()
	holder, err := f.s.DeputyResolver().Resolve(handle)
	if err != nil {
		return Deputy{}, err
	}
	return f.s.DeputyAdd(f.ste, f.epoch, holder, handle, scope, ttl, when)
}

func (f *deputyFixture) mustGrantDeputy(t *testing.T, handle string, scope DeputyScope, when time.Time) Deputy {
	t.Helper()
	d, err := f.grant(t, handle, scope, 24*time.Hour, when)
	if err != nil {
		t.Fatalf("grant %s %v: %v", handle, scope, err)
	}
	return d
}

func sprint(n int) ActTarget { return ActTarget{Sprint: n} }

func wantErr[T error](t *testing.T, err error, what string) {
	t.Helper()
	var target T
	if !errors.As(err, &target) {
		t.Fatalf("%s: want %T, got %v", what, target, err)
	}
}

func TestDeputyFourActsInScopeAreRealJournalMutations(t *testing.T) {
	f := newDeputyFixture(t)
	a := f.instance(t, "Ada")
	c1, c2 := f.instance(t, "Cora"), f.instance(t, "Dex")
	dep := f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{332, 331}}, at(time.Minute))
	if dep.Holder.Episode != a.UUID || dep.ScopeLabel != "331,332" || dep.OnBehalfOf.Name != "steward" {
		t.Fatalf("grant snapshot = %+v", dep)
	}
	me := InstanceRef(a.UUID)
	steps := []ActRequest{
		{Act: ActActivate, Target: sprint(331), Owner: c1.UUID},
		{Act: ActFence, Target: sprint(331), Owner: c2.UUID},
		{Act: ActGate, Target: sprint(331), Summary: "merge gate green"},
		{Act: ActJudge, Target: sprint(331), Outcome: OutcomeSuccess},
	}
	for i, req := range steps {
		e, err := f.s.Act(me, f.epoch, req, at(time.Duration(2+i)*time.Minute))
		if err != nil {
			t.Fatalf("%s in scope: %v", req.Act, err)
		}
		if e.Workstream != "sprint-331" || e.Epoch != f.epoch || e.Actor.Episode != a.UUID {
			t.Fatalf("%s entry = %+v", req.Act, e)
		}
		var tagged, onBehalf bool
		for _, ev := range e.Evidence {
			tagged = tagged || (ev.Kind == "act" && ev.Ref == req.Act)
			onBehalf = onBehalf || (ev.Kind == "deputy" && ev.Ref == dep.ID)
		}
		if !tagged || !onBehalf {
			t.Fatalf("%s entry evidence = %+v", req.Act, e.Evidence)
		}
	}
	board, _, err := f.s.Board()
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range board.Workstreams {
		if ws.Name == "sprint-331" && ws.Owner != c2.UUID {
			t.Fatalf("fence did not reassign the conductor: owner %q", ws.Owner)
		}
	}
}

func TestDeputyCrossScopeAndStewardOnlyFail(t *testing.T) {
	f := newDeputyFixture(t)
	a, b := f.instance(t, "Ada"), f.instance(t, "Bo")
	f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{331}}, at(time.Minute))
	me := InstanceRef(a.UUID)
	now := at(2 * time.Minute)

	_, err := f.s.Act(me, f.epoch, ActRequest{Act: ActGate, Target: sprint(333)}, now)
	wantErr[*ErrDeputyOutOfScope](t, err, "gate outside scope")
	_, err = f.s.Act(me, f.epoch, ActRequest{Act: ActGate, Target: ActTarget{Epic: "comms"}}, now)
	wantErr[*ErrDeputyOutOfScope](t, err, "gate on an epic outside scope")
	_, err = f.s.Act(me, f.epoch, ActRequest{Act: ActJudge}, now)
	wantErr[*ErrDeputyStewardOnly](t, err, "act with no target (cross-scope)")
	_, err = f.s.Act(me, f.epoch, ActRequest{Act: "allocate", Target: sprint(331)}, now)
	if err == nil {
		t.Fatal("a fifth act passed")
	}
	// Allocation (grant), release, integration (generic decisions) stay with the steward.
	_, err = f.s.DeputyAdd(me, f.epoch, InstanceRef(b.UUID), "Bo", DeputyScope{Sprints: []int{340}}, time.Hour, now)
	wantErr[*ErrDeputyIsDeputy](t, err, "deputy granting a deputy")
	wantErr[*ErrNotHolder](t, f.s.DeputyRevoke(me, f.epoch, "dep-x", now), "deputy revoking")
	_, err = f.s.Decide(me, f.epoch, "sprint-331", "integrate", "window", nil, now)
	wantErr[*ErrNotHolder](t, err, "deputy integration decision")
	wantErr[*ErrNotHolder](t, f.s.Release(me, f.epoch, "", now), "deputy releasing the seat")
	// The steward is never scope-limited.
	if _, err := f.s.Act(f.ste, f.epoch, ActRequest{Act: ActGate, Target: sprint(333)}, now); err != nil {
		t.Fatalf("steward gate: %v", err)
	}
	// A stranger with no grant is just not the holder.
	_, err = f.s.Act(InstanceRef(b.UUID), f.epoch, ActRequest{Act: ActGate, Target: sprint(331)}, now)
	wantErr[*ErrNotHolder](t, err, "ungranted instance")
}

func TestDeputyStaleOrZeroEpochFencesNeverFallsThrough(t *testing.T) {
	f := newDeputyFixture(t)
	a := f.instance(t, "Ada")
	f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{331}}, at(time.Minute))
	me := InstanceRef(a.UUID)
	req := ActRequest{Act: ActGate, Target: sprint(331)}

	_, err := f.s.Act(me, 0, req, at(2*time.Minute))
	wantErr[*ErrNoEpoch](t, err, "zero epoch")
	_, err = f.s.Act(me, f.epoch+7, req, at(2*time.Minute))
	wantErr[*ErrFenced](t, err, "wrong epoch")

	// Steward takeover: the old epoch is stale, and presenting the NEW one does
	// not revive a grant made under the old one.
	newEpoch := mustTakeover(t, f.s, agent("steward-2"), at(3*time.Minute))
	_, err = f.s.Act(me, f.epoch, req, at(4*time.Minute))
	wantErr[*ErrFenced](t, err, "stale epoch after takeover")
	_, err = f.s.Act(me, newEpoch, req, at(4*time.Minute))
	wantErr[*ErrDeputyFenced](t, err, "grant from the old steward epoch")
}

func TestDeputyExpiryAndRevokeFence(t *testing.T) {
	f := newDeputyFixture(t)
	a, b := f.instance(t, "Ada"), f.instance(t, "Bo")
	if _, err := f.grant(t, a.UUID, DeputyScope{Sprints: []int{331}}, time.Hour, at(0)); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.Act(InstanceRef(a.UUID), f.epoch, ActRequest{Act: ActGate, Target: sprint(331)}, at(2*time.Hour))
	wantErr[*ErrDeputyExpired](t, err, "expired")

	db := f.mustGrantDeputy(t, b.UUID, DeputyScope{Sprints: []int{340}}, at(3*time.Hour))
	if err := f.s.DeputyRevoke(f.ste, f.epoch, db.ID, at(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Act(InstanceRef(b.UUID), f.epoch, ActRequest{Act: ActJudge, Target: sprint(340)}, at(5*time.Hour))
	wantErr[*ErrDeputyRevoked](t, err, "revoked")
}

func TestDeputyTwoGrantsSameUUIDSelectScopeAndForbidConduct(t *testing.T) {
	f := newDeputyFixture(t)
	a := f.instance(t, "Ada")
	f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{331}}, at(time.Minute))
	f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{340}}, at(2*time.Minute))
	me := InstanceRef(a.UUID)
	now := at(3 * time.Minute)
	for _, n := range []int{331, 340} {
		if _, err := f.s.Act(me, f.epoch, ActRequest{Act: ActGate, Target: sprint(n)}, now); err != nil {
			t.Fatalf("gate %d with two grants: %v", n, err)
		}
		wantErr[*ErrDeputySelfConduct](t, f.s.MayConduct(me, sprint(n), now), "conduct own scope")
		_, err := f.s.Act(f.ste, f.epoch, ActRequest{Act: ActActivate, Target: sprint(n), Owner: a.UUID}, now)
		wantErr[*ErrDeputySelfConduct](t, err, "installing the deputy as conductor")
	}
	if err := f.s.MayConduct(me, sprint(999), now); err != nil {
		t.Fatalf("conducting outside its scope is allowed: %v", err)
	}
}

func TestDeputyEpicSprintOverlap(t *testing.T) {
	// No membership lookup: mixed scopes fail closed.
	f := newDeputyFixture(t)
	a, b := f.instance(t, "Ada"), f.instance(t, "Bo")
	f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{331}}, at(time.Minute))
	_, err := f.grant(t, b.UUID, DeputyScope{Epic: "comms"}, time.Hour, at(2*time.Minute))
	wantErr[*ErrMembershipUnknown](t, err, "mixed scope without membership")

	// With membership: an epic containing a deputized sprint overlaps.
	member := EpicMembershipFunc(func(n int) (string, error) {
		if n == 331 || n == 332 {
			return "comms", nil
		}
		return "", nil
	})
	g := newDeputyFixture(t, WithEpicMembership(member))
	a, b = g.instance(t, "Ada"), g.instance(t, "Bo")
	g.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{331}}, at(time.Minute))
	_, err = g.grant(t, b.UUID, DeputyScope{Epic: "comms"}, time.Hour, at(2*time.Minute))
	wantErr[*ErrDeputyOverlap](t, err, "epic over a deputized sprint")
	_, err = g.grant(t, b.UUID, DeputyScope{Sprints: []int{331, 400}}, time.Hour, at(2*time.Minute))
	wantErr[*ErrDeputyOverlap](t, err, "overlapping sprint lists")
	c := g.instance(t, "Cy")
	g.mustGrantDeputy(t, c.UUID, DeputyScope{Epic: "infra"}, at(3*time.Minute))
	// An epic deputy acts on member sprints, and only them.
	if _, err := g.s.Act(InstanceRef(a.UUID), g.epoch, ActRequest{Act: ActGate, Target: sprint(331)}, at(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, err = g.s.Act(InstanceRef(c.UUID), g.epoch, ActRequest{Act: ActGate, Target: sprint(332)}, at(4*time.Minute))
	wantErr[*ErrDeputyOutOfScope](t, err, "epic deputy on a sprint of another epic")
}

// An --owner that is a handle must resolve to the instance UUID at act time,
// or a deputy can install ITSELF as conductor under its label and then judge
// work it conducted: ownerRef("Ada") is name-only and isHolder matches UUIDs
// only, so the independence checks never fire on the raw label.
func TestDeputyActivateWithOwnHandleAsOwnerIsRefused(t *testing.T) {
	f := newDeputyFixture(t)
	a := f.instance(t, "Ada")
	c := f.instance(t, "Cora")
	f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{331}}, at(time.Minute))
	me := InstanceRef(a.UUID)
	// The deputy's own handle resolves to its own UUID: installing itself is
	// conducting its own scope.
	_, err := f.s.Act(me, f.epoch, ActRequest{Act: ActActivate, Target: sprint(331), Owner: "Ada"}, at(2*time.Minute))
	wantErr[*ErrDeputySelfConduct](t, err, "deputy activating its own sprint with its own handle")
	// A handle that names no live instance is refused, not stored raw.
	_, err = f.s.Act(me, f.epoch, ActRequest{Act: ActActivate, Target: sprint(331), Owner: "nobody"}, at(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("unresolvable owner must be refused naming it, got %v", err)
	}
	// Another instance's handle resolves and is stored as its UUID, so later
	// independence checks compare UUIDs.
	e, err := f.s.Act(me, f.epoch, ActRequest{Act: ActActivate, Target: sprint(331), Owner: "Cora"}, at(3*time.Minute))
	if err != nil {
		t.Fatalf("activate with a resolvable handle: %v", err)
	}
	if e.Update == nil || e.Update.Owner != c.UUID {
		t.Fatalf("owner must be stored as the instance UUID, got %+v", e.Update)
	}
	// Judging a sprint another instance conducts is still allowed.
	if _, err = f.s.Act(me, f.epoch, ActRequest{Act: ActJudge, Target: sprint(331)}, at(4*time.Minute)); err != nil {
		t.Fatalf("judging a sprint conducted by someone else: %v", err)
	}
	// The steward installing the deputy BY HANDLE stores the UUID too, so a
	// later self-judge is still caught by UUID comparison.
	g := newDeputyFixture(t)
	ga := g.instance(t, "Ada")
	if _, err := g.s.Act(g.ste, g.epoch, ActRequest{Act: ActActivate, Target: sprint(331), Owner: "Ada"}, at(time.Minute)); err != nil {
		t.Fatalf("steward activate by handle: %v", err)
	}
	g.mustGrantDeputy(t, ga.UUID, DeputyScope{Sprints: []int{331}}, at(2*time.Minute))
	_, err = g.s.Act(InstanceRef(ga.UUID), g.epoch, ActRequest{Act: ActJudge, Target: sprint(331)}, at(3*time.Minute))
	wantErr[*ErrDeputySelfJudge](t, err, "judging a sprint conducted under a resolved handle")
}

func TestDeputySelfJudgeFails(t *testing.T) {
	f := newDeputyFixture(t)
	a, c := f.instance(t, "Ada"), f.instance(t, "Cora")
	// Ada conducted sprint 331 before being deputized over it.
	if _, err := f.s.Act(f.ste, f.epoch, ActRequest{Act: ActActivate, Target: sprint(331), Owner: a.UUID}, at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{331, 332}}, at(2*time.Minute))
	me := InstanceRef(a.UUID)
	_, err := f.s.Act(me, f.epoch, ActRequest{Act: ActJudge, Target: sprint(331)}, at(3*time.Minute))
	wantErr[*ErrDeputySelfJudge](t, err, "judging a sprint it conducted")

	// Ada authored a claim in sprint 332 (conducted by Cora) and may not judge it.
	if _, err := f.s.Act(f.ste, f.epoch, ActRequest{Act: ActActivate, Target: sprint(332), Owner: c.UUID}, at(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	authored, err := f.s.Act(me, f.epoch, ActRequest{Act: ActGate, Target: sprint(332), Summary: "converged"}, at(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Act(me, f.epoch, ActRequest{Act: ActJudge, Target: sprint(332), TargetSeq: authored.Seq}, at(6*time.Minute))
	wantErr[*ErrDeputySelfJudge](t, err, "judging its own claim")
	// A claim it did not author, in a sprint it did not conduct, it may judge.
	theirs := mustRecord(t, f.s, Entry{Actor: f.ste, Kind: KindEffect, Workstream: "sprint-332", Summary: "work"}, f.epoch, at(7*time.Minute))
	if _, err := f.s.Act(me, f.epoch, ActRequest{Act: ActJudge, Target: sprint(332), TargetSeq: theirs.Seq}, at(8*time.Minute)); err != nil {
		t.Fatalf("independent judge: %v", err)
	}
}

func TestDeputyUnknownRetiredAndReusedLabelGetNoGrant(t *testing.T) {
	f := newDeputyFixture(t)
	_, err := f.grant(t, "0b9a3f0e-1c2d-4e5f-8a9b-0c1d2e3f4a5b", DeputyScope{Sprints: []int{331}}, time.Hour, at(0))
	if !errors.Is(err, fleet.ErrInstanceUnknown) {
		t.Fatalf("unknown UUID: %v", err)
	}
	_, err = f.grant(t, "nobody", DeputyScope{Sprints: []int{331}}, time.Hour, at(0))
	if err == nil {
		t.Fatal("unknown handle granted")
	}
	_, err = f.s.DeputyAdd(f.ste, f.epoch, principal.Ref{Name: "Esme"}, "Esme", DeputyScope{Sprints: []int{331}}, time.Hour, at(0))
	wantErr[*ErrDeputyScope](t, err, "name-only holder")

	old := f.instance(t, "Esme")
	f.mustGrantDeputy(t, "Esme", DeputyScope{Sprints: []int{331}}, at(time.Minute))
	if _, err := f.is.Retire(old.UUID, func(fleet.Instance) ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	_, err = f.grant(t, old.UUID, DeputyScope{Sprints: []int{350}}, time.Hour, at(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired instance: %v", err)
	}
	reused := f.instance(t, "Esme")
	if reused.UUID == old.UUID {
		t.Fatal("label reuse kept the UUID")
	}
	_, err = f.s.Act(InstanceRef(reused.UUID), f.epoch, ActRequest{Act: ActGate, Target: sprint(331)}, at(3*time.Minute))
	wantErr[*ErrNotHolder](t, err, "reused label inheriting the old grant")
	// Nor does a principal that only carries the label as a name.
	_, err = f.s.Act(principal.Ref{Kind: principal.KindAgent, Name: "Esme"}, f.epoch, ActRequest{Act: ActGate, Target: sprint(331)}, at(3*time.Minute))
	wantErr[*ErrNotHolder](t, err, "label as identity")
}

func TestDeputyCorruptJournalRejected(t *testing.T) {
	f := newDeputyFixture(t)
	a := f.instance(t, "Ada")
	f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{331}}, at(time.Minute))
	appendRaw(t, f.s, "{not json\n")
	_, err := f.s.Act(InstanceRef(a.UUID), f.epoch, ActRequest{Act: ActGate, Target: sprint(331)}, at(2*time.Minute))
	wantErr[*ErrCorruptTail](t, err, "act on corrupt journal")
	wantErr[*ErrCorruptTail](t, f.s.MayConduct(InstanceRef(a.UUID), sprint(331), at(2*time.Minute)), "conduct lookup")
	_, err = f.s.DeputyOccupancies(at(2 * time.Minute))
	wantErr[*ErrCorruptTail](t, err, "occupancy lookup")
}

func TestDeputyRoleAddressSurvivesVacancyAndHandoff(t *testing.T) {
	f := newDeputyFixture(t)
	a, b := f.instance(t, "Ada"), f.instance(t, "Bo")
	da := f.mustGrantDeputy(t, a.UUID, DeputyScope{Sprints: []int{331, 332}}, at(time.Minute))
	occ, ok, err := f.s.DeputyOccupant("deputy:331,332", at(2*time.Minute))
	if err != nil || !ok || occ.Holder != a.UUID || occ.Topic != "deputy.331,332" {
		t.Fatalf("occupant = %+v %v %v", occ, ok, err)
	}
	if err := f.s.DeputyRevoke(f.ste, f.epoch, da.ID, at(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	occ, ok, _ = f.s.DeputyOccupant("deputy:331,332", at(4*time.Minute))
	if !ok || !occ.Vacant || occ.Holder != "" || occ.Address != "deputy:331,332" {
		t.Fatalf("vacant address = %+v %v", occ, ok)
	}
	f.mustGrantDeputy(t, b.UUID, DeputyScope{Sprints: []int{332, 331}}, at(5*time.Minute))
	occ, _, _ = f.s.DeputyOccupant("deputy:331,332", at(6*time.Minute))
	if occ.Vacant || occ.Holder != b.UUID {
		t.Fatalf("handoff holder = %+v", occ)
	}
	all, _ := f.s.DeputyOccupancies(at(6 * time.Minute))
	if len(all) != 1 {
		t.Fatalf("one durable address expected, got %+v", all)
	}
}

// cliAs runs the real cobra tree as a given principal (cli() pins "tester").
func cliAs(t *testing.T, dir, urn string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("BASHY_PRINCIPAL", urn)
	t.Setenv("BASHY_EPISODE", "")
	t.Setenv("BASHY_HOST_ID", "cli-test-machine")
	t.Setenv(EpochEnv, os.Getenv(EpochEnv))
	cmd := NewStewardCmd(WithRegistryRoot(cliRegistry(dir)))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"--dir", dir}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestDeputyCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(fleet.InstanceDirEnv, t.TempDir())
	seedSeat(t, dir)
	is := fleet.NewInstanceStore("").WithCap(-1)
	inst, err := is.Open(fleet.Family{Name: "cfg", Display: "Ada", Policy: fleet.PolicySingle, Bindings: []string{"claude:test"}}, fleet.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cli(t, dir, "deputy", "add", "0b9a3f0e-1c2d-4e5f-8a9b-0c1d2e3f4a5b", "--sprints", "331"); err == nil {
		t.Fatalf("unknown UUID granted through the CLI:\n%s", out)
	}
	out := mustCLI(t, dir, "deputy", "add", "Ada", "--sprints", "331,332", "--ttl", "2h")
	if !strings.Contains(out, inst.UUID) || !strings.Contains(out, "deputy:331,332") {
		t.Fatalf("deputy add output:\n%s", out)
	}
	if out := mustCLI(t, dir, "deputy", "occupant", "deputy:331,332"); !strings.Contains(out, inst.UUID) {
		t.Fatalf("occupant output:\n%s", out)
	}
	deputy := principal.InstanceURN(inst.UUID)
	if out, err := cliAs(t, dir, deputy, "act", "gate", "--sprint", "331", "-m", "green"); err != nil {
		t.Fatalf("deputy gate via CLI: %v\n%s", err, out)
	}
	if out, err := cliAs(t, dir, deputy, "act", "gate", "--sprint", "400"); err == nil {
		t.Fatalf("deputy out-of-scope gate via CLI passed:\n%s", out)
	}
	if out, err := cliAs(t, dir, deputy, "act", "gate", "--sprint", "331", "--epoch", "99"); err == nil {
		t.Fatalf("deputy act with a stale epoch passed via CLI:\n%s", out)
	}
	if out, err := cliAs(t, dir, deputy, "deputy", "add", "Ada", "--sprints", "500"); err == nil {
		t.Fatalf("deputy granted a deputy via CLI:\n%s", out)
	}
	if out := mustCLI(t, dir, "deputy", "list"); !strings.Contains(out, "active") {
		t.Fatalf("deputy list:\n%s", out)
	}
	t.Setenv(EpochEnv, "")
	if out, err := cliAs(t, dir, deputy, "act", "gate", "--sprint", "331"); err == nil {
		t.Fatalf("deputy act with no epoch passed via CLI:\n%s", out)
	}
}

// The production steward command is opened with no epic membership wired: the
// sprint board lookup it would need is not small enough to inject here (it
// would execute the board per scope check), so an --epic grant is refused
// with directions instead of failing closed mid-grant. Store-level epic
// grants with injected membership keep working (TestDeputyEpicSprintOverlap).
func TestDeputyCLIEpicGrantRefusedWithoutMembership(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)
	out, err := cli(t, dir, "deputy", "add", "Ada", "--epic", "comms")
	if err == nil {
		t.Fatalf("epic grant without membership passed:\n%s", out)
	}
	if !strings.Contains(out+err.Error(), "--sprints") {
		t.Fatalf("the refusal must point at --sprints: out=%q err=%v", out, err)
	}
}

func TestGlossaryCommand(t *testing.T) {
	dir := t.TempDir()
	out := mustCLI(t, dir, "glossary")
	for _, w := range []string{"steward", "deputy", "conductor", "worker", "deputy:<scope>", "conductor:<sprint>", `"manager"`} {
		if !strings.Contains(out, w) {
			t.Fatalf("glossary missing %q:\n%s", w, out)
		}
	}
	if _, err := cli(t, dir, "glossary", "director"); err == nil {
		t.Fatal("glossary invented a fourth role")
	}
}
