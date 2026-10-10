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

	"github.com/qiangli/yoke/pkg/policy/audit"
	"github.com/qiangli/yoke/pkg/principal"
)

// Regression tests for the forced-takeover re-review: a displacement is one
// transaction that readers cannot see half-done, its audit record precedes the
// grant, and there is neither a rollback nor a silent retry.

// scratchAudit points the force audit at a fresh chain and returns its path.
func scratchAudit(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv("BASHY_AUDIT", p)
	return p
}

// brokenAudit points the force audit at a path that cannot be opened.
func brokenAudit(t *testing.T) {
	t.Helper()
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_AUDIT", filepath.Join(notDir, "audit.jsonl"))
}

func actions(recs []audit.Record) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Action)
	}
	return out
}

// A owns /w/app; B force-acquires /w/app/file. While B's replacement is
// published and A's claim is not yet removed, C's Guard must not be answered
// at all — it waits on the transaction — and once it is, C is refused.
func TestThirdPartyNeverPassesGuardDuringForcedDisplacement(t *testing.T) {
	ledger(t)
	scratchAudit(t)
	ctx := context.Background()
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "app"}, Members: []string{"/w/app"}, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	use := Use{Kind: "path", Name: "/w/app/file"}

	var guardErr error
	var guardReturned, txnReleased time.Time
	done := make(chan struct{})
	forceStepHook = func(stage string) {
		if stage != "published" {
			return
		}
		go func() {
			defer close(done)
			guardErr = Guard(ctx, agentC(), use)
			guardReturned = time.Now()
		}()
		// A reader outside the transaction would answer well within this
		// window; an isolated one is still waiting when it closes.
		select {
		case <-done:
			t.Errorf("Guard answered mid-displacement: %v", guardErr)
		case <-time.After(200 * time.Millisecond):
		}
		txnReleased = time.Now()
	}
	t.Cleanup(func() { forceStepHook = nil })

	if _, err := AcquireRef(ctx, Request{Ref: Ref{"path", "/w/app/file"}, Holder: agentB(), Force: true}); err != nil {
		t.Fatal(err)
	}
	<-done
	if !isConflict(guardErr) {
		t.Fatalf("C after the displacement: %v, want a conflict", guardErr)
	}
	if guardReturned.Before(txnReleased) {
		t.Fatalf("Guard returned at %v, before the transaction ended at %v", guardReturned, txnReleased)
	}
}

// A crash between publishing the replacement and removing the displaced
// claim leaves both live. That state excludes everyone but the two holders
// — each is still blocked by the other's claim — and the next forced
// acquisition heals it with its own audit record.
func TestCrashBetweenPublishAndRemoveLeavesSafeOverlap(t *testing.T) {
	dir := ledger(t)
	logPath := scratchAudit(t)
	ctx := context.Background()
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "app"}, Members: []string{"/w/app"}, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	use := Use{Kind: "path", Name: "/w/app/file"}
	type crash struct{}
	forceStepHook = func(stage string) {
		if stage == "published" {
			panic(crash{})
		}
	}
	t.Cleanup(func() { forceStepHook = nil })
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the simulated crash did not fire")
			} else if _, ok := r.(crash); !ok {
				panic(r)
			}
		}()
		_, _ = AcquireRef(ctx, Request{Ref: Ref{"path", "/w/app/file"}, Holder: agentB(), Force: true})
	}()
	forceStepHook = nil

	all, err := List(dir)
	if err != nil || len(all) != 2 {
		t.Fatalf("ledger after the crash = %+v %v, want both claims live", all, err)
	}
	for _, h := range []struct {
		name string
		who  func() principal.Ref
	}{{"C", agentC}, {"A", agentA}, {"B", agentB}} {
		if err := Guard(ctx, h.who(), use); !isConflict(err) {
			t.Fatalf("%s passed Guard over two overlapping live claims: %v", h.name, err)
		}
	}
	if recs := readAudit(t, logPath); len(recs) != 1 {
		t.Fatalf("audit after the crash = %v, want the one force record", actions(recs))
	}
	// Heal: B's next forced acquisition displaces the lingering claim.
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"path", "/w/app/file"}, Holder: agentB(), Force: true}); err != nil {
		t.Fatalf("healing acquisition: %v", err)
	}
	all, _ = List(dir)
	if len(all) != 1 || !sameHolder(all[0].Holder, agentB()) {
		t.Fatalf("ledger after healing = %+v", all)
	}
	if err := Guard(ctx, agentB(), use); err != nil {
		t.Fatalf("B after healing: %v", err)
	}
	if got := actions(readAudit(t, logPath)); strings.Join(got, ",") != "claim.force,claim.force" {
		t.Fatalf("audit = %v", got)
	}
}

// On a custom backend an audit failure publishes nothing: the backend sees no
// commit, A stays the holder, and no retry adopts the attempt.
func TestAuditFailurePublishesNothingOnCustomBackend(t *testing.T) {
	ledger(t)
	brokenAudit(t)
	mb := &memBackend{m: map[string]*Claim{}}
	RegisterKind(Kind{Name: "test-noaudit", Match: MatchName})
	RegisterBackend("test-noaudit", mb)
	t.Cleanup(func() { RegisterBackend("test-noaudit", nil) })
	ctx := context.Background()
	ref := Ref{"test-noaudit", "k"}
	a, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	before := mb.commits
	_, err = AcquireRef(ctx, Request{Ref: ref, Holder: agentB(), Force: true})
	if err == nil || isConflict(err) {
		t.Fatalf("forced takeover without an audit record = %v", err)
	}
	if mb.commits != before {
		t.Fatalf("the backend saw %d commit(s) for a takeover that was never audited", mb.commits-before)
	}
	if got := mb.m[ref.String()]; got == nil || !sameHolder(got.Holder, agentA()) || got.Epoch != a.Epoch {
		t.Fatalf("A's claim = %+v, want untouched", got)
	}
	if err := Guard(ctx, agentB(), Use{Kind: ref.Kind, Name: ref.Name}); !isConflict(err) {
		t.Fatalf("B passes Guard after a refused force: %v", err)
	}
}

