package llmbudget

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/quotaobs"
)

type fakeAnalyzer struct {
	calls  int
	answer []byte
}

func (f *fakeAnalyzer) Analyze(_ context.Context, _, _, _ string) ([]byte, error) {
	f.calls++
	return f.answer, nil
}

func analyzerFixture(t *testing.T, enabled bool) (*Gate, quotaobs.Observation, time.Time) {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	o := quotaobs.Observation{At: at, Source: "test", Tool: "codex", Plan: "chatgpt-pro", Account: "chatgpt-pro", Excerpt: "You've hit your usage limit. Try again at 1:00 PM."}
	if err := quotaobs.Record(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	read, err := quotaobs.Read(time.Time{}, nil)
	if err != nil || len(read) != 1 {
		t.Fatalf("read: %v, %d", err, len(read))
	}
	g := New(Config{Policy: &Policy{Version: 1, Analyzer: AnalyzerPolicy{Model: "test-analyzer", Enabled: enabled}}})
	return g, read[0], at
}

func analyzerAnswer(o quotaobs.Observation, kind string, reset string) []byte {
	item := map[string]any{"excerpt_sha": o.ExcerptSHA, "kind": kind, "window": "5h", "used_percent": 100.0, "reset_at": reset, "account_hint": o.Account, "confidence": .95, "summary": "Quota reached"}
	b, _ := json.Marshal(map[string]any{"analyses": []any{item}, "account_summaries": map[string]string{o.Account: "This account is waiting for reset."}})
	return b
}

func TestAnalyzerAgreeAndCache(t *testing.T) {
	g, o, at := analyzerFixture(t, true)
	f := &fakeAnalyzer{answer: analyzerAnswer(o, "exhausted", at.Add(time.Hour).Format(time.RFC3339))}
	res, err := g.Analyze(context.Background(), AnalyzeOptions{Backend: f})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 1 || res.Analyzed != 1 || f.calls != 1 {
		t.Fatalf("result=%+v calls=%d", res, f.calls)
	}
	states := Observed(context.Background(), at.Add(time.Minute))
	if len(states) != 1 || states[0].Classification != "observed" || states[0].Signal == "" || states[0].Summary == "" {
		t.Fatalf("observed=%+v", states)
	}
	res, err = g.Analyze(context.Background(), AnalyzeOptions{Backend: f})
	if err != nil || res.Cached != 1 || f.calls != 1 {
		t.Fatalf("cache result=%+v calls=%d err=%v", res, f.calls, err)
	}
	if got := Observed(context.Background(), at.Add(2*time.Hour)); len(got) != 0 {
		t.Fatalf("expired=%+v", got)
	}
}

func TestAnalyzerUnconfirmedInferredConflict(t *testing.T) {
	_, o, at := analyzerFixture(t, false)
	if got := Observed(context.Background(), at.Add(time.Minute)); len(got) != 1 || got[0].Classification != "observed-unconfirmed" {
		t.Fatalf("matcher=%+v", got)
	}
	for _, tt := range []struct {
		name, kind, want string
		parsed           bool
	}{{"inferred", "warning", "inferred", false}, {"conflict", "warning", "conflict", true}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BASHY_HOME", t.TempDir())
			obs := o
			if !tt.parsed {
				obs.Excerpt = "Vendor account is almost full"
				obs.Tool = "fixture"
				obs.Plan = "fixture"
			}
			if err := quotaobs.Record(context.Background(), obs); err != nil {
				t.Fatal(err)
			}
			read, err := quotaobs.Read(time.Time{}, nil)
			if err != nil || len(read) != 1 {
				t.Fatalf("read: %v, %d", err, len(read))
			}
			obs = read[0]
			var rows []annotation
			if tt.parsed {
				rows = append(rows, annotation{ExcerptSHA: obs.ExcerptSHA, Parsed: &quotaobs.ParsedSignal{Signal: "fixture", Kind: "exhausted"}})
			}
			a := quotaobs.Analysis{ExcerptSHA: obs.ExcerptSHA, Kind: tt.kind, Window: "5h", Confidence: .8, Summary: "Warning"}
			rows = append(rows, annotation{ExcerptSHA: obs.ExcerptSHA, Model: "test-analyzer", Analysis: &a})
			if err := appendAnnotations(rows); err != nil {
				t.Fatal(err)
			}
			if got := Observed(context.Background(), at.Add(time.Minute)); len(got) != 1 || got[0].Classification != tt.want {
				t.Fatalf("states=%+v", got)
			}
		})
	}
}

func TestAnalyzerConsentAndInvalidJSON(t *testing.T) {
	g, o, at := analyzerFixture(t, false)
	f := &fakeAnalyzer{answer: analyzerAnswer(o, "exhausted", at.Add(time.Hour).Format(time.RFC3339))}
	res, err := g.Analyze(context.Background(), AnalyzeOptions{Backend: f})
	if err != nil || f.calls != 0 || res.Hint == "" {
		t.Fatalf("consent=%+v calls=%d err=%v", res, f.calls, err)
	}
	g.cfg.Policy.Analyzer.Enabled = true
	f.answer = []byte(`{"analyses":[{"excerpt_sha":"` + o.ExcerptSHA + `","kind":"exhausted","window":"5h","used_percent":100,"reset_at":"` + at.Add(time.Hour).Format(time.RFC3339) + `","account_hint":"x","confidence":1,"summary":"x","extra":1}],"account_summaries":{}}`)
	res, err = g.Analyze(context.Background(), AnalyzeOptions{Backend: f})
	if err != nil || res.Invalid != 1 || res.Analyzed != 0 {
		t.Fatalf("invalid=%+v err=%v", res, err)
	}
	f.answer = analyzerAnswer(o, "exhausted", at.Add(32*24*time.Hour).Format(time.RFC3339))
	res, err = g.Analyze(context.Background(), AnalyzeOptions{Backend: f})
	if err != nil || res.Invalid != 1 {
		t.Fatalf("bad reset=%+v err=%v", res, err)
	}
}

func TestEnableAnalyzerPolicyConsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	g := New(Config{PolicyPath: path})
	if err := g.EnableAnalyzer(); err != nil {
		t.Fatal(err)
	}
	p, err := g.policy()
	if err != nil || !p.Analyzer.Enabled {
		t.Fatalf("enabled=%+v err=%v", p, err)
	}
	if err := g.DisableAnalyzer(); err != nil {
		t.Fatal(err)
	}
	p, err = g.policy()
	if err != nil || p.Analyzer.Enabled {
		t.Fatalf("disabled=%+v err=%v", p, err)
	}
}

func TestAnalyzerSelectsIndependentHeadroom(t *testing.T) {
	model, agent := bestAnalyzerCandidate([]analyzerCandidate{
		{Model: "self", Agent: "self-agent", Account: "observed", Headroom: .99},
		{Model: "other", Agent: "other-agent", Account: "independent", Headroom: .45},
	}, map[string]bool{"observed": true})
	if model != "other" || agent != "other-agent" {
		t.Fatalf("selection = %q %q", model, agent)
	}
}
