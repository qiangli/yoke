package llmbudget

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigurePolicyValidatesThenAtomicallyApplies(t *testing.T) {
	g := fixtureGate(t, nil)
	input := filepath.Join(t.TempDir(), "input.json")
	b, _ := json.Marshal(fixturePolicy())
	if e := os.WriteFile(input, b, 0600); e != nil {
		t.Fatal(e)
	}
	summary, e := g.ConfigurePolicy(context.Background(), input, false)
	if e != nil || summary.Applied || summary.Bindings != 2 {
		t.Fatal(summary, e)
	}
	if _, e := os.Stat(summary.Path); !os.IsNotExist(e) {
		t.Fatal("dry-run wrote policy")
	}
	summary, e = g.ConfigurePolicy(context.Background(), input, true)
	if e != nil || !summary.Applied {
		t.Fatal(summary, e)
	}
	before, e := os.ReadFile(summary.Path)
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{`{`, `{"version":2}`, `{"version":1,"url":"https://foreign.invalid"}`, `{"version":1} {}`, `{"version":1,"sources":[{"id":"x","kind":"openai-organization","provider":"openai","account":"org","pool":"org","lane":"api-key","organization":"org\r\nInjected: yes"}]}`} {
		if e := os.WriteFile(input, []byte(bad), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := g.ConfigurePolicy(context.Background(), input, true); e == nil {
			t.Fatal("invalid config accepted", bad)
		}
		after, _ := os.ReadFile(summary.Path)
		if string(before) != string(after) {
			t.Fatal("bad input replaced valid policy")
		}
	}
	if e := os.WriteFile(input, []byte(strings.Repeat(" ", (1<<20)+1)), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := g.ConfigurePolicy(context.Background(), input, false); e == nil {
		t.Fatal("oversized policy accepted")
	}
}
func TestUnknownResourceDemandsCannotBypassHardCaps(t *testing.T) {
	g := fixtureGate(t, fixturePolicy())
	o := fixtureOwner(t, g)
	r := demand("unknown", o)
	r.UnknownTokens = true
	r.Tokens = 0
	if a, e := g.Reserve(context.Background(), r); e != nil || a.Decision.Action != Queue || a.Reservation != nil {
		t.Fatal(a, e)
	}
	memory := uint64(100)
	g = fixtureGate(t, &Policy{Version: 1, Constraints: []Constraint{{Host: "host-a", MemoryBytes: &memory}}})
	o = fixtureOwner(t, g)
	r = Request{ID: "memory", Owner: o.ID(), Host: "host-a", UnknownMemory: true, HostSlots: 1}
	if a, e := g.Reserve(context.Background(), r); e != nil || a.Decision.Action != Queue {
		t.Fatal(a, e)
	}
	// An unconstrained unknown allocation must still count as unknown if a cap is
	// configured later. It is never converted into a zero-byte known allocation.
	g.cfg.Policy.Constraints = nil
	if a, e := g.Reserve(context.Background(), r); e != nil || a.Reservation == nil {
		t.Fatal(a, e)
	}
	g.cfg.Policy.Constraints = []Constraint{{Host: "host-a", MemoryBytes: &memory}}
	r.ID = "second"
	r.UnknownMemory = false
	r.MemoryBytes = 1
	if a, e := g.Reserve(context.Background(), r); e != nil || a.Decision.Action != Queue {
		t.Fatal(a, e)
	}
}

func TestTerminatedUnknownTokensRemainUnknownUnderNewPolicy(t *testing.T) {
	p := fixturePolicy()
	p.Constraints = nil
	g := fixtureGate(t, p)
	o := fixtureOwner(t, g)
	r := demand("unknown-terminated", o)
	r.Tokens = 0
	r.UnknownTokens = true
	if a, e := g.Reserve(context.Background(), r); e != nil || a.Reservation == nil {
		t.Fatal(a, e)
	}
	o.Close()
	if a, e := g.Reserve(context.Background(), r); e == nil || a.Reservation != nil {
		t.Fatal("dead owner replay authorized", a, e)
	}
	if e := g.ReconcileTerminated(context.Background(), r.ID, TerminationProof{Owner: o.ID(), Run: r.Run, Host: r.Host, VerifiedAt: g.now(), Evidence: "verified child exit"}); e != nil {
		t.Fatal(e)
	}
	p.Constraints = []Constraint{{Provider: "vendor", DailyTokens: i64(100)}}
	newOwner := fixtureOwner(t, g)
	next := demand("next", newOwner)
	if a, e := g.Reserve(context.Background(), next); e != nil || a.Decision.Action != Queue {
		t.Fatal("unknown completion converted to zero", a, e)
	}
}

func TestEstimatedOpaqueUsageRecoversOnlyAfterWindowReset(t *testing.T) {
	p := fixturePolicy()
	p.Constraints = nil
	g := fixtureGate(t, p)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	g.cfg.Now = func() time.Time { return now }
	o := fixtureOwner(t, g)
	r := demand("estimated", o)
	r.UnknownTokens = true
	if _, e := g.Reserve(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	if e := g.Settle(context.Background(), r.ID, o.ID(), Actual{InputTokens: 1, OutputTokens: 1, TokensEstimated: true, SpendMicroUSD: i64(1)}); e != nil {
		t.Fatal(e)
	}
	p.Constraints = []Constraint{{Provider: "vendor", DailyTokens: i64(100), DailySpendMicroUSD: i64(100)}}
	next := demand("next", o)
	if a, e := g.Reserve(context.Background(), next); e != nil || a.Decision.Action != Queue {
		t.Fatal("estimated usage refunded", a, e)
	}
	now = now.AddDate(0, 0, 1)
	if a, e := g.Reserve(context.Background(), next); e != nil || a.Reservation == nil {
		t.Fatal("closed unknown window stuck", a, e)
	}
}
