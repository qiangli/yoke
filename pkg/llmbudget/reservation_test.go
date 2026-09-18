package llmbudget

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func i64(n int64) *int64 { return &n }
func integer(n int) *int { return &n }
func fixturePolicy() *Policy {
	return &Policy{Version: 1, Bindings: []Binding{{Model: "one", Provider: "vendor", Account: "account", Pool: "pool", Lane: LaneAPIKey}, {Model: "two", Provider: "vendor", Account: "account", Pool: "pool", Lane: LaneAPIKey}}, Constraints: []Constraint{{Provider: "vendor", Account: "account", Pool: "pool", Concurrency: integer(1), DailyTokens: i64(100)}}}
}
func fixtureGate(t *testing.T, p *Policy) *Gate {
	t.Helper()
	return New(Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Policy: p, Models: map[string]Model{"one": {Name: "one", Kind: "api", Provider: "vendor", CostMicro: 1}, "two": {Name: "two", Kind: "api", Provider: "vendor", CostMicro: 1}}})
}
func fixtureOwner(t *testing.T, g *Gate) *OwnerLease {
	t.Helper()
	o, e := g.NewOwner(context.Background(), "test")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { o.Close() })
	return o
}
func demand(id string, o *OwnerLease) Request {
	return Request{ID: id, Owner: o.ID(), Model: "one", Tokens: 60, Concurrency: 1, SpendMicroUSD: i64(10), Run: "run-1", Host: "host-a"}
}
func TestReservationSharedAccountAndSettlement(t *testing.T) {
	ctx := context.Background()
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	r := demand("a", o)
	before := g.state
	for range 2 {
		a, e := g.Preview(ctx, r)
		if e != nil || a.Decision.Action != Allow || a.Reservation != nil {
			t.Fatalf("preview: %+v %v", a, e)
		}
	}
	if _, e := os.Stat(g.cfg.StatePath); !os.IsNotExist(e) {
		t.Fatal("preview published meter")
	}
	_ = before
	a, e := g.Reserve(ctx, r)
	if e != nil || a.Reservation == nil {
		t.Fatalf("reserve: %+v %v", a, e)
	}
	retry, e := g.Reserve(ctx, r)
	if e != nil || retry.Reservation == nil {
		t.Fatal("same id not idempotent", e)
	}
	second := demand("b", o)
	second.Model = "two"
	if a, e = g.Reserve(ctx, second); e != nil || a.Decision.Action != Queue {
		t.Fatalf("shared alias capacity: %+v %v", a, e)
	}
	actual := Actual{InputTokens: 20, OutputTokens: 10, CachedInputTokens: 5, SpendMicroUSD: i64(7), Source: "fixture", ObservedAt: time.Now().UTC()}
	if e = g.Settle(ctx, r.ID, o.ID(), actual); e != nil {
		t.Fatal(e)
	}
	if e = g.Settle(ctx, r.ID, o.ID(), actual); e != nil {
		t.Fatal("duplicate settlement", e)
	}
	actual.InputTokens++
	if e = g.Settle(ctx, r.ID, o.ID(), actual); e == nil {
		t.Fatal("conflicting settlement accepted")
	}
	state, e := g.stateSnapshot()
	if e != nil || state.Models["one"].DayTokens != 30 {
		t.Fatalf("usage charged twice: %+v %v", state.Models, e)
	}
	if a, e = g.Reserve(ctx, second); e != nil || a.Reservation == nil {
		t.Fatalf("headroom not released: %+v %v", a, e)
	}
	if e = g.Release(ctx, second.ID, o.ID()); e != nil {
		t.Fatal(e)
	}
	if e = g.Release(ctx, second.ID, o.ID()); e != nil {
		t.Fatal("release not idempotent", e)
	}
}
func TestHardLimitsUnknownStateAndReset(t *testing.T) {
	ctx := context.Background()
	p := fixturePolicy()
	p.Constraints[0].DailySpendMicroUSD = i64(100)
	g := fixtureGate(t, p)
	o := fixtureOwner(t, g)
	r := demand("x", o)
	r.SpendMicroUSD = nil
	if a, e := g.Reserve(ctx, r); e != nil || a.Decision.Action != Queue {
		t.Fatalf("unknown spend allowed: %+v %v", a, e)
	}
	r.SpendMicroUSD = i64(90)
	r.AllowPremium = true
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	g.cfg.Now = func() time.Time { return now }
	if a, e := g.Reserve(ctx, r); e != nil || a.Reservation == nil {
		t.Fatalf("reserve: %+v %v", a, e)
	}
	if e := g.Settle(ctx, "x", o.ID(), Actual{InputTokens: 60, SpendMicroUSD: i64(90)}); e != nil {
		t.Fatal(e)
	}
	r.ID = "y"
	r.Tokens = 10
	r.SpendMicroUSD = i64(20)
	if a, e := g.Reserve(ctx, r); e != nil || a.Decision.Action != Queue {
		t.Fatalf("premium bypassed hard policy: %+v %v", a, e)
	}
	now = now.AddDate(0, 0, 1)
	if a, e := g.Reserve(ctx, r); e != nil || a.Reservation == nil {
		t.Fatalf("reset did not recover: %+v %v", a, e)
	}
	for _, corruption := range []string{"", "{", "{}", `{"version":1}`} {
		t.Run("corrupt"+corruption, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meter")
			if e := os.WriteFile(path, []byte(corruption), 0600); e != nil {
				t.Fatal(e)
			}
			bad := New(Config{StatePath: path, Policy: p})
			if a, e := bad.Preview(ctx, r); e == nil || a.Decision.Allowed() {
				t.Fatalf("corruption reset allowed: %+v %v", a, e)
			}
		})
	}
	if e := os.Remove(g.cfg.StatePath); e != nil {
		t.Fatal(e)
	}
	restarted := New(g.cfg)
	if a, e := restarted.Preview(ctx, r); e == nil || a.Decision.Allowed() {
		t.Fatalf("deleted initialized state reset after restart: %+v %v", a, e)
	}
}
func TestLegacyLimitsReserveAtomicallyAndPreviewDoesNotConsume(t *testing.T) {
	ctx := context.Background()
	g := fixtureGate(t, &Policy{Version: 1})
	o := fixtureOwner(t, g)
	g.cfg.Models["one"] = Model{Name: "one", Kind: "api", Provider: "vendor", CostMicro: 1, Limits: Limits{RateTokens: 100, RatePer: time.Hour}}
	r := demand("first", o)
	for range 2 {
		a, e := g.Preview(ctx, r)
		if e != nil || a.Decision.Action != Allow {
			t.Fatal(a, e)
		}
	}
	if a, e := g.Reserve(ctx, r); e != nil || a.Reservation == nil {
		t.Fatal(a, e)
	}
	if e := g.Release(ctx, r.ID, o.ID()); e != nil {
		t.Fatal(e)
	}
	r.ID = "second"
	if a, e := New(g.cfg).Reserve(ctx, r); e != nil || a.Decision.Action != Queue {
		t.Fatalf("legacy rate bucket bypassed: %+v %v", a, e)
	}
	g2 := fixtureGate(t, &Policy{Version: 1})
	o2 := fixtureOwner(t, g2)
	g2.cfg.Models["one"] = Model{Name: "one", Kind: "api", Provider: "vendor", CostMicro: 1000, Limits: Limits{BudgetUSD: 0.1}}
	r = demand("spend1", o2)
	r.SpendMicroUSD = i64(60000)
	if a, e := g2.Reserve(ctx, r); e != nil || a.Reservation == nil {
		t.Fatal(a, e)
	}
	r.ID = "spend2"
	if a, e := g2.Reserve(ctx, r); e != nil || a.Decision.Action == Allow {
		t.Fatalf("legacy pending spend ignored: %+v %v", a, e)
	}
}
func TestHostAxesAndTerminationProof(t *testing.T) {
	ctx := context.Background()
	p := fixturePolicy()
	p.Constraints = append(p.Constraints, Constraint{Host: "host-a", HostSlots: integer(1)})
	g := fixtureGate(t, p)
	o := fixtureOwner(t, g)
	r := Request{ID: "host1", Owner: o.ID(), Host: "host-a", Run: "build", HostSlots: 1}
	if a, e := g.Reserve(ctx, r); e != nil || a.Reservation == nil {
		t.Fatal(a, e)
	}
	o.Close()
	now := time.Now().Add(48 * time.Hour)
	g.cfg.Now = func() time.Time { return now }
	next := fixtureOwner(t, g)
	r.ID = "host2"
	r.Owner = next.ID()
	if a, e := g.Reserve(ctx, r); e != nil || a.Decision.Action != Queue {
		t.Fatalf("dead owner/TTL reclaimed surviving work: %+v %v", a, e)
	}
	proof := TerminationProof{Owner: o.ID(), Host: "host-a", Run: "wrong", VerifiedAt: now, Evidence: "process tree verified terminated"}
	if e := g.ReconcileTerminated(ctx, "host1", proof); e == nil {
		t.Fatal("wrong run proof accepted")
	}
	proof.Run = "build"
	if e := g.ReconcileTerminated(ctx, "host1", proof); e != nil {
		t.Fatal(e)
	}
	if a, e := g.Reserve(ctx, r); e != nil || a.Reservation == nil {
		t.Fatalf("verified termination not reclaimed: %+v %v", a, e)
	}
}
func TestReceiptCompactionKeepsIdempotency(t *testing.T) {
	ctx := context.Background()
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	for n := 0; n < 140; n++ {
		r := Request{ID: strings.Repeat("x", n+1), Owner: o.ID(), Host: "host-a", HostSlots: 1}
		if a, e := g.Reserve(ctx, r); e != nil || a.Reservation == nil {
			t.Fatal(a, e)
		}
		if e := g.Release(ctx, r.ID, o.ID()); e != nil {
			t.Fatal(e)
		}
	}
	if len(g.state.Completed) > 128 {
		t.Fatal("receipt state unbounded")
	}
	restarted := New(g.cfg)
	r := Request{ID: "x", Owner: o.ID(), Host: "host-a", HostSlots: 1}
	if a, e := restarted.Reserve(ctx, r); e != nil || a.Decision.Action != Block {
		t.Fatalf("archived id replay admitted: %+v %v", a, e)
	}
}

