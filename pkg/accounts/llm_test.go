package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
	"github.com/qiangli/yoke/pkg/llmbudget"
)

type llmFixture struct {
	adapter *LLMAdapter
	now     time.Time
	refresh []bool
}

func llmMetric(name string, v float64, unit, class, source string) llmbudget.Metric {
	return llmbudget.Metric{Name: name, Value: &v, Unit: unit, Classification: class, Source: source, ObservedAt: time.Now()}
}

func newLLMFixture() *llmFixture {
	f := &llmFixture{now: time.Now().Truncate(time.Second)}
	day := f.now.UTC().Truncate(24 * time.Hour)
	dayEnd := day.Add(24 * time.Hour)
	reset := f.now.Add(5 * time.Hour)
	local := func(name string, v float64, unit string) llmbudget.Metric {
		m := llmMetric(name, v, unit, "actual", "local-meter")
		m.WindowStart, m.WindowEnd = &day, &dayEnd
		return m
	}
	report := &llmbudget.Report{
		SchemaVersion: "bashy-model-usage-v1",
		GeneratedAt:   f.now,
		Warnings:      []string{"fixture warning " + accountstest.CanarySecret},
		Accounts: []llmbudget.AccountReport{
			{
				Provider: "owl", Account: "alice-seat", Pool: "pro", Lane: llmbudget.LaneSubscription,
				AccountKnown: true, Status: "ok",
				Models: []string{"owl-model-a"}, Agents: []string{"alice-agent"},
				Attribution: []llmbudget.Attribution{{Model: "owl-model-a", Agent: "alice-agent", Host: accountstest.CanarySecret}},
				Metrics: []llmbudget.Metric{
					llmMetric("usage.input_tokens", 100, "tokens", "actual", "owl-vendor"),
					llmMetric("usage.output_tokens", 50, "tokens", "actual", "owl-vendor"),
					llmMetric("usage.cached_input_tokens", 20, "tokens", "actual", "owl-vendor"),
					llmMetric("usage.context_remaining_percent", 80, "percent", "actual", "owl-vendor"),
					llmMetric("context.input_tokens", 7, "tokens", "actual", "owl-vendor"),
					func() llmbudget.Metric {
						m := llmMetric("quota.used_percent", 42, "percent", "actual", "owl-vendor:five_hour")
						m.ResetAt = &reset
						return m
					}(),
					func() llmbudget.Metric {
						m := llmMetric("billing.spend", 1.25, "usd", "estimated", "owl-vendor")
						m.Limitation = "Session cost is estimated, not account billing."
						return m
					}(),
					local("usage.tokens", 150, "tokens"),
					local("usage.requests", 9, "requests"),
				},
			},
			{
				Provider: "owl", Account: "alice-seat", Pool: "max", Lane: llmbudget.LaneSubscription,
				AccountKnown: true, Status: "ok",
				Models: []string{"owl-model-b"}, Agents: []string{"bob-agent"},
				Metrics: []llmbudget.Metric{
					llmMetric("usage.input_tokens", 200, "tokens", "actual", "owl-vendor"),
					func() llmbudget.Metric {
						m := llmMetric("usage.output_tokens", 60, "tokens", "estimated", "owl-vendor")
						m.Limitation = "Sampled tail; not a full-window measurement."
						return m
					}(),
					local("usage.tokens", 200, "tokens"),
				},
			},
			{
				Provider: "owl", Account: "alice-key", Pool: "alice-key", Lane: llmbudget.LaneAPIKey,
				AccountKnown: true, Status: "ok",
				Models: []string{"owl-model-c"},
				Metrics: []llmbudget.Metric{
					llmMetric("billing.spend", 3, "usd", "actual", "owl-billing"),
					llmMetric("usage.requests", 11, "requests", "actual", "owl-billing"),
				},
			},
			{
				// Unrelated seat sharing a model name with alice-seat: shared
				// bindings must not merge distinct provider identities.
				Provider: "owl", Account: "bob-seat", Pool: "pro", Lane: llmbudget.LaneSubscription,
				AccountKnown: true, Status: "stale",
				Models: []string{"owl-model-a"}, Agents: []string{"otter-agent"},
				Limitations: []string{"Cached sample is older than the freshness bound."},
				Metrics: []llmbudget.Metric{
					llmMetric("usage.input_tokens", 5, "tokens", "actual", "owl-vendor"),
					llmMetric("billing.spend", 0.5, "usd", "actual", "owl-vendor"),
				},
			},
			{
				Provider: "otter", Account: "alice-seat", Pool: "pro", Lane: llmbudget.LaneSubscription,
				AccountKnown: true, Status: "unavailable",
				Limitations: []string{"No supported observation for this account."},
				Metrics: []llmbudget.Metric{
					{Name: "usage.input_tokens", Unit: "tokens", Classification: "unknown", Source: "unavailable", ObservedAt: f.now, Limitation: "No supported observation for this account."},
				},
			},
			{
				Provider: "owl", Account: "unresolved", Pool: "guest", Lane: llmbudget.LaneSubscription,
				AccountKnown: false, Status: "unsupported",
				Models: []string{"owl-guest"},
			},
		},
	}
	f.adapter = &LLMAdapter{collect: func(_ context.Context, opt llmbudget.ReportOptions) (*llmbudget.Report, error) {
		f.refresh = append(f.refresh, opt.Refresh)
		return report, nil
	}}
	return f
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func findMetric(ms []Metric, name, source string) (Metric, bool) {
	for _, m := range ms {
		if m.Name == name && (source == "" || m.Source == source) {
			return m, true
		}
	}
	return Metric{}, false
}

func TestLLMDiscoverAggregatesSharedSeat(t *testing.T) {
	f := newLLMFixture()
	ctx := context.Background()
	obs, err := f.adapter.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, obs))
	var seat *Observation
	count := 0
	for i := range obs {
		if obs[i].Account.ProviderAccountID == "alice-seat" && obs[i].Account.Provider == "owl" {
			seat = &obs[i]
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared seat aggregated %d times, want 1: %s", count, mustJSON(t, obs))
	}
	if seat.Account.Category != LLM || seat.Account.Status != Resolved {
		t.Fatalf("bad seat record: %+v", seat.Account)
	}
	if seat.Account.Lane != string(llmbudget.LaneSubscription) {
		t.Fatalf("lane = %q, want subscription", seat.Account.Lane)
	}
	if len(seat.Account.Connections) != 2 {
		t.Fatalf("connections = %+v, want one per pool", seat.Account.Connections)
	}
	if len(seat.Account.Links.Models) != 2 || len(seat.Account.Links.Agents) != 2 {
		t.Fatalf("links = %+v, want both pools bound", seat.Account.Links)
	}
	if seat.SourceKey != "" {
		t.Fatalf("verified seat carries source key %q", seat.SourceKey)
	}
	for _, r := range f.refresh {
		if r {
			t.Fatal("Discover triggered a refresh")
		}
	}
}

