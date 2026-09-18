package chat

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmbudget"
)

func chatCapacityGate(t *testing.T, cap int) *llmbudget.Gate {
	t.Helper()
	g := llmbudget.New(llmbudget.Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Models: map[string]llmbudget.Model{"opus5": {Name: "opus5", Provider: "anthropic", Kind: "api", CostMicro: 1}}, Policy: &llmbudget.Policy{Version: 1, Bindings: []llmbudget.Binding{{Model: "opus5", Provider: "anthropic", Account: "acct", Pool: "pool", Lane: llmbudget.LaneAPIKey}}, Constraints: []llmbudget.Constraint{{HostSlots: &cap}}}})
	restore := llmbudget.SetDefault(g)
	t.Cleanup(restore)
	return g
}
func TestBudgetInvokeConcurrentLaunchRefusesBeforeRunner(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	isolatedRoom(t)
	g := chatCapacityGate(t, 1)
	r := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan invokeOutcome, 1)
	go func() {
		v, e := Invoke(context.Background(), Options{Agent: "claude:opus", Instruction: "first", Cwd: t.TempDir()}, r)
		done <- invokeOutcome{v, e}
	}()
	waitFor(t, r.started, "first gated runner")
	second := &fakeRunner{}
	_, e := Invoke(context.Background(), Options{Agent: "claude:opus", Instruction: "second", Cwd: t.TempDir()}, second)
	if e == nil || second.agent != "" {
		t.Fatal("capacity bypassed", e, second.agent)
	}
	close(r.release)
	if result := waitFor(t, done, "gated completion"); result.err != nil {
		t.Fatal(result.err)
	}
	report, e := g.CollectReport(context.Background(), llmbudget.ReportOptions{Model: "opus5"})
	if e != nil {
		t.Fatal(e)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].ActiveReservations != 0 {
		t.Fatal(report)
	}
	found := false
	for _, m := range report.Accounts[0].Metrics {
		if m.Name == "usage.input_tokens" && m.Source == "local-meter" {
			found = true
			if m.Classification != "estimated" {
				t.Fatal("harness text called actual", m)
			}
		}
	}
	if !found {
		t.Fatal("estimated observation missing")
	}
}
func TestBudgetOpaqueUnknownAndErrorRetention(t *testing.T) {
	g := chatCapacityGate(t, 1)
	l := Launch{ModelName: "opus5", Nick: "claude", ToolName: "claude"}
	work, e := reserveBudgetWork(context.Background(), l, "prompt", "run", true, false)
	if e != nil {
		t.Fatal(e)
	}
	if e = work.finish("partial", context.Canceled); e != nil {
		t.Fatal(e)
	}
	if _, e = reserveBudgetWork(context.Background(), l, "next", "run-2", true, false); e == nil {
		t.Fatal("cancelled opaque work refunded")
	}
	report, e := g.CollectReport(context.Background(), llmbudget.ReportOptions{Model: "opus5"})
	if e != nil || report.Accounts[0].ActiveReservations != 1 {
		t.Fatal(report, e)
	}
}
func TestBudgetHardUnknownUsageRefusesBeforeLaunch(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	isolatedRoom(t)
	_ = chatCapacityGate(t, 1)
	// A separate explicit hard token policy cannot accept rune estimates as a cap.
	cap := int64(10000)
	g := llmbudget.New(llmbudget.Config{Models: map[string]llmbudget.Model{"opus5": {Name: "opus5", Kind: "api", Provider: "anthropic", CostMicro: 1}}, Policy: &llmbudget.Policy{Version: 1, Bindings: []llmbudget.Binding{{Model: "opus5", Provider: "anthropic", Account: "acct", Pool: "pool", Lane: llmbudget.LaneAPIKey}}, Constraints: []llmbudget.Constraint{{DailyTokens: &cap}}}})
	restore := llmbudget.SetDefault(g)
	defer restore()
	r := &fakeRunner{}
	_, e := Invoke(context.Background(), Options{Agent: "claude:opus", Instruction: "tiny", Cwd: t.TempDir()}, r)
	if e == nil || r.agent != "" || !strings.Contains(e.Error(), "unknown") {
		t.Fatal(e, r.agent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = reserveBudgetWork(ctx, Launch{ModelName: "opus5"}, "x", "cancel", true, false); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestBudgetAlternativeRequiresExplicitPaidPermission(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	isolatedRoom(t)
	zero, one := 0, 1
	p := &llmbudget.Policy{Version: 1, Bindings: []llmbudget.Binding{{Model: "opus5", Provider: "anthropic", Account: "subscription", Pool: "sub", Lane: llmbudget.LaneSubscription}, {Model: "sonnet5", Provider: "anthropic", Account: "paid", Pool: "api", Lane: llmbudget.LaneAPIKey}}, Constraints: []llmbudget.Constraint{{Account: "subscription", Concurrency: &zero}, {Account: "paid", Concurrency: &one}}, Routes: []llmbudget.Route{{From: "opus5", To: "sonnet5", AllowPremium: true}}}
	g := llmbudget.New(llmbudget.Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Policy: p, Models: map[string]llmbudget.Model{"opus5": {Name: "opus5", Provider: "anthropic", Kind: "subscription"}, "sonnet5": {Name: "sonnet5", Provider: "anthropic", Kind: "api", CostMicro: 1}}})
	restore := llmbudget.SetDefault(g)
	defer restore()
	r := &fakeRunner{}
	_, e := Invoke(context.Background(), Options{Agent: "claude:opus", Instruction: "work", Cwd: t.TempDir()}, r)
	if e == nil || r.agent != "" {
		t.Fatal("paid fallback launched without permission", e)
	}
	res, e := Invoke(context.Background(), Options{Agent: "claude:opus", Instruction: "work", Cwd: t.TempDir(), AllowPremium: true}, r)
	if e != nil || r.agent == "" || !strings.Contains(res.Model, "sonnet") {
		t.Fatal("explicit allowed fallback failed", res, e)
	}
}
