package llmbudget

import (
	"context"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/snippet"
)

// seedProbe loads a record's first usage probe from the compiled-in baseline.
func seedProbe(t *testing.T, pick func(c *fleet.Catalog) fleet.UsageProbe) fleet.UsageProbe {
	t.Helper()
	isolateReportCatalog(t)
	c := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
	p := pick(c)
	if p.ID == "" {
		t.Fatal("seed probe missing")
	}
	return p
}

func collectProbe(t *testing.T, probe fleet.UsageProbe, stdout string) SourceResult {
	t.Helper()
	snippet.SetRunner(func(_ context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		return snippet.Result{Stdout: []byte(stdout)}, nil
	})
	t.Cleanup(func() { snippet.SetRunner(nil) })
	c := SourceConfig{ID: "probe:model:x/" + probe.ID, Kind: "probe", Provider: "x", Account: "x", Pool: "x", Lane: LaneAPIKey, Enabled: true, probe: &probe}
	r, err := (ProbeAdapter{}).Collect(context.Background(), c, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return r
}

// The claude-max seed maps the bridge snapshot payload (field names shared
// with the claude-statusline adapter) onto 5h and week rows; resets_at is an
// epoch and survives as a reset time.
func TestSeedClaudeMaxMetrics(t *testing.T) {
	probe := seedProbe(t, func(c *fleet.Catalog) fleet.UsageProbe {
		plan, _ := c.Plan("claude-max")
		if len(plan.Usage) == 0 {
			return fleet.UsageProbe{}
		}
		return plan.Usage[0]
	})
	snapshot := `{"schema_version":"bashy-claude-statusline-v1","observed_at":"2026-10-15T11:58:00Z","account":"claude-max","payload":{"rate_limits":{"five_hour":{"used_percentage":42.5,"resets_at":1760539200},"seven_day":{"used_percentage":11,"resets_at":1761057600}}}}`
	r := collectProbe(t, probe, snapshot)
	if r.Status != "ok" || len(r.Metrics) != 2 {
		t.Fatalf("claude-max collect: %+v", r)
	}
	for _, m := range r.Metrics {
		if m.Name != "quota.used_percent" || m.Unit != "percent" || m.Classification != "measured" {
			t.Fatalf("metric: %+v", m)
		}
		switch m.Window {
		case "5h":
			if *m.Value != 42.5 || m.ResetAt == nil || m.ResetAt.Unix() != 1760539200 {
				t.Fatalf("5h row: %+v", m)
			}
		case "week":
			if *m.Value != 11 || m.ResetAt == nil || m.ResetAt.Unix() != 1761057600 {
				t.Fatalf("week row: %+v", m)
			}
		default:
			t.Fatalf("unexpected window %q: %+v", m.Window, m)
		}
	}
}

// The chatgpt-pro seed self-describes (bashy-usage-v1) because Pro currently
// has no five-hour window: the redacted real line (secondary null, primary
// weekly per window_minutes) maps to exactly one week row, with the epoch
// reset carried through. The real body+fixture pipeline runs in
// pkg/llmbudget/probeseeds; this pins the adapter side of that shape.
func TestSeedChatGPTProSessions(t *testing.T) {
	probe := seedProbe(t, func(c *fleet.Catalog) fleet.UsageProbe {
		plan, _ := c.Plan("chatgpt-pro")
		if len(plan.Usage) == 0 {
			return fleet.UsageProbe{}
		}
		return plan.Usage[0]
	})
	emitted := `{"schema_version":"bashy-usage-v1","metrics":[{"name":"used_percent","value":1,"unit":"percent","window":"week","reset_at":1792685131}]}`
	r := collectProbe(t, probe, emitted)
	if r.Status != "ok" || len(r.Metrics) != 1 {
		t.Fatalf("chatgpt-pro collect: %+v", r)
	}
	m := r.Metrics[0]
	if m.Name != "quota.used_percent" || m.Window != "week" || *m.Value != 1 || m.ResetAt == nil || m.ResetAt.Unix() != 1792685131 {
		t.Fatalf("week metric: %+v", m)
	}
}

// The deepseek and kimi balance seeds map the DOCUMENTED sample responses
// (api-docs.deepseek.com Get User Balance; platform.kimi.ai Check Balance,
// both read 2026-10-15). fetch has no test transport, so the mapping is
// validated against those samples; no network is touched.
func TestSeedBalanceMetricsAgainstDocs(t *testing.T) {
	// The seeds resolve the bearer key through model:credential; this test is
	// about the documented response mapping, so the same KEY name is fed from
	// a fixture variable (the credential path has its own test).
	t.Setenv("SEED_BALANCE_KEY", "fixture-token")
	subs := func(p fleet.UsageProbe) fleet.UsageProbe {
		p.Run.Env = map[string]string{"KEY": "env:SEED_BALANCE_KEY"}
		return p
	}
	deepseek := subs(seedProbe(t, func(c *fleet.Catalog) fleet.UsageProbe {
		m, _ := c.Model("deepseek-v4-pro")
		if len(m.Usage) == 0 {
			return fleet.UsageProbe{}
		}
		return m.Usage[0]
	}))
	r := collectProbe(t, deepseek, `{"is_available": true, "balance_infos": [ {"currency": "CNY", "total_balance": "110.00", "granted_balance": "10.00", "topped_up_balance": "100.00"} ]}`)
	if r.Status != "ok" || len(r.Metrics) != 1 {
		t.Fatalf("deepseek collect: %+v", r)
	}
	if m := r.Metrics[0]; m.Name != "usage.balance" || *m.Value != 110 || m.Unit != "CNY" || m.Window != "" || m.ResetAt != nil {
		t.Fatalf("deepseek metric: %+v", m)
	}

	kimi := subs(seedProbe(t, func(c *fleet.Catalog) fleet.UsageProbe {
		m, _ := c.Model("kimi-k3")
		if len(m.Usage) == 0 {
			return fleet.UsageProbe{}
		}
		return m.Usage[0]
	}))
	r = collectProbe(t, kimi, `{"code": 0, "data": {"available_balance": 49.58894, "voucher_balance": 46.58893, "cash_balance": 3.00001}, "scode": "0x0", "status": true}`)
	if r.Status != "ok" || len(r.Metrics) != 1 {
		t.Fatalf("kimi collect: %+v", r)
	}
	if m := r.Metrics[0]; m.Name != "usage.balance" || *m.Value != 49.58894 || m.Unit != "USD" {
		t.Fatalf("kimi metric: %+v", m)
	}
}