func TestLLMDiscoverKeepsUnrelatedSeatsSeparate(t *testing.T) {
	f := newLLMFixture()
	obs, err := f.adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, obs))
	verified := map[string]int{}
	for _, o := range obs {
		if o.Account.ProviderAccountID != "" {
			verified[o.Account.Provider+"/"+o.Account.ProviderAccountID]++
		}
	}
	for _, key := range []string{"owl/alice-seat", "owl/alice-key", "owl/bob-seat", "otter/alice-seat"} {
		if verified[key] != 1 {
			t.Fatalf("seat %s appears %d times: %v", key, verified[key], verified)
		}
	}
	// Subscription and API lanes are distinct access methods, not one account.
	for _, o := range obs {
		if o.Account.Provider == "owl" && o.Account.ProviderAccountID == "alice-key" && o.Account.Lane != string(llmbudget.LaneAPIKey) {
			t.Fatalf("api lane = %q", o.Account.Lane)
		}
	}
	// The unverified guest seat stays unresolved and keyed by pool, not identity.
	found := false
	for _, o := range obs {
		if o.SourceKey != "" {
			found = true
			if o.Account.ProviderAccountID != "" {
				t.Fatalf("unresolved seat claims identity %q", o.Account.ProviderAccountID)
			}
		}
	}
	if !found {
		t.Fatal("unverified seat missing from discovery")
	}
}

