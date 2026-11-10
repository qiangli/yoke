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
		{Principal: "owner", Requests: 2, PromptTokens: 50, CompletionTokens: 10, CachedTokens: 12, CostUSD: 0.0625},
		{Principal: "peer:alpha", Requests: 2, PromptTokens: 80, CompletionTokens: 16, CachedTokens: 18, CostUSD: 0.075, Unpriced: true},
		{Principal: "peer:beta", Requests: 1, PromptTokens: 20, CompletionTokens: 4, CachedTokens: 5, CostUSD: 0.025},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rollup = %#v, want %#v", rows, want)
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
	want := []PrincipalUsage{{Principal: "peer:beta", Requests: 1, PromptTokens: 20, CompletionTokens: 4, CachedTokens: 5, CostUSD: 0.025}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rollup = %#v, want %#v", rows, want)
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
	want := []PrincipalUsage{{Principal: "peer:beta", Requests: 1, PromptTokens: 20, CompletionTokens: 4, CachedTokens: 5, CostUSD: 0.025}}
	if got.Schema != UsageSchemaVersion || !got.Since.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) || !reflect.DeepEqual(got.Principals, want) {
		t.Fatalf("JSON report = %#v, want schema %q, since 2026-11-03 and %#v", got, UsageSchemaVersion, want)
	}
}
