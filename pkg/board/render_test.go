package board

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) *Board {
	t.Helper()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	sources := []Source{SourceFunc{SourceName: "fixture", Func: func(_ context.Context, b *Board, _ Options) error {
		b.Runs = []Run{{ID: 7, Label: "ship board", Repo: "coreutils", State: "working", Tool: "codex", Agent: "sol", Model: "gpt-5.6-sol", Band: 4, StartedAt: now.Add(-5 * time.Minute), MaxRuntime: 1800}, {ID: 8, Label: "merge me", Repo: "bashy", State: "submitted", Tool: "claude", Model: "opus4.8", Band: 4, AgeSeconds: int64((5 * time.Hour) / time.Second), Stale: true}, {ID: 9, Label: "commit survived watchdog", Repo: "coreutils", State: "killed", Tool: "codex", Salvageable: true, UnmergedCommits: 2}}
		b.Todos = []Todo{{ID: "abc", Number: 3, Title: "blocked chore", Status: "blocked", Scope: "user steward"}}
		b.Sprints = []Sprint{{ID: 2, Title: "board sprint", Column: "review"}}
		b.Agents = []Agent{{Name: "sol", Tool: "codex", Band: 4, Model: "gpt-5.6-sol", Available: true, Availability: "available", State: "working"}}
		return nil
	}}}
	b, err := Collect(context.Background(), Options{Now: now}, sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTerminalAndJSONGoldens(t *testing.T) {
	b := fixture(t)
	text, err := (TerminalRenderer{}).Render(b, Options{Expand: map[string]bool{"agents": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "age 5h0m0s") || !strings.Contains(string(text), "STALE") {
		t.Fatalf("terminal did not render unattended age and flag:\n%s", text)
	}
	// Golden rebased 2026-09-22 for the read-only Claims panel. Claims remain a
	// panel projection, not Rows, so the work lanes and summary are unchanged.
	if got, want := fmt.Sprintf("%x", sha256.Sum256(text)), "b31bb27445252c32d0f1b60deabff6a286f4dd0f312ab58ff2fa9fc63aea6580"; got != want {
		t.Errorf("terminal golden changed: got %s\n%s", got, text)
	}
	if !strings.Contains(string(text), "Dag runs") {
		t.Errorf("dag panel missing from the terminal board:\n%s", text)
	}
	raw, err := (JSONRenderer{}).Render(b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got Board
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SchemaVersion || got.Summary.NeedsSteward != 4 || got.Summary.Unattended != 1 {
		t.Fatalf("bad JSON envelope: %+v", got.Summary)
	}
	// Rebased 2026-09-22 for the read-only Claims panel.
	if sum, want := fmt.Sprintf("%x", sha256.Sum256(raw)), "f02444951ce5647707c2747aefa0b96d6b988c52ad02085f426c5ab465ba5ee8"; sum != want {
		t.Errorf("JSON golden changed: got %s\n%s", sum, raw)
	}
	if strings.Contains(string(raw), "dag_runs") {
		t.Errorf("empty dag_runs must be omitted from the wire shape:\n%s", raw)
	}
}

// The dag panel projects the run journal; failures are what a steward scans
// for, so they must surface in the collapsed summary without expanding it.
func TestDagPanelSurfacesFailures(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	sources := []Source{SourceFunc{SourceName: "fixture", Func: func(_ context.Context, b *Board, _ Options) error {
		b.DagRuns = []DagRun{
			{RunID: "2-aaa", File: "/w/DAG.md", Targets: "train", StartedAt: now.Add(-time.Hour), DurationMS: 90000, Total: 3, OK: 3},
			{RunID: "1-bbb", File: "/w/DAG.md", Targets: "test", StartedAt: now.Add(-2 * time.Hour), DurationMS: 5, Total: 2, OK: 1, FailedN: 1, Failed: true},
		}
		return nil
	}}}
	b, err := Collect(context.Background(), Options{Now: now}, sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	var view *PanelView
	for i := range b.Panels {
		if b.Panels[i].ID == "dag" {
			view = &b.Panels[i]
		}
	}
	if view == nil {
		t.Fatal("no dag panel built")
	}
	if !strings.Contains(view.Collapsed, "2 recent run(s); 1 failed") {
		t.Errorf("collapsed summary = %q", view.Collapsed)
	}
	if len(view.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(view.Rows))
	}
	if view.Rows[0][0] != "ok" {
		t.Errorf("first row result = %q, want ok", view.Rows[0][0])
	}
	if view.Rows[1][0] != "FAILED 1/2" {
		t.Errorf("failed row result = %q, want the failed/total count", view.Rows[1][0])
	}
}

func TestSalvageableRunRoutesToNeedsStewardAndPanel(t *testing.T) {
	b := fixture(t)
	var inLane bool
	for _, lane := range b.Lanes {
		if lane.ID != "needs-steward" {
			continue
		}
		for _, card := range lane.Cards {
			inLane = inLane || card.ID == "9" && card.Salvageable && card.Unmerged == 2
		}
	}
	if !inLane {
		t.Fatal("salvageable killed run was not routed to needs-steward")
	}
	var salvage PanelView
	for _, panel := range b.Panels {
		if panel.ID == "salvage" {
			salvage = panel
		}
	}
	if len(salvage.Rows) != 1 || salvage.Rows[0][0] != "#9" {
		t.Fatalf("salvage panel = %+v, want exactly run #9", salvage)
	}
	text, err := (TerminalRenderer{}).Render(b, Options{Expand: map[string]bool{"salvage": true}})
	if err != nil || !strings.Contains(string(text), "#9") || !strings.Contains(string(text), "2 commits") {
		t.Fatalf("expanded salvage panel did not list the steward decision: err=%v\n%s", err, text)
	}
}

func TestHTMLIsSelfContained(t *testing.T) {
	raw, err := (HTMLRenderer{}).Render(fixture(t), Options{Expand: map[string]bool{"agents": true}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"<!doctype html>", "<style>", "prefers-color-scheme", "<details id=\"agents\" open", "age 5h0m0s", "STALE"} {
		if !strings.Contains(s, want) {
			t.Errorf("html missing %q", want)
		}
	}
	for _, bad := range []string{"http://", "https://", "<script", "src=", "href="} {
		if strings.Contains(strings.ToLower(s), bad) {
			t.Errorf("HTML contains external-capable token %q", bad)
		}
	}
}