func TestLLMObserveRoundTrip(t *testing.T) {
	f := newLLMFixture()
	obs, err := f.adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := Open(t.TempDir() + "/accounts.json")
	if err := s.Observe(f.adapter.Name(), obs); err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(f.adapter.Name(), obs); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, list))
	verified, unresolved := 0, 0
	for _, a := range list {
		switch a.Status {
		case Resolved:
			verified++
		case Unresolved:
			unresolved++
		default:
			t.Fatalf("unexpected status %s for %+v", a.Status, a)
		}
	}
	if verified != 4 || unresolved != 1 {
		t.Fatalf("verified=%d unresolved=%d, want 4 and 1", verified, unresolved)
	}
}

func TestLLMUsageNilNotZero(t *testing.T) {
	f := newLLMFixture()
	ctx := context.Background()
	usage, err := f.adapter.Usage(ctx, Account{Provider: "owl", ProviderAccountID: "alice-seat"}, "day")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, usage))
	in, ok := findMetric(usage, "usage.input_tokens", "owl-vendor")
	if !ok || in.Value == nil || *in.Value != 100 && *in.Value != 200 {
		t.Fatalf("input tokens missing: %s", mustJSON(t, usage))
	}
	if in.Classification != ProviderReported {
		t.Fatalf("vendor input classification = %v", in.Classification)
	}
	if in.Coverage != Partial {
		t.Fatalf("point sample coverage = %v, want partial", in.Coverage)
	}
	day, ok := findMetric(usage, "usage.tokens", "local-meter")
	if !ok || day.Value == nil || day.Coverage != Complete {
		t.Fatalf("day meter = %+v, want complete coverage", day)
	}
	if day.Classification != LocalObservation {
		t.Fatalf("local meter classification = %v", day.Classification)
	}
	week, err := f.adapter.Usage(ctx, Account{Provider: "owl", ProviderAccountID: "alice-seat"}, "week")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, week))
	wday, ok := findMetric(week, "usage.tokens", "local-meter")
	if !ok || wday.Coverage != Partial || len(wday.Limitations) == 0 {
		t.Fatalf("week view hides partial coverage: %+v", wday)
	}
	// Stale and unavailable seats surface nil, never zero.
	stale, err := f.adapter.Usage(ctx, Account{Provider: "owl", ProviderAccountID: "bob-seat"}, "day")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, stale))
	if len(stale) == 0 {
		t.Fatal("stale seat returned no metrics")
	}
	for _, m := range stale {
		if m.Value != nil {
			t.Fatalf("stale metric has value %v: %+v", *m.Value, m)
		}
		if m.Coverage != Unknown {
			t.Fatalf("stale coverage = %v, want unknown", m.Coverage)
		}
	}
	missing, err := f.adapter.Usage(ctx, Account{Provider: "otter", ProviderAccountID: "alice-seat"}, "day")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, missing))
	for _, m := range missing {
		if m.Value != nil {
			t.Fatalf("unsupported metric has value %v", *m.Value)
		}
	}
	if _, err := f.adapter.Usage(ctx, Account{Provider: "owl", ProviderAccountID: "nobody"}, "day"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown seat usage = %v, want ErrNotFound", err)
	}
	if _, err := f.adapter.Usage(ctx, Account{Provider: "owl", ProviderAccountID: "alice-seat"}, "fortnight"); err == nil {
		t.Fatal("bad period accepted")
	}
}

func TestLLMUsageRefusesDoubleCount(t *testing.T) {
	f := newLLMFixture()
	usage, err := f.adapter.Usage(context.Background(), Account{Provider: "owl", ProviderAccountID: "alice-seat"}, "day")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, usage))
	var dayTokens []Metric
	for _, m := range usage {
		if m.Name == "usage.tokens" && m.Value != nil {
			dayTokens = append(dayTokens, m)
		}
	}
	if len(dayTokens) != 2 {
		t.Fatalf("want both pool day totals, got %s", mustJSON(t, usage))
	}
	if _, err := SumMetrics(dayTokens[0], dayTokens[1]); !errors.Is(err, ErrOverlappingMetric) {
		t.Fatalf("overlapping source windows summed: %v", err)
	}
}

func TestLLMContextNeverCumulative(t *testing.T) {
	f := newLLMFixture()
	usage, err := f.adapter.Usage(context.Background(), Account{Provider: "owl", ProviderAccountID: "alice-seat"}, "day")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, usage))
	for _, name := range []string{"context.input_tokens", "usage.context_remaining_percent"} {
		m, ok := findMetric(usage, name, "")
		if !ok {
			t.Fatalf("%s absent from usage", name)
		}
		if _, err := SumMetrics(m); !errors.Is(err, ErrCurrentContext) {
			t.Fatalf("%s summed: %v", name, err)
		}
	}
}

