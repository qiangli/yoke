// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// Regression tests for the independent review of the claim engine.

func ageClaim(c *Claim) {
	c.AcquiredAt = time.Now().Add(-TTL - 2*time.Minute)
	c.Heartbeat = time.Now().Add(-TTL - time.Minute)
}

func TestSameHolderEpisodesDecide(t *testing.T) {
	a := principal.Ref{Name: "claude", Episode: "ep-1", Host: "h"}
	b := principal.Ref{Name: "claude", Episode: "ep-2", Host: "h"}
	if sameHolder(a, b) {
		t.Fatal("two sessions with different episodes share a name and host but are not the same holder")
	}
	if !sameHolder(a, a) {
		t.Fatal("a holder is not itself")
	}
	legacy := principal.Ref{Name: "claude", Host: "h"}
	if !sameHolder(a, legacy) || !sameHolder(legacy, b) {
		t.Fatal("name+host must still identify a holder that carries no episode")
	}
	if sameHolder(principal.Ref{Name: "codex", Host: "h"}, legacy) {
		t.Fatal("different names matched")
	}
}

func TestGuardDoesNotExemptSameNamedSessions(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	a := principal.Ref{Name: "claude", Episode: "ep-s1", Host: "h"}
	b := principal.Ref{Name: "claude", Episode: "ep-s2", Host: "h"}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"name", "same"}, Holder: a}); err != nil {
		t.Fatal(err)
	}
	if err := Guard(ctx, b, Use{Kind: "name", Name: "same"}); !isConflict(err) {
		t.Fatalf("a distinct session got the holder exemption: %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"name", "same"}, Holder: b}); !isConflict(err) {
		t.Fatalf("a distinct session re-acquired another's claim: %v", err)
	}
	if _, err := Refresh(ctx, Ref{"name", "same"}, b, 0); !isConflict(err) {
		t.Fatalf("a distinct session refreshed another's claim: %v", err)
	}
}

type guardProvider struct{}

func (guardProvider) Kind() Kind {
	return Kind{Name: "test-gprov", Domain: "fs", Match: MatchPath}
}
func (guardProvider) Exists(n string) bool { return n == "app" }
func (guardProvider) Members(n string) ([]string, error) {
	return []string{"/w/" + n}, nil
}

func TestGuardResolvesProviderMembers(t *testing.T) {
	ledger(t)
	RegisterProvider(guardProvider{})
	ctx := context.Background()
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"path", "/w/app"}, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"test-gprov", "app"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("acquisition resolves /w/app and must refuse: %v", err)
	}
	if err := Guard(ctx, agentB(), Use{Kind: "test-gprov", Name: "app"}); !isConflict(err) {
		t.Fatalf("Guard skipped provider resolution and admitted a use acquisition refuses: %v", err)
	}
	if err := Guard(ctx, agentA(), Use{Kind: "test-gprov", Name: "app"}); err != nil {
		t.Fatalf("holder refused: %v", err)
	}
}

func TestCustomBackendCASSeesRefreshOfSameEpoch(t *testing.T) {
	ledger(t)
	mb := &memBackend{m: map[string]*Claim{}}
	RegisterKind(Kind{Name: "test-cas", Match: MatchName})
	RegisterBackend("test-cas", mb)
	t.Cleanup(func() { RegisterBackend("test-cas", nil) })
	ctx := context.Background()
	ref := Ref{"test-cas", "k"}
	g, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	ageClaim(mb.m[ref.String()])
	// B reads A's lapsed claim; before B commits, A refreshes it (same epoch).
	mb.afterLoad = func() {
		if _, err := Refresh(ctx, ref, agentA(), g.Epoch); err != nil {
			t.Error(err)
		}
	}
	if _, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("takeover of a claim refreshed under the read was admitted: %v", err)
	}
	if got := mb.m[ref.String()]; got == nil || !sameHolder(got.Holder, agentA()) {
		t.Fatalf("A's refreshed claim was overwritten: %+v", got)
	}
}

func TestCustomBackendEpochSurvivesRelease(t *testing.T) {
	ledger(t)
	mb := &memBackend{m: map[string]*Claim{}}
	RegisterKind(Kind{Name: "test-hw", Match: MatchName})
	RegisterBackend("test-hw", mb)
	t.Cleanup(func() { RegisterBackend("test-hw", nil) })
	ctx := context.Background()
	ref := Ref{"test-hw", "k"}
	g1, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA()})
	if err != nil || g1.Epoch != 1 {
		t.Fatalf("g1 = %+v %v", g1, err)
	}
	if err := ReleaseRef(ctx, ref, agentA(), g1.Epoch); err != nil {
		t.Fatal(err)
	}
	g2, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA()})
	if err != nil || g2.Epoch <= g1.Epoch {
		t.Fatalf("epoch reset across release: %+v %v", g2, err)
	}
	if _, err := Refresh(ctx, ref, agentA(), g1.Epoch); !errors.Is(err, ErrFenced) {
		t.Fatalf("old token still valid after reacquire: %v", err)
	}
	if err := ReleaseRef(ctx, ref, agentA(), g1.Epoch); !errors.Is(err, ErrFenced) {
		t.Fatalf("old token released the new claim: %v", err)
	}
}

