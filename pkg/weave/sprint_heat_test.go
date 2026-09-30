package weave

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
)

func TestHeatFakeLaunchAndGrade(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 10, 0, time.UTC)
	rec := heatRecord{ID: "fake", Attempts: []heatAttempt{{Agent: "agent-a"}, {Agent: "agent-b"}}}
	story := heatStoryPrompt("Implement parser\nHeat: secret")
	launch := func(i int, a *heatAttempt) error {
		if strings.Contains(story, "Heat") {
			return fmt.Errorf("blind prompt leaked")
		}
		a.Fairness = heatFairness{Base: strings.Repeat("a", 40), Template: "same", ToolVersion: "v1", Model: a.Agent, Points: 3, MaxRuntime: time.Minute, Started: now.Add(time.Duration(i) * time.Second)}
		return nil
	}
	grades := 0
	grade := func(i int, a *heatAttempt) error { grades++; a.Verdict = "pass"; a.Turns = i + 1; return nil }
	heatSchedule(&rec, launch, grade)
	if rec.Status != "rated" || rec.Winner != "agent-a" || grades != 2 || rec.Attempts[0].Fairness.Template != rec.Attempts[1].Fairness.Template {
		t.Fatalf("heat: %+v grades=%d", rec, grades)
	}
	rec = heatRecord{ID: "fake", Attempts: []heatAttempt{{Agent: "agent-a"}, {Agent: "agent-b"}}}
	heatSchedule(&rec, func(i int, a *heatAttempt) error {
		if err := launch(i, a); err != nil {
			return err
		}
		if i == 1 {
			a.Fairness.ToolVersion = "v2"
		}
		return nil
	}, grade)
	if rec.Status != "rated" || grades != 4 {
		t.Fatalf("cross-tool heat should rate: %+v grades=%d", rec, grades)
	}
}

func TestHeatFairnessAndWinner(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 10, 0, time.UTC)
	base := heatFairness{Base: strings.Repeat("a", 40), Template: "template", ToolVersion: "v1", Model: "agent-a", Points: 3, MaxRuntime: time.Minute * 10, Started: now}
	b := base
	b.Started = now.Add(20 * time.Second)
	if heatDigest(base) != heatDigest(b) || heatMismatch([]heatFairness{base, b}) != "" {
		t.Fatal("same tick and components must be fair")
	}
	b.Model = "agent-b"
	if heatDigest(base) == heatDigest(b) || heatMismatch([]heatFairness{base, b}) != "" {
		t.Fatal("distinct entrant models need distinct digests but remain comparable")
	}
	b.ToolVersion = "v2"
	if heatMismatch([]heatFairness{base, b}) != "" {
		t.Fatal("different entrant tool versions must remain comparable")
	}
	b.Template = "other"
	if heatMismatch([]heatFairness{base, b}) == "" {
		t.Fatal("template mismatch must void heat")
	}
	b = base
	b.PromptHash = "different"
	if heatMismatch([]heatFairness{base, b}) != "prompt mismatch" {
		t.Fatal("prompt mismatch must void heat")
	}
	b = base
	b.Gate = "different"
	if heatMismatch([]heatFairness{base, b}) != "gate mismatch" {
		t.Fatal("gate mismatch must void heat")
	}
	b = base
	b.Started = now.Add(59 * time.Second)
	if heatMismatch([]heatFairness{base, b}) != "" {
		t.Fatal("starts within 60 seconds are comparable")
	}
	b.Started = now.Add(61 * time.Second)
	if heatMismatch([]heatFairness{base, b}) != "start tick mismatch" {
		t.Fatal("starts beyond 60 seconds must void")
	}
	results := []heatAttempt{{Agent: "agent-a", Verdict: "fail", Turns: 1}, {Agent: "agent-b", Verdict: "pass", Turns: 20}}
	if got := heatWinner(results); got != "agent-b" {
		t.Fatalf("winner = %q", got)
	}
	results[0].Verdict = "pass"
	results[0].Turns = 4
	if got := heatWinner(results); got != "agent-a" {
		t.Fatalf("lower turns winner = %q", got)
	}
	results[0].Cost = 2
	results[1].Cost = 1
	if got := heatWinner(results); got != "agent-b" {
		t.Fatalf("lower cost winner = %q", got)
	}
	if got := heatResultCost("{\"type\":\"result\",\"total_cost_usd\":1.25}\n"); got != 1.25 {
		t.Fatalf("cost = %g", got)
	}
}

