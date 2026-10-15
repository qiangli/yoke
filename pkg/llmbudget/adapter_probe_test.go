package llmbudget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/snippet"
)

func TestProbeAdapterMappingAndSelfEmission(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	snippet.SetRunner(func(_ context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		return snippet.Result{Stdout: []byte(`{"usage":{"pct":97,"unit":"percent","reset":1792065600000}}`)}, nil
	})
	defer snippet.SetRunner(nil)
	probe := fleet.UsageProbe{ID: "q", Run: snippet.Snippet{Body: "echo fixture"}, Metrics: []fleet.UsageMetricMap{{Name: "used_percent", Value: ".usage.pct", Unit: ".usage.unit", Window: "5h", Reset: ".usage.reset"}}}
	c := SourceConfig{ID: "probe:plan:p/q", Kind: "probe", Provider: "vendor", Account: "p", Pool: "p", Lane: LaneSubscription, Enabled: true, probe: &probe}
	r, e := (ProbeAdapter{}).Collect(context.Background(), c, now)
	if e != nil || len(r.Metrics) != 1 {
		t.Fatalf("collect: %+v %v", r, e)
	}
	m := r.Metrics[0]
	if m.Name != "quota.used_percent" || *m.Value != 97 || m.Unit != "percent" || m.Window != "5h" || m.ResetAt == nil || m.ResetAt.Unix() != 1792065600 {
		t.Fatalf("mapping: %+v", m)
	}
	g := New(Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Policy: &Policy{Version: 1, Sources: []SourceConfig{c}}, Now: func() time.Time { return now }})
	if _, e := g.CollectReport(context.Background(), ReportOptions{Now: now}); e != nil {
		t.Fatal(e)
	}
	h, known, e := g.SubscriptionHeadroom(context.Background(), Binding{Provider: "vendor", Account: "p", Pool: "p", Lane: LaneSubscription, AccountKnown: true})
	if e != nil || !known || math.Abs(h-.03) > 1e-9 {
		t.Fatalf("headroom %.9f %v %v", h, known, e)
	}
	snippet.SetRunner(func(_ context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		return snippet.Result{Stdout: []byte(`{"schema_version":"bashy-usage-v1","metrics":[{"name":"balance","value":12,"unit":"credits","window":"day","reset_at":"2026-10-16T00:00:00Z"}]}`)}, nil
	})
	probe.Metrics = nil
	r, e = (ProbeAdapter{}).Collect(context.Background(), c, now)
	if e != nil || len(r.Metrics) != 1 || r.Metrics[0].Name != "usage.balance" || r.Metrics[0].ResetAt == nil {
		t.Fatalf("self emission: %+v %v", r, e)
	}
}

