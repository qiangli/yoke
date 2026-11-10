package broker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRollupUsageFixture(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "audit_usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rows, err := RollupUsage(f, UsageRollupOptions{Since: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	want := []PrincipalUsage{
		{Principal: "owner", Requests: 2, PromptTokens: 50, CompletionTokens: 10, CachedTokens: 12, CostUSD: 0.0625, PricedTokens: 60},
		{Principal: "peer:alpha", Requests: 2, PromptTokens: 80, CompletionTokens: 16, CachedTokens: 18, CostUSD: 0.075, Unpriced: true, PricedTokens: 60, UnpricedTokens: 36},
		{Principal: "peer:beta", Requests: 1, PromptTokens: 20, CompletionTokens: 4, CachedTokens: 5, CostUSD: 0.025, PricedTokens: 24},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rollup = %#v, want %#v", rows, want)
	}
}

func TestRollupUsageCacheWrites(t *testing.T) {
	rows, err := RollupUsage(strings.NewReader("{\"time\":\"2026-11-10T00:00:00Z\",\"principal\":\"owner\",\"prompt_tokens\":15,\"cached_tokens\":7,\"cache_write_tokens\":5}\n"), UsageRollupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PromptTokens != 15 || rows[0].CachedTokens != 7 || rows[0].CacheWriteTokens != 5 {
		t.Fatalf("rollup = %+v", rows)
	}
}

func TestRollupUsagePrincipalFilter(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "audit_usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rows, err := RollupUsage(f, UsageRollupOptions{Principal: "peer:beta"})
	if err != nil {
		t.Fatal(err)
	}
	want := []PrincipalUsage{{Principal: "peer:beta", Requests: 1, PromptTokens: 20, CompletionTokens: 4, CachedTokens: 5, CostUSD: 0.025, PricedTokens: 24}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rollup = %#v, want %#v", rows, want)
	}
}

// A principal mixing priced and unpriced models keeps the OR-ed unpriced flag
// (any unpriced record) and splits its tokens into the priced and unpriced
// totals, which is what the owner alarm needs to judge the priced spend alone.
func TestRollupUsageSplitsPricedAndUnpricedTokens(t *testing.T) {
	log := strings.Join([]string{
		`{"time":"2026-11-09T10:00:00Z","principal":"peer:mixed","prompt_tokens":100,"completion_tokens":20,"cost_usd":0.01}`,
		`{"time":"2026-11-09T11:00:00Z","principal":"peer:mixed","prompt_tokens":7,"completion_tokens":3,"unpriced":true}`,
		`{"time":"2026-11-09T12:00:00Z","principal":"peer:mixed","prompt_tokens":50,"completion_tokens":10,"cost_usd":0.005}`,
		`{"time":"2026-11-09T13:00:00Z","principal":"peer:free","prompt_tokens":9,"completion_tokens":1,"unpriced":true}`,
		``,
	}, "\n")
	rows, err := RollupUsage(strings.NewReader(log), UsageRollupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rollup = %#v, want 2 principals", rows)
	}
	for _, row := range rows {
		var wantPriced, wantUnpriced uint64
		switch row.Principal {
		case "peer:free":
			wantPriced, wantUnpriced = 0, 10
		case "peer:mixed":
			wantPriced, wantUnpriced = 180, 10
		default:
			t.Fatalf("unexpected principal %q", row.Principal)
		}
		if row.PricedTokens != wantPriced || row.UnpricedTokens != wantUnpriced {
			t.Errorf("%s priced/unpriced tokens = %d/%d, want %d/%d", row.Principal, row.PricedTokens, row.UnpricedTokens, wantPriced, wantUnpriced)
		}
		if !row.Unpriced {
			t.Errorf("%s unpriced flag lost", row.Principal)
		}
		if got := row.PromptTokens + row.CompletionTokens; got != row.PricedTokens+row.UnpricedTokens {
			t.Errorf("%s priced+unpriced = %d, want the prompt+completion total %d", row.Principal, row.PricedTokens+row.UnpricedTokens, got)
		}
	}
	mixed := rows[1]
	if mixed.Principal != "peer:mixed" || mixed.Requests != 3 || mixed.PromptTokens != 157 || mixed.CompletionTokens != 33 || mixed.CostUSD != 0.015 {
		t.Errorf("peer:mixed row = %+v, want 3 requests, 157 prompt, 33 completion, $0.015", mixed)
	}
}

func TestParseUsageWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1.5d": 36 * time.Hour, "24h": 24 * time.Hour, "0": 0} {
		got, err := parseUsageWindow(in)
		if err != nil || got != want {
			t.Errorf("parseUsageWindow(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseUsageWindow("-1d"); err == nil {
		t.Fatal("negative window accepted")
	}
}

func TestUsageCommandTextAndJSON(t *testing.T) {
	oldPath, oldNow := usageAuditPath, usageNow
	fixture, err := filepath.Abs(filepath.Join("testdata", "audit_usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	usageAuditPath = func() (string, error) { return fixture, nil }
	usageNow = func() time.Time { return time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { usageAuditPath, usageNow = oldPath, oldNow })

	var text bytes.Buffer
	cmd := newUsageCmd()
	cmd.SetOut(&text)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PRINCIPAL", "owner", "peer:alpha", "0.062500", "true"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text output missing %q:\n%s", want, text.String())
		}
	}

	var raw bytes.Buffer
	cmd = newUsageCmd()
	cmd.SetOut(&raw)
	cmd.SetArgs([]string{"--json", "--peer", "beta"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got usageReport
	if err := json.Unmarshal(raw.Bytes(), &got); err != nil {
		t.Fatalf("JSON output: %v\n%s", err, raw.String())
	}
	want := []PrincipalUsage{{Principal: "peer:beta", Requests: 1, PromptTokens: 20, CompletionTokens: 4, CachedTokens: 5, CostUSD: 0.025, PricedTokens: 24}}
	if got.Schema != UsageSchemaVersion || !got.Since.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) || !reflect.DeepEqual(got.Principals, want) {
		t.Fatalf("JSON report = %#v, want schema %q, since 2026-11-03 and %#v", got, UsageSchemaVersion, want)
	}
}

// The --json envelope is additive only: priced_tokens and unpriced_tokens are
// new keys on each principal row, the schema stays v1 and every pre-existing
// key is still there with its meaning unchanged.
func TestUsageCommandJSONCarriesSplitTokenFields(t *testing.T) {
	oldPath, oldNow := usageAuditPath, usageNow
	fixture, err := filepath.Abs(filepath.Join("testdata", "audit_usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	usageAuditPath = func() (string, error) { return fixture, nil }
	usageNow = func() time.Time { return time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { usageAuditPath, usageNow = oldPath, oldNow })

	var raw bytes.Buffer
	cmd := newUsageCmd()
	cmd.SetOut(&raw)
	cmd.SetArgs([]string{"--json", "--peer", "alpha"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Schema     string `json:"schema"`
		Since      string `json:"since"`
		Principals []struct {
			Principal        string  `json:"principal"`
			Requests         uint64  `json:"requests"`
			PromptTokens     uint64  `json:"prompt_tokens"`
			CompletionTokens uint64  `json:"completion_tokens"`
			CachedTokens     uint64  `json:"cached_tokens"`
			CostUSD          float64 `json:"cost_usd"`
			Unpriced         bool    `json:"unpriced"`
			PricedTokens     uint64  `json:"priced_tokens"`
			UnpricedTokens   uint64  `json:"unpriced_tokens"`
		} `json:"principals"`
	}
	if err := json.Unmarshal(raw.Bytes(), &rep); err != nil {
		t.Fatalf("JSON output: %v\n%s", err, raw.String())
	}
	if rep.Schema != UsageSchemaVersion {
		t.Fatalf("schema = %q, want %q", rep.Schema, UsageSchemaVersion)
	}
	if len(rep.Principals) != 1 {
		t.Fatalf("principals = %#v, want the single peer:alpha row", rep.Principals)
	}
	row := rep.Principals[0]
	if row.Principal != "peer:alpha" || row.Requests != 2 || row.PromptTokens != 80 || row.CompletionTokens != 16 || row.CachedTokens != 18 || row.CostUSD != 0.075 || !row.Unpriced {
		t.Fatalf("peer:alpha row = %+v, want the fixture totals with the OR-ed unpriced flag", row)
	}
	if row.PricedTokens != 60 || row.UnpricedTokens != 36 {
		t.Fatalf("peer:alpha priced/unpriced tokens = %d/%d, want 60/36 (priced 50+10, unpriced 30+6)", row.PricedTokens, row.UnpricedTokens)
	}
}
