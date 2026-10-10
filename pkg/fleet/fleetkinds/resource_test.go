// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package fleetkinds

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/policy/coord"
)

// saveResourceKind writes a user kind and picks it up, the way a fresh
// process does at load: Sync re-reads the ambient catalog.
func saveResourceKind(t *testing.T, rec fleet.ResourceKind) {
	t.Helper()
	if rec.Match == "" {
		rec.Match = "member"
	}
	if err := fleet.New().SaveResourceKind(rec); err != nil {
		t.Fatal(err)
	}
	Sync()
}

func acquire(t *testing.T, ref, episode, holder string) coord.Grant {
	t.Helper()
	g, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref: coord.ParseRef(ref), Holder: asHolder(t, episode, holder),
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A resource's claim semantics come from its declared kind: a path-kind
// resource with members /w/app conflicts with a path claim beneath it,
// exactly as a path claim over /w/app would.
func TestResourceClaimConflictsViaMembersUnderItsKind(t *testing.T) {
	fleetRoot(t)
	if err := fleet.New().SaveResource(fleet.Resource{Name: "appdir", Kind: "path", Members: []string{"/w/app"}}); err != nil {
		t.Fatal(err)
	}

	holderA := asHolder(t, "ep-res-a", "test-res-a")
	g, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "resource", Name: "appdir"}, Holder: holderA})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Claim.Members) != 1 || g.Claim.Members[0] != "/w/app" {
		t.Fatalf("members = %v, want [/w/app]", g.Claim.Members)
	}
	if g.Claim.Kind != "path" {
		t.Fatalf("stored kind = %q, want the record's declared kind", g.Claim.Kind)
	}

	asHolder(t, "ep-res-b", "test-res-b")
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "path", Name: "/w/app/x"}, Holder: coord.Self()}); err == nil {
		t.Fatal("path claim under a resource's members admitted")
	} else {
		asConflict(t, err)
	}
	// A bare name that names a registered resource claims as it.
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.ParseRef("appdir"), Holder: coord.Self()}); err == nil {
		t.Fatal("bare name of a registered resource admitted over its claim")
	} else {
		asConflict(t, err)
	}
	if err := coord.Guard(context.Background(), coord.Self(), coord.Use{Kind: "resource", Name: "appdir"}); err == nil {
		t.Fatal("guard admitted over a live resource claim")
	} else {
		asConflict(t, err)
	}

	// The reverse order conflicts too: the path claim first, the resource over it.
	if err := coord.ReleaseRef(context.Background(), coord.Ref{Kind: "resource", Name: "appdir"}, holderA, 0); err != nil {
		t.Fatal(err)
	}
	holderB := asHolder(t, "ep-res-b2", "test-res-b")
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "path", Name: "/w/app/x"}, Holder: holderB}); err != nil {
		t.Fatal(err)
	}
	asHolder(t, "ep-res-a2", "test-res-a")
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "resource", Name: "appdir"}, Holder: coord.Self()}); err == nil {
		t.Fatal("resource claim over a live path claim admitted")
	} else {
		asConflict(t, err)
	}
}

// A user resourcekind with member matching works: a resource held under it
// conflicts with a direct claim of the kind sharing a member.
func TestUserResourceKindMemberMatch(t *testing.T) {
	fleetRoot(t)
	saveResourceKind(t, fleet.ResourceKind{Name: "seat-s2", Match: "member", Domain: "seat"})
	k, ok := coord.LookupKind("seat-s2")
	if !ok || k.Match != coord.MatchMember || k.Domain != "seat" {
		t.Fatalf("user kind = %+v ok=%v", k, ok)
	}
	if err := fleet.New().SaveResource(fleet.Resource{Name: "s1-s2", Kind: "seat-s2", Members: []string{"gpu0"}}); err != nil {
		t.Fatal(err)
	}

	holderA := asHolder(t, "ep-seat-a", "test-seat-a")
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "resource", Name: "s1-s2"}, Holder: holderA}); err != nil {
		t.Fatal(err)
	}
	asHolder(t, "ep-seat-b", "test-seat-b")
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "seat-s2", Name: "gpu0"}, Holder: coord.Self()}); err == nil {
		t.Fatal("kind claim sharing a resource's member admitted")
	} else {
		asConflict(t, err)
	}
}

