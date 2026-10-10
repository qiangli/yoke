package weave

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/policy/coord"
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

// backdateBaton rewrites the baton claim for dir so its heartbeat (and tenure)
// began `age` ago, standing in for the passage of time the engine reads from
// the wall clock.
func backdateBaton(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	cdir := coord.DefaultDir()
	entries, err := os.ReadDir(cdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(cdir, e.Name())
		b, _ := os.ReadFile(path)
		var m map[string]any
		if json.Unmarshal(b, &m) != nil || m["kind"] != batonKind || m["resource"] != dir {
			continue
		}
		ts := time.Now().Add(-age).UTC().Format(time.RFC3339Nano)
		m["acquired_at"], m["heartbeat"] = ts, ts
		b, _ = json.Marshal(m)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no baton claim for %s in %s", dir, cdir)
}

func TestConductorLockExclusivityAndStaleTakeover(t *testing.T) {
	dir := t.TempDir()
	l1, err := acquireConductorLock(dir, "claude", false)
	if err != nil || l1.Holder != "claude" || l1.Epoch != 1 {
		t.Fatalf("claude take: err=%v lock=%+v", err, l1)
	}
	// agy is REFUSED while claude's lock is live, and told who holds it.
	l, err := acquireConductorLock(dir, "agy", false)
	var conflict *coord.Conflict
	if !errors.As(err, &conflict) || l.Holder != "claude" {
		t.Fatalf("agy should be refused by claude's live lock: err=%v lock=%+v", err, l)
	}
	// After the TTL with no heartbeat, agy takes over (epoch bumps = fencing token).
	backdateBaton(t, dir, conductorLockTTL+time.Minute)
	if cur, _ := loadConductorLock(dir); cur == nil || !cur.stale(time.Now()) {
		t.Fatalf("lapsed lock should read stale: %+v", cur)
	}
	l2, err := acquireConductorLock(dir, "agy", false)
	if err != nil || l2.Holder != "agy" || l2.Epoch != 2 {
		t.Fatalf("agy stale-takeover: err=%v lock=%+v (want epoch 2)", err, l2)
	}
	// Release frees it.
	if err := releaseConductorLock(dir, "agy", 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadConductorLock(dir); ok {
		t.Fatal("lock should be gone after release")
	}
}

func TestConductorLockConcurrentAcquireHasOneWinner(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := acquireConductorLock(dir, fmt.Sprintf("c%d", i), false); err == nil {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d contenders won the baton, want exactly 1", wins.Load())
	}
	if l, ok := loadConductorLock(dir); !ok || l.Epoch != 1 {
		t.Fatalf("lock after the race: %+v", l)
	}
}

func TestConductorLockStaleEpochIsFenced(t *testing.T) {
	dir := t.TempDir()
	if _, err := acquireConductorLock(dir, "old", false); err != nil {
		t.Fatal(err)
	}
	backdateBaton(t, dir, conductorLockTTL+time.Minute)
	l, err := acquireConductorLock(dir, "new", false)
	if err != nil || l.Epoch != 2 {
		t.Fatalf("takeover: err=%v lock=%+v", err, l)
	}
	// The old conductor resumes holding epoch 1: it is not the holder any more.
	if err := heartbeatConductorLock(dir, "old", 1); err == nil {
		t.Fatal("old conductor's heartbeat must be refused")
	}
	// Even under its own name, a stale epoch is fenced rather than trusted.
	if err := heartbeatConductorLock(dir, "new", 1); !errors.Is(err, coord.ErrFenced) {
		t.Fatalf("stale epoch heartbeat = %v, want ErrFenced", err)
	}
	if err := heartbeatConductorLock(dir, "new", 2); err != nil {
		t.Fatalf("held epoch heartbeat: %v", err)
	}
	if err := releaseConductorLock(dir, "new", 1); !errors.Is(err, coord.ErrFenced) {
		t.Fatalf("stale epoch release = %v, want ErrFenced", err)
	}
}

func TestConductorLockZeroHeartbeatIsNotTakeable(t *testing.T) {
	dir := t.TempDir()
	if _, err := acquireConductorLock(dir, "ghost", false); err != nil {
		t.Fatal(err)
	}
	// A claim with no recorded heartbeat is UNKNOWN: nothing says its holder is gone.
	cdir := coord.DefaultDir()
	entries, _ := os.ReadDir(cdir)
	for _, e := range entries {
		path := filepath.Join(cdir, e.Name())
		b, _ := os.ReadFile(path)
		var m map[string]any
		if strings.HasSuffix(e.Name(), ".json") && json.Unmarshal(b, &m) == nil && m["resource"] == dir {
			m["heartbeat"] = time.Time{}
			b, _ = json.Marshal(m)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := acquireConductorLock(dir, "usurper", false); err == nil {
		t.Fatal("an unknown-liveness baton must not be taken without --force")
	}
	if l, err := acquireConductorLock(dir, "usurper", true); err != nil || l.Epoch != 2 {
		t.Fatalf("forced take: err=%v lock=%+v", err, l)
	}
}
