package llmbudget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

func TestPublishBoundariesRecoverWithoutReset(t *testing.T) {
	for _, stage := range []string{"before-marker", "after-marker", "after-meter"} {
		t.Run(stage, func(t *testing.T) {
			g := fixtureGate(t, fixturePolicy())
			o := fixtureOwner(t, g)
			r := demand("new", o)
			g.publishFault = func(at string) error {
				if at == stage {
					return errors.New("injected write failure")
				}
				return nil
			}
			if _, e := g.Reserve(context.Background(), r); e == nil {
				t.Fatal("fault missed")
			}
			recovered := New(g.cfg)
			a, e := recovered.Preview(context.Background(), r)
			switch stage {
			case "before-marker":
				if e != nil || a.Decision.Action != Allow {
					t.Fatal(a, e)
				}
			case "after-marker":
				if e == nil {
					t.Fatal("incomplete first publication reset")
				}
			case "after-meter":
				if e != nil || a.Decision.Action != Queue {
					t.Fatal("committed claim lost", a, e)
				}
				a, e = recovered.Reserve(context.Background(), r)
				if e != nil || a.Reservation == nil {
					t.Fatal("published retry not idempotent", a, e)
				}
			}
		})
	}
}
func TestArchiveCrashCannotAcknowledgeUncommittedSettlement(t *testing.T) {
	for _, stage := range []string{"after-receipt", "before-marker", "after-marker", "after-meter"} {
		t.Run(stage, func(t *testing.T) {
			g := fixtureGate(t, fixturePolicy())
			o := fixtureOwner(t, g)
			ctx := context.Background()
			// Seed 128 committed receipts; the next settlement triggers archival.
			if e := g.transaction(ctx, func() error {
				for i := 0; i < 128; i++ {
					id := string(rune(i + 1000))
					g.state.Completed[id] = Completion{Owner: o.ID(), Request: Request{ID: id, Owner: o.ID()}, Kind: "released", At: g.now()}
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			r := demand("settlement-at-boundary", o)
			if a, e := g.Reserve(ctx, r); e != nil || a.Reservation == nil {
				t.Fatal(a, e)
			}
			actual := Actual{InputTokens: 7, OutputTokens: 3, SpendMicroUSD: i64(1), Source: "fixture"}
			g.publishFault = func(at string) error {
				if at == stage {
					return errors.New("injected crash")
				}
				return nil
			}
			if e := g.Settle(ctx, r.ID, o.ID(), actual); e == nil {
				t.Fatal("fault missed")
			}
			resumed := New(g.cfg)
			if e := resumed.Settle(ctx, r.ID, o.ID(), actual); e != nil {
				t.Fatal("settlement retry failed", e)
			}
			state, e := resumed.stateSnapshot()
			if e != nil || state.Models["one"].DayTokens != 10 || len(state.Reservations) != 0 {
				t.Fatal("settlement lost or doubled", state.Models, e)
			}
			if e := resumed.Settle(ctx, r.ID, o.ID(), actual); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestMissingUsedHardPolicyFailsClosed(t *testing.T) {
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	r := demand("policy", o)
	if _, e := g.Reserve(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	c := g.cfg
	c.Policy = nil
	missing := New(c)
	if _, e := missing.Preview(context.Background(), r); e == nil {
		t.Fatal("missing prior hard policy accepted")
	}
	c.PolicyPath = filepath.Join(t.TempDir(), "missing.json")
	c.StatePath = filepath.Join(t.TempDir(), "brandnew.json")
	if _, e := New(c).Preview(context.Background(), r); e == nil {
		t.Fatal("explicit missing policy accepted")
	}
}
func TestLockCancellationAndRollback(t *testing.T) {
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	l, e := lockfile.TryAcquire(g.cfg.StatePath+".lock", lockfile.Holder{Name: "fixture"})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, e = g.Reserve(ctx, demand("cancel", o))
	l.Release()
	if !errors.Is(e, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("lock ignored cancellation", e)
	}
	g = New(Config{Policy: fixturePolicy(), Models: g.cfg.Models})
	if e := g.transaction(context.Background(), func() error { g.state.Models["ghost"] = Counters{DayTokens: 1}; return errors.New("rollback") }); e == nil {
		t.Fatal("fault missed")
	}
	if _, ok := g.state.Models["ghost"]; ok {
		t.Fatal("rollback retained map insertion")
	}
}
func TestVersionedMeterTamperFailsDigest(t *testing.T) {
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	if _, e := g.Reserve(context.Background(), demand("digest", o)); e != nil {
		t.Fatal(e)
	}
	s, e := g.stateSnapshot()
	if e != nil {
		t.Fatal(e)
	}
	s.Reservations = map[string]Reservation{}
	if e = atomicJSON(g.cfg.StatePath, s); e != nil {
		t.Fatal(e)
	}
	if _, e = New(g.cfg).Preview(context.Background(), demand("bypass", o)); e == nil {
		t.Fatal("reservation deletion passed digest")
	}
	// The marker carries no generation digest and cannot conflict with valid state.
	s.Integrity = stateDigest(s)
	if e = atomicJSON(g.cfg.StatePath, s); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(g.cfg.StatePath+".initialized", []byte(`{"version":1}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = New(g.cfg).Preview(context.Background(), demand("valid", o)); e != nil {
		t.Fatal("marker confused with state digest", e)
	}
}
