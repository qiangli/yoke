package weave

import (
	"strings"
	"testing"
	"time"
)

func TestBatonRoundTripAndRender(t *testing.T) {
	dir := t.TempDir()
	bt := &Baton{Goal: "close the bash-gap", Stage: "sprint 1 of 2",
		Done: []string{"#258 cd merged"}, NextActions: []string{"reassign #259 to claude"},
		Lessons: []string{"codex not steerable"}, WrittenBy: "claude"}
	if err := saveBaton(dir, bt); err != nil {
		t.Fatal(err)
	}
	got, ok := loadBaton(dir)
	if !ok || got.Goal != bt.Goal || len(got.NextActions) != 1 {
		t.Fatalf("round-trip failed: %+v", got)
	}
	md := renderBaton(got)
	for _, want := range []string{"close the bash-gap", "sprint 1 of 2", "reassign #259", "Reconcile with live state"} {
		if !strings.Contains(md, want) {
			t.Fatalf("render missing %q", want)
		}
	}
}

func TestConductorLockExclusivityAndStaleTakeover(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// claude takes it.
	l1, ok := acquireConductorLock(dir, "claude", false, now)
	if !ok || l1.Holder != "claude" || l1.Epoch != 1 {
		t.Fatalf("claude take: ok=%v holder=%s epoch=%d", ok, l1.Holder, l1.Epoch)
	}
	// agy is REFUSED while claude's lock is live.
	if _, ok := acquireConductorLock(dir, "agy", false, now.Add(time.Minute)); ok {
		t.Fatal("agy should be refused while claude holds a live lock")
	}
	// After the TTL with no heartbeat, agy takes over (epoch bumps = fencing token).
	l2, ok := acquireConductorLock(dir, "agy", false, now.Add(conductorLockTTL+time.Minute))
	if !ok || l2.Holder != "agy" || l2.Epoch != 2 {
		t.Fatalf("agy stale-takeover: ok=%v holder=%s epoch=%d (want epoch 2)", ok, l2.Holder, l2.Epoch)
	}
	// Release frees it.
	releaseConductorLock(dir, "agy")
	if _, ok := loadConductorLock(dir); ok {
		t.Fatal("lock should be gone after release")
	}
}