func TestDefaultModeCannotMintAttachedWithoutLock(t *testing.T) {
	dir := ledger(t)
	RegisterKind(Kind{Name: "test-attfirst", Match: MatchName, Modes: []string{ModeAttached, ModeLease}})
	ctx := context.Background()
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"test-attfirst", "x"}, Holder: agentA()}); err == nil {
		t.Fatal("AcquireRef with an empty mode created an attached claim without a kernel lock")
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "claim-test-attfirst-*")); len(files) != 0 {
		t.Fatalf("a record was written: %v", files)
	}
	g, err := AcquireRef(ctx, Request{Ref: Ref{"test-attfirst", "x"}, Holder: agentA(), Mode: ModeLease})
	if err != nil || g.Claim.Mode != ModeLease {
		t.Fatalf("explicit lease = %+v %v", g, err)
	}
	// The attached path is still the way to hold such a kind.
	_, l, err := AcquireAttachedRef(ctx, Request{Ref: Ref{"test-attfirst", "y"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
}

func TestReleaseAttachedUsesTheClaimsBackend(t *testing.T) {
	dir := ledger(t)
	mb := &memBackend{m: map[string]*Claim{}}
	RegisterKind(Kind{Name: "test-attmem", Match: MatchName})
	RegisterBackend("test-attmem", mb)
	t.Cleanup(func() { RegisterBackend("test-attmem", nil) })
	ctx := context.Background()
	g, l, err := AcquireAttachedRef(ctx, Request{Ref: Ref{"test-attmem", "k"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	if len(mb.m) != 1 {
		t.Fatalf("attached record not stored through the custom backend: %v", mb.m)
	}
	if err := ReleaseAttached(dir, g.Claim, l); err != nil {
		t.Fatal(err)
	}
	if len(mb.m) != 0 {
		t.Fatalf("cleanup left a permanently live record in the custom backend: %v", mb.m)
	}
	g2, l2, err := AcquireAttachedRef(ctx, Request{Ref: Ref{"test-attmem", "k"}, Holder: agentB()})
	if err != nil {
		t.Fatalf("resource not reusable after cleanup: %v", err)
	}
	if g2.Epoch <= g.Epoch {
		t.Fatalf("epoch %d <= %d", g2.Epoch, g.Epoch)
	}
	l2.Release()
}

func TestStaleAttachedCleanupIsHarmless(t *testing.T) {
	dir := ledger(t)
	ctx := context.Background()
	ref := Ref{"name", "stale-att"}
	gA, lA, err := AcquireAttachedRef(ctx, Request{Ref: ref, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseAttached(dir, gA.Claim, lA); err != nil {
		t.Fatal(err)
	}
	gB, lB, err := AcquireAttachedRef(ctx, Request{Ref: ref, Holder: agentB()})
	if err != nil {
		t.Fatal(err)
	}
	defer lB.Release()
	// A's cleanup runs again with its old Claim and Lock.
	if err := ReleaseAttached(dir, gA.Claim, lA); err != nil {
		t.Fatalf("repeated cleanup errored: %v", err)
	}
	cur, err := (fileBackend{dir: dir}).Load(ref.String())
	if err != nil || cur == nil || !sameHolder(cur.Holder, agentB()) || cur.Epoch != gB.Epoch {
		t.Fatalf("stale cleanup deleted B's record: %+v %v", cur, err)
	}
	if _, _, err := AcquireAttachedRef(ctx, Request{Ref: ref, Holder: agentC()}); !isConflict(err) {
		t.Fatalf("B lost ledger protection: %v", err)
	}
}

func TestAcquireWithOldTokenAfterAbsenceIsFenced(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	ref := Ref{"name", "tok"}
	old, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentB(), Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseRef(ctx, ref, agentB(), b.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA(), Epoch: old.Epoch}); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale token revived an absent claim: %v", err)
	}
	// A third party presenting a token for a claim that is not there, too.
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"name", "never"}, Holder: agentC(), Epoch: 1}); !errors.Is(err, ErrFenced) {
		t.Fatalf("token for a claim that never existed was honoured: %v", err)
	}
	g, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA()})
	if err != nil || g.Epoch <= b.Epoch {
		t.Fatalf("tokenless reacquire = %+v %v", g, err)
	}
	// A different holder presenting the current epoch of a live claim is a conflict, not a fence.
	if _, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentB(), Epoch: g.Epoch}); !isConflict(err) {
		t.Fatalf("current-epoch token from a non-holder = %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentB(), Epoch: g.Epoch + 5}); !errors.Is(err, ErrFenced) {
		t.Fatalf("wrong-epoch token from a non-holder = %v", err)
	}
}

