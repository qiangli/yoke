package dbstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGateAdmissionIsBoundedAndCancellable(t *testing.T) {
	g := NewGate()
	leave, err := g.Enter(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := g.Enter(context.Background(), 50*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("held gate: %v, want busy", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("wait not bounded: %s", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := g.Enter(ctx, time.Minute); Code(err) != "cancelled" {
		t.Fatalf("cancelled wait: %v", err)
	}
	leave()
	leave2, err := g.Enter(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("released gate: %v", err)
	}
	leave2()
}

func TestGuardRejectsUseAfterCloseAndWaitsForInFlight(t *testing.T) {
	var g Guard
	id := Identity{ID: "db:x:mem:s/n"}
	if err := g.Enter(id); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		g.Close(func() error { close(released); return nil })
	}()
	select {
	case <-released:
		t.Fatal("Close ran release while an operation was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	g.Exit()
	<-released
	if err := g.Enter(id); !errors.Is(err, ErrLifetimeEnded) {
		t.Fatalf("enter after close: %v", err)
	}
	calls := 0
	g.Close(func() error { calls++; return nil })
	if calls != 0 {
		t.Fatal("second Close ran release again")
	}
}

func TestMemKVWriterAdmissionIsBounded(t *testing.T) {
	m := NewMemKV()
	hold, done := make(chan struct{}), make(chan struct{})
	go func() {
		m.Update(context.Background(), func(KVTx) error { close(hold); <-done; return nil })
	}()
	<-hold
	defer close(done)
	start := time.Now()
	err := m.UpdateWithin(context.Background(), 50*time.Millisecond, func(KVTx) error { return nil })
	if !errors.Is(err, ErrBusy) || time.Since(start) > time.Second {
		t.Fatalf("contended writer: %v after %s", err, time.Since(start))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.UpdateWithin(ctx, time.Minute, func(KVTx) error { return nil }); Code(err) != "timeout" {
		t.Fatalf("ctx deadline while waiting: %v", err)
	}
	if err := m.View(context.Background(), func(KVTx) error { return nil }); err != nil {
		t.Fatalf("readers must not wait on a writer: %v", err)
	}
}

func TestSummarizeDistinguishesModes(t *testing.T) {
	c := Summarize(Caps{Modes: map[Mode]ModeCaps{
		Persistent: {Supported: true, Via: "native",
			Sharing:   &Sharing{NativeMultiprocess: true},
			Exclusion: &Exclusion{Scopes: []string{"database"}, EnforcedBy: EnforcedByNativeLock, NonBashyClients: NonBashyBlocked}},
		Memory: MemoryModeCaps("native"),
	}})
	if c.Sharing.NativeMultiprocess || !c.Sharing.OwnerRequiredForMultiprocess {
		t.Fatalf("summary must not claim native sharing a mode lacks: %+v", c.Sharing)
	}
	if c.Exclusion.EnforcedBy != "owner+native-lock" || c.Exclusion.NonBashyClients != NonBashyBlocked ||
		len(c.Exclusion.Scopes) != 1 || c.Exclusion.Scopes[0] != "database" {
		t.Fatalf("exclusion summary %+v", c.Exclusion)
	}
	if !c.Modes[Persistent].Sharing.NativeMultiprocess || c.Modes[Memory].Sharing.NativeMultiprocess {
		t.Fatal("per-mode sharing lost")
	}
	coop := Summarize(Caps{Modes: map[Mode]ModeCaps{
		Persistent: {Supported: true, Sharing: &Sharing{OwnerRequiredForMultiprocess: true},
			Exclusion: &Exclusion{Scopes: []string{"database"}, EnforcedBy: EnforcedByNativeLock, NonBashyClients: NonBashyCooperative}},
		Memory: {Supported: false, Reason: "none"},
	}})
	if coop.Exclusion.NonBashyClients != NonBashyCooperative || coop.Exclusion.EnforcedBy != EnforcedByNativeLock {
		t.Fatalf("unsupported mode must not contribute: %+v", coop.Exclusion)
	}
}
