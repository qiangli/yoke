package llmbudget

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func forecastFixture(now time.Time, ratios []float64) []forecastRecord {
	var rows []forecastRecord
	for i, v := range ratios {
		at := now.Add(-time.Duration(len(ratios)-i) * 7 * 24 * time.Hour)
		rows = append(rows, forecastRecord{Kind: "cycle", Key: recordKey(i), At: at, Account: "plan", Window: "week", CycleStart: at.Add(-7 * 24 * time.Hour), ResetAt: at, TokensPerPercent: v, Tokens: int64(v * 80), Peak: 80})
	}
	rows = append(rows, forecastRecord{Kind: "snapshot", Key: "s", At: now.Add(-time.Minute), Account: "plan", Window: "week", Used: 40, ResetAt: now.Add(24 * time.Hour), Classification: "measured"})
	for i := 0; i < 24; i++ {
		rows = append(rows, forecastRecord{Kind: "usage", Key: recordKey("u", i), At: now.Add(-time.Duration(i) * time.Hour), Account: "plan", Agent: "agent-a", Tool: "claude", Input: 900, Output: 100})
	}
	return rows
}
func TestForecastLearningBurnAndWorkers(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	one := calculateForecast(forecastFixture(now, []float64{100}), now)[0]
	if one.Verdict != "learning" || one.P50 == nil || one.P90.Before(*one.P50) || one.Classification != "predicted" {
		t.Fatalf("learning forecast: %+v", one)
	}
	rows := forecastFixture(now, []float64{100, 100, 100})
	f := calculateForecast(rows, now)[0]
	if f.Burn3H <= 0 || f.Burn24H <= 0 || f.P50 == nil || !strings.HasPrefix(f.Verdict, "exhausts-before-reset") {
		t.Fatalf("burn forecast: %+v", f)
	}
	n := 8
	q := []QuotaRow{{Account: "plan", Window: "week", SuggestedWorkers: &n, Classification: "measured"}}
	applyForecast(q, []AccountForecast{f}, now)
	if q[0].SuggestedWorkers == nil || *q[0].SuggestedWorkers <= n || q[0].Classification != "measured" {
		t.Fatalf("workers: %+v", q[0])
	}
}
func TestForecastLearningForObservedAndMeasuredAccounts(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	used, balance := 25.0, 10.0
	rows := []QuotaRow{
		{Account: "plan", Window: "week", UsedPercent: &used, ObservedAt: &now, Classification: "observed"},
		{Account: "key", Balance: &balance, ObservedAt: &now, Classification: "measured"},
		{Account: "empty", Status: "unmeasured"},
	}
	applyForecast(rows, nil, now)
	if rows[0].Predict != "learning" || rows[1].Predict != "learning" || rows[2].Predict != "" {
		t.Fatalf("learning/default: %+v", rows)
	}
	forecasts := []AccountForecast{{Account: "plan", Window: "week", Verdict: "ok-until-reset"}, {Account: "key", Window: "", Verdict: "learning"}}
	applyForecast(rows, forecasts, now)
	if rows[0].Predict != "ok-until-reset" || rows[1].Predict != "learning" {
		t.Fatalf("plan/key attachment: %+v", rows)
	}
}
func TestForecastConvergenceProfileAndRegime(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	noisy := calculateForecast(forecastFixture(now, []float64{50, 150}), now)[0]
	stable := calculateForecast(forecastFixture(now, []float64{50, 150, 98, 99, 100, 101, 100, 100}), now)[0]
	if !(stable.CoefficientOfVariation < noisy.CoefficientOfVariation) {
		t.Fatalf("CV %v >= %v", stable.CoefficientOfVariation, noisy.CoefficientOfVariation)
	}
	rows := forecastFixture(now, []float64{100, 100, 100, 100})
	base := calculateForecast(rows, now)[0]
	for i := 1; i <= 20; i++ {
		rows = append(rows, forecastRecord{Kind: "usage", Key: recordKey("profile", i), At: now.Add(-time.Duration(i*7*24) * time.Hour), Account: "plan", Agent: "agent-a", Tool: "claude", Input: 5000})
	}
	prof := calculateForecast(rows, now)[0]
	if prof.BurnRate <= base.BurnRate {
		t.Fatalf("profile did not raise burn: %v <= %v", prof.BurnRate, base.BurnRate)
	}
	jump := forecastFixture(now, []float64{100, 100, 220})
	regimes := deriveRegimes(jump)
	if len(regimes) == 0 {
		t.Fatal("2x jump undetected")
	}
	jump = append(jump, regimes...)
	f := calculateForecast(jump, now)[0]
	if f.Verdict != "relearning" || f.Confidence >= base.Confidence || f.TokensPerPercent < 200 {
		t.Fatalf("regime: %+v", f)
	}
}
func TestForecastPlanAndContradiction(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(24 * time.Hour)
	rows := []forecastRecord{{Kind: "snapshot", At: now.Add(-2 * time.Hour), Account: "plan", Window: "week", Used: 10, ResetAt: reset, Classification: "measured", PlanDigest: "a"}, {Kind: "prediction", At: now.Add(-time.Hour), Account: "plan", Window: "week", ResetAt: reset, Verdict: "ok-until-reset"}, {Kind: "snapshot", At: now, Account: "plan", Window: "week", Used: 100, ResetAt: reset, Classification: "measured", PlanDigest: "b"}}
	regimes := deriveRegimes(rows)
	reasons := map[string]bool{}
	for _, r := range regimes {
		reasons[r.Reason] = true
	}
	if !reasons["plan digest changed"] || !reasons["forecast contradicted by vendor"] {
		t.Fatalf("regimes: %+v", regimes)
	}
}