// B force-replaces A on a custom backend; between B's read and its commit
// A refreshes (another process of the current holder). B's audit record is
// already down, so the lost compare-and-swap is recorded as aborted and
// returned — never retried into a grant the first record did not describe.
func TestRefreshDuringPendingForceAbortsWithoutRetry(t *testing.T) {
	ledger(t)
	logPath := scratchAudit(t)
	mb := &memBackend{m: map[string]*Claim{}}
	RegisterKind(Kind{Name: "test-pending", Match: MatchName})
	RegisterBackend("test-pending", mb)
	t.Cleanup(func() { RegisterBackend("test-pending", nil) })
	ctx := context.Background()
	ref := Ref{"test-pending", "k"}
	a, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	mb.afterLoad = func() {
		if _, err := Refresh(ctx, ref, agentA(), a.Epoch); err != nil {
			t.Error(err)
		}
	}
	_, err = AcquireRef(ctx, Request{Ref: ref, Holder: agentB(), Force: true})
	if !errors.Is(err, ErrEpochMismatch) || isConflict(err) {
		t.Fatalf("pending force after a concurrent refresh = %v, want the commit error", err)
	}
	if got := mb.m[ref.String()]; got == nil || !sameHolder(got.Holder, agentA()) || got.Epoch != a.Epoch {
		t.Fatalf("holder after the aborted force = %+v, want A", got)
	}
	recs := readAudit(t, logPath)
	if got := strings.Join(actions(recs), ","); got != "claim.force,claim.force-aborted" {
		t.Fatalf("audit = %v", got)
	}
	if recs[1].Exit != 1 || !strings.Contains(strings.Join(recs[1].Argv, " "), "error=") {
		t.Fatalf("aborted record = %+v", recs[1])
	}
}

// Two B processes force the same takeover at once. Exactly one grant results
// and it carries its own force record; the loser's attempt is recorded as
// aborted. No grant exists that the audit does not account for.
func TestConcurrentForcesProduceOneAuditedGrant(t *testing.T) {
	ledger(t)
	logPath := scratchAudit(t)
	mb := &memBackend{m: map[string]*Claim{}}
	RegisterKind(Kind{Name: "test-race", Match: MatchName})
	RegisterBackend("test-race", mb)
	t.Cleanup(func() { RegisterBackend("test-race", nil) })
	ctx := context.Background()
	ref := Ref{"test-race", "k"}
	if _, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	var inner Grant
	mb.afterLoad = func() {
		g, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentB(), Force: true, Intent: "inner"})
		if err != nil {
			t.Error(err)
		}
		inner = g
	}
	_, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentB(), Force: true, Intent: "outer"})
	if !errors.Is(err, ErrEpochMismatch) {
		t.Fatalf("outer force over the inner's commit = %v, want the commit error, not a silent retry", err)
	}
	got := mb.m[ref.String()]
	if got == nil || !sameHolder(got.Holder, agentB()) || got.Epoch != inner.Epoch {
		t.Fatalf("holder = %+v, want the inner grant (epoch %d)", got, inner.Epoch)
	}
	recs := readAudit(t, logPath)
	if a := strings.Join(actions(recs), ","); a != "claim.force,claim.force,claim.force-aborted" {
		t.Fatalf("audit = %v", a)
	}
	// The inner attempt ran whole inside the outer's read-commit window, so
	// its record precedes the outer's; the abort names the outer attempt.
	joined := func(r audit.Record) string { return strings.Join(r.Argv, " ") }
	if !strings.Contains(joined(recs[0]), "intent=inner") || !strings.Contains(joined(recs[1]), "intent=outer") || !strings.Contains(joined(recs[2]), "intent=outer") {
		t.Fatalf("records = %q / %q / %q", joined(recs[0]), joined(recs[1]), joined(recs[2]))
	}
}

// A forced request that loses its compare-and-swap is not retried: a retry
// would re-read a state the caller never saw and take it over unasked.
func TestForcedAcquisitionNeverRetriesSilently(t *testing.T) {
	ledger(t)
	logPath := scratchAudit(t)
	mb := &memBackend{m: map[string]*Claim{}}
	RegisterKind(Kind{Name: "test-noretry", Match: MatchName})
	RegisterBackend("test-noretry", mb)
	t.Cleanup(func() { RegisterBackend("test-noretry", nil) })
	ctx := context.Background()
	ref := Ref{"test-noretry", "k"}
	// B forces a free key; C takes it between B's read and B's commit.
	mb.afterLoad = func() {
		if _, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentC()}); err != nil {
			t.Error(err)
		}
	}
	_, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentB(), Force: true})
	if !errors.Is(err, ErrEpochMismatch) {
		t.Fatalf("forced acquisition after a lost CAS = %v, want the commit error", err)
	}
	if got := mb.m[ref.String()]; got == nil || !sameHolder(got.Holder, agentC()) {
		t.Fatalf("holder = %+v, want C: the forced request retried and displaced it", got)
	}
	if recs := readAudit(t, logPath); len(recs) != 0 {
		t.Fatalf("audit = %v, want nothing: no takeover happened", actions(recs))
	}
	// The same lost race without Force is retried and ends in an honest conflict.
	mb.afterLoad = nil
	if _, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("unforced acquisition = %v", err)
	}
}