func TestLLMEstimatesCarryLimitations(t *testing.T) {
	f := newLLMFixture()
	ctx := context.Background()
	usage, err := f.adapter.Usage(ctx, Account{Provider: "owl", ProviderAccountID: "alice-seat"}, "day")
	if err != nil {
		t.Fatal(err)
	}
	billing, err := f.adapter.Billing(ctx, Account{Provider: "owl", ProviderAccountID: "alice-seat"}, "day")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, usage), mustJSON(t, billing))
	estimates := 0
	for _, m := range append(append([]Metric(nil), usage...), billing...) {
		if m.Classification == Estimate {
			estimates++
			if len(m.Limitations) == 0 {
				t.Fatalf("estimate without limitations: %+v", m)
			}
		}
	}
	if estimates == 0 {
		t.Fatal("no estimates surfaced")
	}
	spend, ok := findMetric(billing, "billing.spend", "")
	if !ok || spend.Value == nil || spend.Currency != "USD" {
		t.Fatalf("billing spend = %+v", spend)
	}
	if spend.Classification != Estimate {
		t.Fatalf("session-cost spend classification = %v", spend.Classification)
	}
	// Estimates never sum with provider-reported totals of the same name.
	pro, _ := findMetric(usage, "usage.input_tokens", "owl-vendor")
	est := pro
	est.Classification = Estimate
	est.Limitations = []string{"Estimated value; not a vendor measurement."}
	if _, err := SumMetrics(pro, est); !errors.Is(err, ErrMixedClassification) {
		t.Fatalf("estimate + provider-reported = %v", err)
	}
}

func TestLLMMixedClassificationFromAdapter(t *testing.T) {
	f := newLLMFixture()
	usage, err := f.adapter.Usage(context.Background(), Account{Provider: "owl", ProviderAccountID: "alice-seat"}, "day")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, usage))
	var reported, estimated *Metric
	for i := range usage {
		m := &usage[i]
		if m.Name != "usage.output_tokens" || m.Value == nil {
			continue
		}
		switch m.Classification {
		case ProviderReported:
			reported = m
		case Estimate:
			estimated = m
		}
	}
	if reported == nil || estimated == nil {
		t.Fatalf("want both classifications of output tokens: %s", mustJSON(t, usage))
	}
	if _, err := SumMetrics(*reported, *estimated); !errors.Is(err, ErrMixedClassification) {
		t.Fatalf("mixed sum = %v, want ErrMixedClassification", err)
	}
}

func TestLLMVerifySelectResources(t *testing.T) {
	f := newLLMFixture()
	ctx := context.Background()
	got, err := f.adapter.Verify(ctx, Account{Provider: "owl", ProviderAccountID: "alice-seat"})
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, got))
	if got.Account.ProviderAccountID != "alice-seat" || len(got.Account.Links.Models) != 2 {
		t.Fatalf("verify = %+v", got.Account)
	}
	if _, err := f.adapter.Verify(ctx, Account{Provider: "owl", ProviderAccountID: "nobody"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("verify unknown = %v", err)
	}
	sel, err := f.adapter.Select(ctx, Account{ID: "acc_alice", Username: "alice", Name: accountstest.CanarySecret}, "connect")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, sel))
	if sel.AccountID != "acc_alice" || sel.CredentialRef != "" {
		t.Fatalf("selection leaks or misroutes: %+v", sel)
	}
	if _, err := f.adapter.Resources(ctx, Account{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("resources = %v, want ErrUnsupported", err)
	}
	for _, r := range f.refresh {
		if r {
			t.Fatal("a read triggered a refresh")
		}
	}
}

func TestLLMRegistry(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(NewLLMAdapter()); err != nil {
		t.Fatal(err)
	}
	a, err := r.Adapter("llm", Usage|Billing)
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, mustJSON(t, a.Name()), mustJSON(t, a.Capabilities()))
	if a.Name() != "llm" || a.Capabilities()&(Discover|Verify|Usage|Billing) == 0 {
		t.Fatalf("bad adapter: %s %d", a.Name(), a.Capabilities())
	}
	if _, err := r.Adapter("llm", Resources); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("resources capability = %v", err)
	}
	if err := r.Register(NewLLMAdapter()); err == nil {
		t.Fatal("double registration accepted")
	}
}