func TestCrossKeyForceDisplacesConflictingClaims(t *testing.T) {
	dir := ledger(t)
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv("BASHY_AUDIT", logPath)
	ctx := context.Background()
	a, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "app"}, Members: []string{"/w/app"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	use := Use{Kind: "path", Name: "/w/app/file"}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"path", "/w/app/file"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("unforced cross-key acquisition = %v", err)
	}
	b, err := AcquireRef(ctx, Request{Ref: Ref{"path", "/w/app/file"}, Holder: agentB(), Force: true, Intent: "urgent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Guard(ctx, agentB(), use); err != nil {
		t.Fatalf("Guard refuses the holder of the forced grant: %v", err)
	}
	if _, err := Refresh(ctx, Ref{"repo", "app"}, agentA(), a.Epoch); !errors.Is(err, ErrFenced) {
		t.Fatalf("displaced holder can still refresh: %v", err)
	}
	if err := Guard(ctx, agentA(), use); !isConflict(err) {
		t.Fatalf("displaced holder still passes Guard: %v", err)
	}
	all, _ := List(dir)
	if len(all) != 1 || !sameHolder(all[0].Holder, agentB()) || all[0].Epoch != b.Epoch {
		t.Fatalf("ledger = %+v", all)
	}
	recs := readAudit(t, logPath)
	if len(recs) != 1 || !strings.Contains(strings.Join(recs[0].Argv, " "), "claude-a@repo:app") {
		t.Fatalf("audit = %+v", recs)
	}
	// The displaced holder reacquires after B releases, with a higher epoch.
	if err := ReleaseRef(ctx, Ref{"path", "/w/app/file"}, agentB(), b.Epoch); err != nil {
		t.Fatal(err)
	}
	a2, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "app"}, Members: []string{"/w/app"}, Holder: agentA()})
	if err != nil || a2.Epoch <= a.Epoch {
		t.Fatalf("reacquire = %+v %v", a2, err)
	}
}

func TestCrossKeyForceNeverDisplacesAttached(t *testing.T) {
	ledger(t)
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv("BASHY_AUDIT", logPath)
	ctx := context.Background()
	_, l, err := AcquireAttachedRef(ctx, Request{Ref: Ref{"path", "/w/kern"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "kern"}, Members: []string{"/w/kern"}, Holder: agentB(), Force: true}); !isConflict(err) {
		t.Fatalf("force displaced an attached hold across keys: %v", err)
	}
	if got := readAudit(t, logPath); len(got) != 0 {
		t.Fatalf("audit = %+v", got)
	}
}

func TestSameHolderLeaseToAttachedTransition(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	ref := Ref{"name", "promote"}
	lease, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA(), Intent: "detached"})
	if err != nil {
		t.Fatal(err)
	}
	g, l, err := AcquireAttachedRef(ctx, Request{Ref: ref, Holder: agentA()})
	if err != nil {
		t.Fatalf("holder could not attach to its own lease: %v", err)
	}
	if g.Claim.Mode != ModeAttached || g.Epoch != lease.Epoch {
		t.Fatalf("grant = %+v (lease epoch %d)", g, lease.Epoch)
	}
	if _, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA()}); !isConflict(err) {
		t.Fatalf("a live attached hold was replaced by a lease: %v", err)
	}
	if _, _, err := AcquireAttachedRef(ctx, Request{Ref: ref, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("other holder attached over it: %v", err)
	}
	l.Release()
}

func TestForceFailsAndRollsBackWhenAuditCannotBeWritten(t *testing.T) {
	dir := ledger(t)
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_AUDIT", filepath.Join(notDir, "audit.jsonl"))
	ctx := context.Background()

	// Same key.
	a, err := AcquireRef(ctx, Request{Ref: Ref{"name", "fa"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"name", "fa"}, Holder: agentB(), Force: true}); err == nil || isConflict(err) {
		t.Fatalf("forced takeover succeeded without an audit record: %v", err)
	}
	if g, err := Refresh(ctx, Ref{"name", "fa"}, agentA(), a.Epoch); err != nil || g.Epoch != a.Epoch {
		t.Fatalf("A's claim was not restored: %+v %v", g, err)
	}

	// Cross key.
	ra, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "app"}, Members: []string{"/w/app"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"path", "/w/app/f"}, Holder: agentB(), Force: true}); err == nil || isConflict(err) {
		t.Fatalf("forced cross-key grant succeeded without an audit record: %v", err)
	}
	if g, err := Refresh(ctx, Ref{"repo", "app"}, agentA(), ra.Epoch); err != nil || g.Epoch != ra.Epoch {
		t.Fatalf("displaced claim was not restored: %+v %v", g, err)
	}
	if c, _ := (fileBackend{dir: dir}).Load("path:/w/app/f"); c != nil {
		t.Fatalf("the failed forced grant remained: %+v", c)
	}
	if err := Guard(ctx, agentB(), Use{Kind: "path", Name: "/w/app/f"}); !isConflict(err) {
		t.Fatalf("Guard after rollback = %v", err)
	}

	// An unforced or non-displacing acquisition needs no audit.
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"name", "free"}, Holder: agentB(), Force: true}); err != nil {
		t.Fatalf("a force that displaced nobody needs no audit: %v", err)
	}
}