func TestProbeConsentAndPlanDedupe(t *testing.T) {
	isolateReportCatalog(t)
	planDir := t.TempDir()
	t.Setenv("BASHY_PLANS_DIR", planDir)
	if e := os.WriteFile(filepath.Join(planDir, "consent-plan.yaml"), []byte("name: consent-plan\nvendor: Vendor\nusage:\n- id: quota\n  run:\n    body: 'echo fixture'\n    effects: [net]\n"), 0600); e != nil {
		t.Fatal(e)
	}
	cat := fleet.New()
	for _, name := range []string{"consent-one", "consent-two"} {
		if e := cat.SaveModel(fleet.Model{Name: name, Provider: "vendor", Kind: fleet.ModelKindSubscription, Plan: "consent-plan"}); e != nil {
			t.Fatal(e)
		}
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	g := New(Config{PolicyPath: path})
	p := &Policy{Version: 1}
	sources := synthesizedProbes(p)
	count := 0
	for _, s := range sources {
		if s.ID == "probe:plan:consent-plan/quota" {
			count++
			if s.Enabled || s.Account != "consent-plan" || s.Provider != "vendor" {
				t.Fatalf("disabled source %+v", s)
			}
		}
	}
	if count != 1 {
		t.Fatalf("dedupe count %d", count)
	}
	// An absent explicit policy is invalid, so establish it through the same
	// validated file format used by ConfigurePolicy before toggling consent.
	if e := os.WriteFile(path, []byte(`{"version":1}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := g.EnableProbe("probe:plan:consent-plan/quota"); e != nil {
		t.Fatal(e)
	}
	p, e := g.policy()
	if e != nil {
		t.Fatal(e)
	}
	sources = synthesizedProbes(p)
	enabled := false
	for _, s := range sources {
		if s.ID == "probe:plan:consent-plan/quota" {
			enabled = s.Enabled
		}
	}
	if !enabled {
		t.Fatal("consent did not enable probe")
	}
	if e := g.DisableProbe("probe:plan:consent-plan/quota"); e != nil {
		t.Fatal(e)
	}
	p, e = g.policy()
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range synthesizedProbes(p) {
		if s.ID == "probe:plan:consent-plan/quota" && s.Enabled {
			t.Fatal("probe remained enabled")
		}
	}
}

func TestProbeConsentFollowsSharedCredential(t *testing.T) {
	isolateReportCatalog(t)
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	t.Setenv("SHARED_API_KEY", "fixture-only")
	cat := fleet.New()
	probe := fleet.UsageProbe{ID: "balance", Run: snippet.Snippet{Body: "fixture", Effects: []string{"net"}}}
	for _, name := range []string{"old-model", "new-model"} {
		m := fleet.Model{Name: name, Provider: "fixture", Kind: fleet.ModelKindAPI, APIKeyRef: "shared", Usage: []fleet.UsageProbe{probe}}
		if e := cat.SaveModel(m); e != nil {
			t.Fatal(e)
		}
	}
	p := &Policy{Version: 1, ProbeConsent: []string{"probe:model:old-model/balance"}}
	found := false
	for _, s := range synthesizedProbes(p) {
		if s.ID == "probe:model:new-model/balance" && !s.Enabled {
			t.Fatal("existing account consent was lost")
		}
		found = found || s.ID == "probe:model:new-model/balance"
	}
	if !found {
		t.Fatal("new model probe missing")
	}
}

type slowOverlay struct{}

func (slowOverlay) Kind() string { return "slow-overlay" }
func (slowOverlay) Collect(ctx context.Context, _ SourceConfig, _ time.Time) (SourceResult, error) {
	<-ctx.Done()
	return SourceResult{Status: "unavailable"}, ctx.Err()
}

func TestRefreshProbeSurvivesSlowOverlay(t *testing.T) {
	isolateReportCatalog(t)
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	defer snippet.SetRunner(nil)
	measured := false
	snippet.SetRunner(func(ctx context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		measured = ctx.Err() == nil
		return snippet.Result{Stdout: []byte(`{"schema_version":"bashy-usage-v1","metrics":[{"name":"used_percent","value":25,"window":"5h"}]}`)}, nil
	})
	probe := fleet.UsageProbe{ID: "quota", Run: snippet.Snippet{Body: "fixture", Timeout: time.Second}}
	p := &Policy{Version: 1, Sources: []SourceConfig{
		{ID: "overlay", Kind: "slow-overlay", Provider: "fixture", Account: "seat", Pool: "seat", Lane: LaneSubscription, Enabled: true, TimeoutSeconds: 1},
		{ID: "probe", Kind: "probe", Provider: "fixture", Account: "seat", Pool: "seat", Lane: LaneSubscription, Enabled: true, probe: &probe},
	}}
	g := New(Config{Policy: p, Models: map[string]Model{}, Adapters: []Adapter{slowOverlay{}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r, err := g.CollectReport(ctx, ReportOptions{Refresh: true})
	if err != nil || !measured || len(r.Accounts) != 1 {
		t.Fatalf("probe starved: %+v, measured=%v, err=%v", r, measured, err)
	}
	found := false
	for _, m := range r.Accounts[0].Metrics {
		found = found || m.Name == "quota.used_percent" && m.Value != nil && *m.Value == 25
	}
	if !found {
		t.Fatalf("measurement missing: %+v", r.Accounts[0])
	}
}

func TestModelCredentialProbeGetsValueOnly(t *testing.T) {
	isolateReportCatalog(t)
	t.Setenv("FIXTURE_PROBE_API_KEY", "fixture-token")
	cat := fleet.New()
	if e := cat.SaveModel(fleet.Model{Name: "credential-probe-model", Provider: "fixture", Kind: fleet.ModelKindAPI, APIKeyRef: "fixture-probe"}); e != nil {
		t.Fatal(e)
	}
	snippet.SetRunner(func(_ context.Context, _ snippet.Snippet, io snippet.IO) (snippet.Result, error) {
		found := false
		for _, kv := range io.Env {
			if kv == "PROBE_KEY=fixture-token" {
				found = true
			}
			if kv == "PROBE_KEY=FIXTURE_PROBE_API_KEY=fixture-token" {
				t.Fatal("credential entry passed as value")
			}
		}
		if !found {
			t.Fatal("model credential missing")
		}
		return snippet.Result{Stdout: []byte(`{"schema_version":"bashy-usage-v1","metrics":[]}`)}, nil
	})
	defer snippet.SetRunner(nil)
	probe := fleet.UsageProbe{ID: "key", Run: snippet.Snippet{Body: "echo fixture", Env: map[string]string{"PROBE_KEY": "model:credential"}}}
	c := SourceConfig{ID: "probe:model:credential-probe-model/key", Kind: "probe", Provider: "fixture", Account: "fixture-probe", Pool: "fixture-probe", Lane: LaneAPIKey, Enabled: true, probe: &probe, probeModel: "credential-probe-model"}
	r, e := (ProbeAdapter{}).Collect(context.Background(), c, time.Now())
	if e != nil || r.Status != "ok" {
		t.Fatalf("credential probe %+v %v", r, e)
	}
}

func TestPlanProbeHeadroomCoversEveryModelOnSeat(t *testing.T) {
	isolateReportCatalog(t)
	planDir := t.TempDir()
	t.Setenv("BASHY_PLANS_DIR", planDir)
	if e := os.WriteFile(filepath.Join(planDir, "shared-seat.yaml"), []byte("name: shared-seat\nvendor: Vendor\nusage:\n- id: quota\n  run: 'echo fixture'\n"), 0600); e != nil {
		t.Fatal(e)
	}
	cat := fleet.New()
	for _, name := range []string{"shared-one", "shared-two"} {
		if e := cat.SaveModel(fleet.Model{Name: name, Provider: "vendor", Kind: fleet.ModelKindSubscription, Plan: "shared-seat"}); e != nil {
			t.Fatal(e)
		}
	}
	snippet.SetRunner(func(_ context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		return snippet.Result{Stdout: []byte(`{"schema_version":"bashy-usage-v1","metrics":[{"name":"used_percent","value":97,"unit":"percent","window":"5h"}]}`)}, nil
	})
	defer snippet.SetRunner(nil)
	g := New(Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Policy: &Policy{Version: 1, Bindings: []Binding{{Model: "shared-two", Agent: "shared-agent", Provider: "vendor", Account: "operator-seat", Pool: "operator-seat", Lane: LaneSubscription}}}})
	if _, e := g.CollectReport(context.Background(), ReportOptions{}); e != nil {
		t.Fatal(e)
	}
	h, b, known, e := g.MeasuredHeadroom(context.Background(), "shared-two", "shared-agent")
	if e != nil || !known || math.Abs(h-.03) > 1e-9 || b.Account != "operator-seat" {
		t.Fatalf("shared seat headroom %.9f %+v %v %v", h, b, known, e)
	}
}

func TestProbeStatusesAndInferredHeadroom(t *testing.T) {
	now := time.Now()
	probe := fleet.UsageProbe{ID: "x", Run: snippet.Snippet{Body: "agentic { echo fixture; }", Effects: []string{"spend"}}}
	c := SourceConfig{ID: "probe:plan:p/x", Kind: "probe", Provider: "vendor", Account: "p", Pool: "p", Lane: LaneSubscription, Enabled: true, probe: &probe, probePlan: "p"}
	snippet.SetRunner(func(_ context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		return snippet.Result{Stdout: []byte(`{"schema_version":"bashy-usage-v1","metrics":[{"name":"used_percent","value":97,"unit":"percent"}]}`)}, nil
	})
	defer snippet.SetRunner(nil)
	probeResult, probeErr := (ProbeAdapter{}).Collect(context.Background(), c, now)
	if probeErr != nil || len(probeResult.Metrics) != 1 || probeResult.Metrics[0].Classification != "inferred" {
		t.Fatalf("inferred classification %+v %v", probeResult, probeErr)
	}
	g := New(Config{Policy: &Policy{Version: 1, Sources: []SourceConfig{c}}, Now: func() time.Time { return now }})
	h, known, e := g.SubscriptionHeadroom(context.Background(), Binding{Provider: "vendor", Account: "p", Pool: "p", Lane: LaneSubscription, AccountKnown: true})
	if e != nil || known || h != 0 {
		t.Fatalf("inferred headroom %v %v %v", h, known, e)
	}
	c.Enabled = false
	r, e := g.source(context.Background(), c, now, false)
	if e != nil || r.Status != "disabled" {
		t.Fatalf("consent status %+v %v", r, e)
	}
	c.Enabled = true
	c.probe = nil
	r, e = (ProbeAdapter{}).Collect(context.Background(), c, now)
	if e != nil || r.Status != "not-configured" {
		t.Fatalf("not configured %+v %v", r, e)
	}
	c.probe = &probe
	probe.Run.Env = map[string]string{"API_KEY": "model:credential"}
	c.ID = "probe:model:missing-probe-model/x"
	c.probeModel = "missing-probe-model"
	r, e = (ProbeAdapter{}).Collect(context.Background(), c, now)
	if e != nil || r.Status != "not-configured" {
		t.Fatalf("credential not configured %+v %v", r, e)
	}
	probe.Run.Env = nil
	c.probeModel = ""
	snippet.SetRunner(func(_ context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		return snippet.Result{}, snippet.ErrNeedsInput
	})
	r, e = (ProbeAdapter{}).Collect(context.Background(), c, now)
	if e != nil || r.Status != "needs-input" {
		t.Fatalf("needs input error %+v %v", r, e)
	}
	snippet.SetRunner(func(_ context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		return snippet.Result{Exit: 6}, nil
	})
	r, e = (ProbeAdapter{}).Collect(context.Background(), c, now)
	if e != nil || r.Status != "needs-input" {
		t.Fatalf("needs input %+v %v", r, e)
	}
}

func TestProbeMissingInputStatus(t *testing.T) {
	defer snippet.SetRunner(nil)
	for _, tc := range []struct {
		name   string
		exit   int
		stdout string
		status string
		err    error
	}{
		{"absent", 1, "", "not-configured", nil},
		{"failed", 2, "", "unavailable", nil},
		{"output", 1, "{}", "unavailable", nil},
		{"runtime-error", 1, "", "unavailable", errors.New("runner failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snippet.SetRunner(func(context.Context, snippet.Snippet, snippet.IO) (snippet.Result, error) {
				return snippet.Result{Exit: tc.exit, Stdout: []byte(tc.stdout), Stderr: []byte("no snapshot at /fixture/missing\nextra detail\n")}, tc.err
			})
			c := SourceConfig{probe: &fleet.UsageProbe{Run: snippet.Snippet{Body: "fixture"}}}
			r, _ := (ProbeAdapter{}).Collect(context.Background(), c, time.Now())
			if r.Status != tc.status {
				t.Fatalf("%+v", r)
			}
			if tc.status == "not-configured" && (len(r.Limitations) != 1 || r.Limitations[0] != "no snapshot at /fixture/missing") {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestProbeRefreshRuntimeBudget(t *testing.T) {
	isolateReportCatalog(t)
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	defer snippet.SetRunner(nil)
	for _, tc := range []struct {
		name           string
		declared, want time.Duration
	}{
		{"default", 0, 10 * time.Second},
		{"declared", 20 * time.Second, 20 * time.Second},
		{"fractional", 1500 * time.Millisecond, 1500 * time.Millisecond},
		{"capped", time.Minute, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			called := false
			snippet.SetRunner(func(ctx context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
				called = true
				deadline, ok := ctx.Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining < tc.want-50*time.Millisecond || remaining > tc.want {
					t.Fatalf("runtime %v, want %v", remaining, tc.want)
				}
				<-parent.Done()
				if ctx.Err() != nil {
					t.Fatalf("outer deadline cut probe short: %v", ctx.Err())
				}
				return snippet.Result{Stdout: []byte(`{"schema_version":"bashy-usage-v1","metrics":[]}`)}, nil
			})
			probe := fleet.UsageProbe{Run: snippet.Snippet{Body: "fixture", Timeout: tc.declared}}
			c := SourceConfig{ID: "runtime", Kind: "probe", Provider: "fixture", Account: "fixture", Pool: "fixture", Lane: LaneSubscription, Enabled: true, probe: &probe}
			g := New(Config{Policy: &Policy{Version: 1, Sources: []SourceConfig{c}}, Models: map[string]Model{}})
			r, err := g.CollectReport(parent, ReportOptions{Refresh: true})
			if err != nil || !called || len(r.Accounts) != 1 || r.Accounts[0].Status != "ok" {
				t.Fatalf("report %+v, called %v: %v", r, called, err)
			}
		})
	}
}

func TestProbeSourcePreservesCancellation(t *testing.T) {
	defer snippet.SetRunner(nil)
	for _, refresh := range []bool{false, true} {
		t.Run(fmt.Sprint(refresh), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			snippet.SetRunner(func(callCtx context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
				cancel()
				select {
				case <-callCtx.Done():
					return snippet.Result{}, callCtx.Err()
				case <-time.After(time.Second):
					t.Fatal("explicit cancellation lost")
					return snippet.Result{}, nil
				}
			})
			c := SourceConfig{Kind: "probe", Enabled: true, probe: &fleet.UsageProbe{Run: snippet.Snippet{Body: "fixture"}}}
			_, err := New(Config{}).source(ctx, c, time.Now(), refresh)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		})
	}
}
