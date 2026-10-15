// Package probeseeds is a test-only package: it runs the baseline usage-probe
// seeds through the REAL engine — the dag-registered Bash# snippet runner and
// the in-process userland (cmds/all), not a fake — end to end into
// llmbudget.Quota. It exists as its own package because llmbudget's internal
// tests nil the snippet runner, and because llmbudget cannot import pkg/dag
// (dag imports llmbudget).
package probeseeds

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/qiangli/yoke/cmds/all" // in-process cat, ls, grep, tail, jq
	_ "github.com/qiangli/yoke/pkg/dag"  // registers the real Bash# snippet runner
	"github.com/qiangli/yoke/pkg/llmbudget"
)

var rodNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// isolate clears the ambient bashy environment and pins HOME to a scratch
// dir, so probes read fixtures, never this host's real captures.
func isolate(t *testing.T) string {
	t.Helper()
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "BASHY_") {
			t.Setenv(k, "")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// rodFleet writes one model YAML into a scratch BASHY_FLEET_DIR and turns the
// embedded baseline seeds off, so Quota synthesizes exactly this model's
// probes (baseline plan probes attach through the model's plan field).
func rodFleet(t *testing.T, name, yaml string) {
	t.Helper()
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	root := t.TempDir()
	t.Setenv("BASHY_FLEET_DIR", root)
	if err := os.MkdirAll(filepath.Join(root, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "models", name+".yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rodQuota(t *testing.T, consent ...string) []llmbudget.QuotaRow {
	t.Helper()
	g := llmbudget.New(llmbudget.Config{Policy: &llmbudget.Policy{Version: 1, ProbeConsent: consent}, Now: func() time.Time { return rodNow }})
	rows, err := g.Quota(context.Background(), llmbudget.QuotaOptions{Now: rodNow})
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	return rows
}

func findRow(t *testing.T, rows []llmbudget.QuotaRow, provider, window string) llmbudget.QuotaRow {
	t.Helper()
	for _, r := range rows {
		if r.Provider == provider && r.Window == window {
			return r
		}
	}
	t.Fatalf("no %s/%s row in %+v", provider, window, rows)
	return llmbudget.QuotaRow{}
}

// Rod: a temp BASHY_FLEET_DIR model whose usage probe (effects [pure]) echoes
// a bashy-usage-v1 JSON surfaces as a measured quota row.
func TestRodEchoProbeReachesQuota(t *testing.T) {
	isolate(t)
	t.Setenv("RODVENDOR_API_KEY", "fixture-key")
	rodFleet(t, "rod-echo", `name: rod-echo
display: Rod Echo
kind: api
provider: rodvendor
api_key_ref: rodvendor
usage:
  - id: pulse
    run:
      body: echo '{"schema_version":"bashy-usage-v1","metrics":[{"name":"used_percent","value":25,"unit":"percent","window":"5h","reset_at":1792685131}]}'
      effects: [pure]
`)
	rows := rodQuota(t)
	r := findRow(t, rows, "rodvendor", "5h")
	if r.UsedPercent == nil || *r.UsedPercent != 25 || r.Status != "ok" || r.Classification != "measured" {
		t.Fatalf("row: %+v", r)
	}
	if r.ResetAt == nil || r.ResetAt.Unix() != 1792685131 {
		t.Fatalf("reset: %+v", r)
	}
	if r.Headroom == nil || *r.Headroom != 0.75 {
		t.Fatalf("headroom: %+v", r)
	}
	if r.Source != "probe:model:rod-echo/pulse" {
		t.Fatalf("source: %q", r.Source)
	}
}

// Rod: the same pipeline through a nested ~~~py fence inside the Bash# body
// (an external python3 run under the exec effect, consented like any exec).
func TestRodPyFenceProbeReachesQuota(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	isolate(t)
	t.Setenv("RODVENDOR_API_KEY", "fixture-key")
	rodFleet(t, "rod-py", `name: rod-py
display: Rod Py
kind: api
provider: rodvendor
api_key_ref: rodvendor
usage:
  - id: pulse
    run:
      body: |
        ~~~py as py
        def main() -> str:
            return '{"schema_version":"bashy-usage-v1","metrics":[{"name":"used_percent","value":40,"unit":"percent","window":"week"}]}'
        ~~~
        value := py.main()
        echo "$value"
      effects: [pure, exec]
`)
	rows := rodQuota(t, "probe:model:rod-py/pulse")
	r := findRow(t, rows, "rodvendor", "week")
	if r.UsedPercent == nil || *r.UsedPercent != 40 || r.Status != "ok" || r.Classification != "measured" {
		t.Fatalf("row: %+v", r)
	}
}

const claudeFixture = `{"schema_version":"bashy-claude-statusline-v1","observed_at":"2026-10-15T11:58:00Z","account":"claude-max","payload":{"rate_limits":{"five_hour":{"used_percentage":42.5,"resets_at":1760539200},"seven_day":{"used_percentage":11,"resets_at":1761057600}}}}`

// Rod: the compiled-in claude-max statusline probe against a temp HOME with a
// fixture capture file yields the 5h and week rows. The plan probe
// synthesizes from the baseline models seated on claude-max (seeds stay on;
// HOME is what isolates the read).
func TestRodClaudeMaxProbeTempHome(t *testing.T) {
	home := isolate(t)
	if err := os.MkdirAll(filepath.Join(home, ".bashy", "quota"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bashy", "quota", "claude.json"), []byte(claudeFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := rodQuota(t)
	five := findRow(t, rows, "anthropic", "5h")
	if five.UsedPercent == nil || *five.UsedPercent != 42.5 || five.ResetAt == nil || five.ResetAt.Unix() != 1760539200 {
		t.Fatalf("5h row: %+v", five)
	}
	if five.Source != "probe:plan:claude-max/statusline" {
		t.Fatalf("source: %q", five.Source)
	}
	week := findRow(t, rows, "anthropic", "week")
	if week.UsedPercent == nil || *week.UsedPercent != 11 || week.ResetAt == nil || week.ResetAt.Unix() != 1761057600 {
		t.Fatalf("week row: %+v", week)
	}
}

// Rod: the compiled-in chatgpt-pro sessions probe against a temp HOME holding
// the committed REDACTED rollout fixture. The newest rollout wins: an older
// file carrying a 5h window is present and must NOT contribute a row — on
// this Pro seat the only window is the weekly one (window_minutes 10080).
func TestRodChatGPTProProbeRedactedFixture(t *testing.T) {
	home := isolate(t)
	fixture, err := os.ReadFile("testdata/rollout-redacted.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(home, ".codex", "sessions", "2026", "10", "15")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	newest := filepath.Join(sessions, "rollout-2026-10-15T16-43-53-fixture.jsonl")
	if err := os.WriteFile(newest, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(sessions, "rollout-2026-10-15T09-00-00-fixture.jsonl")
	stale := `{"timestamp":"2026-10-15T09:00:00.000Z","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":90,"window_minutes":300,"resets_at":1792680000},"secondary":null}}}` + "\n"
	if err := os.WriteFile(older, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	past := rodNow.Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	rows := rodQuota(t)
	week := findRow(t, rows, "openai", "week")
	if week.UsedPercent == nil || *week.UsedPercent != 1 || week.ResetAt == nil || week.ResetAt.Unix() != 1792685131 {
		t.Fatalf("week row: %+v", week)
	}
	if week.Source != "probe:plan:chatgpt-pro/sessions" {
		t.Fatalf("source: %q", week.Source)
	}
	for _, r := range rows {
		if r.Provider == "openai" && r.Window == "5h" {
			t.Fatalf("older rollout leaked a 5h row: %+v", r)
		}
	}
}