func TestCompletedCycleClosesPreviousWindow(t *testing.T) {
	end := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	start := end.Add(-7 * 24 * time.Hour)
	rows := []forecastRecord{{Kind: "snapshot", Account: "plan", Window: "week", At: end.Add(-time.Hour), Used: 80, ResetAt: end}, {Kind: "snapshot", Account: "plan", Window: "week", At: end.Add(time.Hour), Used: 1, ResetAt: end.Add(7 * 24 * time.Hour)}, {Kind: "usage", Account: "plan", At: start.Add(time.Hour), Input: 8000}, {Kind: "usage", Account: "plan", At: end.Add(time.Hour), Input: 9000}}
	cycles := deriveCycles(rows)
	if len(cycles) != 1 || !cycles[0].ResetAt.Equal(end) || cycles[0].Tokens != 8000 || cycles[0].TokensPerPercent != 100 {
		t.Fatalf("completed cycle: %+v", cycles)
	}
}

func TestForecastCompactionKeepsHourlyHistory(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	at := now.Add(-8 * 24 * time.Hour)
	path := filepath.Join(t.TempDir(), "history.jsonl")
	rows := []forecastRecord{{Kind: "usage", Key: "one", At: at, Account: "plan", Agent: "a", Tool: "codex", Input: 100}, {Kind: "usage", Key: "two", At: at.Add(time.Minute), Account: "plan", Agent: "a", Tool: "codex", Output: 50}}
	if e := compactForecast(path, rows, now); e != nil {
		t.Fatal(e)
	}
	got := loadForecast(path)
	if len(got) != 1 || got[0].Kind != "usage_hour" || got[0].Input+got[0].Output != 150 {
		t.Fatalf("hourly history: %+v", got)
	}
	st, e := os.Stat(path)
	// Windows has no POSIX permission bits to assert.
	if runtime.GOOS != "windows" && (e != nil || st.Mode().Perm() != 0600) {
		t.Fatalf("history mode: %v %v", st, e)
	}
}
func TestForecastIngestIdempotentAndOTelFailOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_OTEL_QUERY_URL", "http://127.0.0.1:1")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	used := 20.0
	report := &Report{Accounts: []AccountReport{{Account: "plan", Metrics: []Metric{{Name: "quota.used_percent", Value: &used, Window: "5h", ObservedAt: now, ResetAt: &reset, Classification: "measured", Source: "fixture"}}}}}
	if e := ingestReport(context.Background(), now, report); e != nil {
		t.Fatal(e)
	}
	a := loadForecast(forecastPath())
	if e := ingestReport(context.Background(), now, report); e != nil {
		t.Fatal(e)
	}
	b := loadForecast(forecastPath())
	if len(a) != len(b) {
		t.Fatalf("duplicate rows %d -> %d", len(a), len(b))
	}
	if len(TokenUsage(context.Background(), now.Add(-time.Hour), now)) != 0 {
		t.Fatal("unreachable OTel emitted usage")
	}
	// Local copies of the same request are collapsed before they enter history.
	_ = os.MkdirAll(filepath.Join(home, "cligw"), 0700)
	_ = os.MkdirAll(filepath.Join(home, "broker"), 0700)
	payload := map[string]any{"schema_version": "bashy-cligw-usage-v1", "at": now, "agent": "codex-gpt6-sol", "model": "gpt6-sol", "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 50}}
	line, _ := json.Marshal(payload)
	_ = os.WriteFile(filepath.Join(home, "cligw", "usage.jsonl"), append(line, '\n'), 0600)
	broker := map[string]any{"time": now, "principal": "codex-gpt6-sol", "model": "gpt6-sol", "prompt_tokens": 100, "completion_tokens": 50}
	line, _ = json.Marshal(broker)
	_ = os.WriteFile(filepath.Join(home, "broker", "audit.jsonl"), append(line, '\n'), 0600)
	samples := TokenUsage(context.Background(), now.Add(-time.Hour), now.Add(time.Hour))
	if len(samples) != 1 {
		t.Fatalf("dedupe: %+v", samples)
	}
	if math.Abs(float64(samples[0].Input-100)) > 0 {
		t.Fatalf("usage: %+v", samples)
	}
}