// A resolve hook naming nothing registered is refused, never run from PATH.
func TestResolveHookRefusesUnregisteredCommand(t *testing.T) {
	fleetRoot(t)
	saveResourceKind(t, fleet.ResourceKind{Name: "hooked-s2", Match: "member", Resolve: "no-such-hook-command-s2"})
	if _, err := runResolve("no-such-hook-command-s2", "x"); err == nil || !strings.Contains(err.Error(), "not a registered command") {
		t.Fatalf("err = %v, want the unregistered-command refusal", err)
	}
	asHolder(t, "ep-hook-a", "test-hook-a")
	_, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "hooked-s2", Name: "x"}, Holder: coord.Self()})
	if err == nil || !strings.Contains(err.Error(), "not a registered command") {
		t.Fatalf("err = %v, want the unregistered-command refusal", err)
	}
}

// The probe contract, pinned: exit 0 free, 1 busy, anything else unknown —
// and a probe naming nothing registered is refused like a resolve hook.
func TestProbeContract(t *testing.T) {
	for code, want := range map[int]ProbeState{0: ProbeFree, 1: ProbeBusy, 2: ProbeUnknown, 3: ProbeUnknown, -1: ProbeUnknown} {
		if got := probeStateFromExit(code); got != want {
			t.Errorf("exit %d = %q, want %q", code, got, want)
		}
	}
	fleetRoot(t)
	if _, err := runProbe("no-such-probe-command-s2", "x"); err == nil || !strings.Contains(err.Error(), "not a registered command") {
		t.Fatalf("err = %v, want the unregistered-command refusal", err)
	}
}

// TestHelperHook is the hook a resolve/probe test command runs: the test
// binary re-executed with -test.run=TestHelperHook. The parent sets
// GO_WANT_HELPER_PROCESS=1 plus HOOK_BEHAVIOR; without the marker it is an
// ordinary passing test.
func TestHelperHook(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("HOOK_BEHAVIOR") {
	case "resolve":
		// Exit directly: returning would let the test harness print its
		// own PASS line to the same stdout the parent parses for members.
		os.Stdout.WriteString("mem-a\nmem-b\n")
		os.Exit(0)
	case "probe-busy":
		os.Exit(1)
	case "probe-boom":
		os.Exit(3)
	}
}

func hookCommand(t *testing.T, name string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.New().SaveCommand(fleet.Command{Name: name, Exec: []string{bin, "-test.run=TestHelperHook"}}); err != nil {
		t.Fatal(err)
	}
}

// Resolve and probe hooks execute registered commands end to end: members
// come from the hook's output lines, and probe exit codes map to free/busy/
// unknown.
func TestHookResolveAndProbeExecute(t *testing.T) {
	fleetRoot(t)
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	hookCommand(t, "s2-resolve")
	hookCommand(t, "s2-probe")
	saveResourceKind(t, fleet.ResourceKind{Name: "rz-s2", Match: "member", Domain: "rz", Resolve: "s2-resolve"})
	saveResourceKind(t, fleet.ResourceKind{Name: "pz-s2", Match: "member", Domain: "pz", Probe: "s2-probe"})

	t.Setenv("HOOK_BEHAVIOR", "resolve")
	members, err := runResolve("s2-resolve", "thing")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members[0] != "mem-a" || members[1] != "mem-b" {
		t.Fatalf("members = %v", members)
	}
	// A claim of the hooked kind resolves through the hook, and two names
	// resolving to the same members conflict under member matching.
	holderA := asHolder(t, "ep-rz-a", "test-rz-a")
	g, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "rz-s2", Name: "thing"}, Holder: holderA})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Claim.Members) != 2 {
		t.Fatalf("members = %v", g.Claim.Members)
	}
	asHolder(t, "ep-rz-b", "test-rz-b")
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "rz-s2", Name: "other"}, Holder: coord.Self()}); err == nil {
		t.Fatal("same-member hooked claims admitted")
	} else {
		asConflict(t, err)
	}

	for _, tc := range []struct {
		behavior string
		state    ProbeState
		exists   bool
	}{
		{"probe-free", ProbeFree, true},
		{"probe-busy", ProbeBusy, true},
		{"probe-boom", ProbeUnknown, false},
	} {
		t.Setenv("HOOK_BEHAVIOR", tc.behavior)
		st, err := runProbe("s2-probe", "gpu0")
		if err != nil {
			t.Fatal(err)
		}
		if st != tc.state {
			t.Errorf("%s probe = %q, want %q", tc.behavior, st, tc.state)
		}
		hook := kindHook{rec: fleet.ResourceKind{Name: "pz-s2", Match: "member", Probe: "s2-probe"}}
		if hook.Exists("gpu0") != tc.exists {
			t.Errorf("%s exists = %v, want %v", tc.behavior, !tc.exists, tc.exists)
		}
	}
	if (kindHook{rec: fleet.ResourceKind{Name: "pz-s2", Match: "member"}}).Exists("gpu0") {
		t.Error("a kind with no probe reports names existing")
	}
}