func TestBudgetProcessHelper(t *testing.T) {
	mode := os.Getenv("LLMBUDGET_HELPER")
	if mode == "" {
		return
	}
	path := os.Getenv("LLMBUDGET_HELPER_PATH")
	cfg := Config{StatePath: path, Policy: fixturePolicy(), Models: map[string]Model{"one": {Name: "one", Kind: "api", Provider: "vendor", CostMicro: 1}}}
	g := New(cfg)
	if mode == "record" {
		for range 20 {
			g.Record("one", 1, 0, 0)
		}
		os.Exit(0)
	}
	o, e := g.NewOwner(context.Background(), "child")
	if e != nil {
		os.Exit(11)
	}
	r := demand(os.Getenv("LLMBUDGET_HELPER_ID"), o)
	a, e := g.Reserve(context.Background(), r)
	if e != nil {
		os.Exit(12)
	}
	json.NewEncoder(os.Stdout).Encode(struct {
		Admission Admission
		Owner     string
	}{a, o.ID()})
	io.Copy(io.Discard, os.Stdin)
	o.Close()
	os.Exit(0)
}
func helperCmd(path, id, mode string) *exec.Cmd {
	c := exec.Command(os.Args[0], "-test.run=^TestBudgetProcessHelper$")
	c.Env = append(os.Environ(), "LLMBUDGET_HELPER="+mode, "LLMBUDGET_HELPER_PATH="+path, "LLMBUDGET_HELPER_ID="+id)
	return c
}
func TestMultiprocessReservationAndCrash(t *testing.T) {
	g := fixtureGate(t, fixturePolicy())
	var commands []*exec.Cmd
	var inputs []io.WriteCloser
	var readers []*bufio.Reader
	for _, id := range []string{"p1", "p2", "p3", "p4"} {
		c := helperCmd(g.cfg.StatePath, id, "reserve")
		input, e := c.StdinPipe()
		if e != nil {
			t.Fatal(e)
		}
		output, e := c.StdoutPipe()
		if e != nil {
			t.Fatal(e)
		}
		if e = c.Start(); e != nil {
			t.Fatal(e)
		}
		commands = append(commands, c)
		inputs = append(inputs, input)
		readers = append(readers, bufio.NewReader(output))
	}
	t.Cleanup(func() {
		for i, c := range commands {
			inputs[i].Close()
			if c.ProcessState == nil {
				c.Process.Kill()
				c.Wait()
			}
		}
	})
	admitted := 0
	var winning Reservation
	winner := -1
	for i, r := range readers {
		line, e := r.ReadBytes('\n')
		if e != nil {
			t.Fatalf("child %d: %v", i, e)
		}
		var got struct {
			Admission Admission
			Owner     string
		}
		if e = json.Unmarshal(line, &got); e != nil {
			t.Fatal(e)
		}
		if got.Admission.Reservation != nil {
			admitted++
			winning = *got.Admission.Reservation
			winner = i
		}
	}
	if admitted != 1 {
		t.Fatalf("concurrent processes admitted %d, want 1", admitted)
	}
	commands[winner].Process.Kill()
	commands[winner].Wait()
	o := fixtureOwner(t, g)
	next := demand("after-crash", o)
	if a, e := g.Reserve(context.Background(), next); e != nil || a.Decision.Action != Queue {
		t.Fatalf("crash lost reservation: %+v %v", a, e)
	}
	proof := TerminationProof{Owner: winning.Owner, Run: winning.Request.Run, Host: winning.Request.Host, VerifiedAt: time.Now().UTC(), Evidence: "fixture child terminated; no external work"}
	if e := g.ReconcileTerminated(context.Background(), winning.ID, proof); e != nil {
		t.Fatal(e)
	}
	// Token estimate is conservatively retained; reduced demand fits remaining.
	next.Tokens = 30
	if a, e := g.Reserve(context.Background(), next); e != nil || a.Reservation == nil {
		t.Fatalf("proof did not release concurrency: %+v %v", a, e)
	}
}
func TestMultiprocessLegacyRecordDoesNotLoseCounters(t *testing.T) {
	g := fixtureGate(t, fixturePolicy())
	var commands []*exec.Cmd
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		c := helperCmd(g.cfg.StatePath, id, "record")
		if e := c.Start(); e != nil {
			t.Fatal(e)
		}
		commands = append(commands, c)
	}
	for _, c := range commands {
		if e := c.Wait(); e != nil {
			t.Fatal(e)
		}
	}
	s, e := g.stateSnapshot()
	if e != nil || s.Models["one"].DayTokens != 80 {
		t.Fatalf("lost concurrent writes: %+v %v", s.Models, e)
	}
}