func TestTokenUsageWeaveQueue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_OTEL_QUERY_URL", "http://127.0.0.1:1")
	now := time.Now().UTC()
	dir := filepath.Join(home, "weave", "fixture")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	log := filepath.Join(dir, "run.log")
	if e := os.WriteFile(log, []byte("[turn completed input=120 cached=20 output=30]\n"), 0600); e != nil {
		t.Fatal(e)
	}
	queue := map[string]any{"items": []any{map[string]any{"log_path": log, "owner": "worker-1", "coding_agent": "codex-gpt6-sol", "tool": "codex"}}}
	b, _ := json.MarshalIndent(queue, "", "  ")
	if e := os.WriteFile(filepath.Join(dir, "queue.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	samples := TokenUsage(context.Background(), now.Add(-time.Hour), now.Add(time.Hour))
	if len(samples) != 1 || samples[0].Account != "chatgpt-pro" || samples[0].Input != 120 || samples[0].Cached != 20 {
		t.Fatalf("weave usage: %+v", samples)
	}
}

func TestTokenUsageOTelSpan(t *testing.T) {
	now := time.Now().UTC()
	t.Setenv("BASHY_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/traces/select/logsql/query" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"_time": now.Format(time.RFC3339Nano), "span_attr:gen_ai.request.model": "gpt-6-sol", "span_attr:agent.principal": "codex-gpt6-sol", "span_attr:gen_ai.usage.input_tokens": "250", "span_attr:gen_ai.usage.output_tokens": "50"}})
	}))
	defer server.Close()
	t.Setenv("BASHY_OTEL_QUERY_URL", server.URL)
	samples := TokenUsage(context.Background(), now.Add(-time.Hour), now.Add(time.Hour))
	if len(samples) != 1 || samples[0].Account != "chatgpt-pro" || samples[0].Input != 250 {
		t.Fatalf("OTel span: %+v", samples)
	}
}