func TestHeatPreflightFailureIsEnvironmentBlame(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	rec := heatRecord{ID: "preflight", Attempts: []heatAttempt{{Agent: "agent-a"}, {Agent: "agent-b"}}}
	heatSchedule(&rec, func(i int, a *heatAttempt) error {
		a.Fairness = heatFairness{Base: "base", Template: "template", ToolVersion: fmt.Sprintf("v%d", i), Model: a.Agent, Points: 1, MaxRuntime: time.Minute, Started: now}
		if i == 1 {
			return fmt.Errorf("workspace preflight failed: authentication required")
		}
		return nil
	}, func(i int, a *heatAttempt) error { a.Verdict = "pass"; return nil })
	if rec.Status != "rated" || rec.Attempts[1].Verdict != "fail" || rec.Attempts[1].Error == "" {
		t.Fatalf("preflight attempt: %+v", rec)
	}
	e := heatDeliveryEvent(rec, rec.Attempts[1], now)
	if e.Blame.Class != "environment" || !strings.Contains(e.Blame.Evidence[0].Note, "authentication required") {
		t.Fatalf("blame: %+v", e.Blame)
	}
}

func TestHeatEntrantVersionChangesVoid(t *testing.T) {
	f := heatFairness{Base: "base", Template: "template", ToolVersion: "v1", Model: "model-a", Points: 1, MaxRuntime: time.Minute, Started: time.Now()}
	rec := heatRecord{Attempts: []heatAttempt{{Fairness: f, FinishToolVersion: "v2", FinishModel: "model-a"}, {Fairness: f, FinishToolVersion: "v1", FinishModel: "model-a"}}}
	heatSchedule(&rec, func(_ int, _ *heatAttempt) error { return nil }, func(_ int, a *heatAttempt) error { a.Verdict = "pass"; return nil })
	if rec.Status != "void" || rec.Reason != "entrant version changed" {
		t.Fatalf("version drift: %+v", rec)
	}
}

func TestHeatBlindPrompt(t *testing.T) {
	story := "Implement the parser.\nHeat: hidden\nSprint: hidden"
	prompt := heatStoryPrompt(story)
	if strings.Contains(strings.ToLower(prompt), "heat") || strings.Contains(strings.ToLower(prompt), "sprint") {
		t.Fatalf("prompt leaks scheduling context: %q", prompt)
	}
}

func TestHeatTemplateAndPairedEvents(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "story.txt"), []byte("same story"), 0600); err != nil {
		t.Fatal(err)
	}
	template := boothTemplate{Dir: source}
	var digests []string
	for i := 0; i < 2; i++ {
		dst := filepath.Join(t.TempDir(), "booth")
		if _, _, err := copyBoothTemplate(template, dst); err != nil {
			t.Fatal(err)
		}
		d, err := boothTemplateDigest(dst)
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, d)
	}
	if digests[0] != digests[1] {
		t.Fatalf("template copies differ: %v", digests)
	}
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	rec := heatRecord{ID: "paired", Sprint: 331, Story: "story-id"}
	a := heatAttempt{Agent: "agent-a", Run: "repo#1", Verdict: "pass", Fairness: heatFairness{Points: 3, MaxRuntime: time.Minute}}
	b := heatAttempt{Agent: "agent-b", Run: "repo#2", Verdict: "fail", Fairness: a.Fairness}
	e1, e2 := heatDeliveryEvent(rec, a, now), heatDeliveryEvent(rec, b, now)
	if e1.Note != "heat:paired" || e2.Note != e1.Note || e1.Outcome != 1 || e2.Outcome != 0 || e2.Blame.Evidence[0].Ref != "heat:paired/repo#2" {
		t.Fatalf("paired events: %+v %+v", e1, e2)
	}
	store, err := ladder.OpenStore(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(e1); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(e2); err != nil {
		t.Fatal(err)
	}
	events, err := store.Read()
	if err != nil || len(events) != 2 {
		t.Fatalf("read paired events: %v %v", events, err)
	}
}
