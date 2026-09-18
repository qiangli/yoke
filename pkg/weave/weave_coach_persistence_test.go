package weave

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Coach fields must survive a save/load round-trip so the leaderboard
// can measure reflex-coach effectiveness across runs.
func TestCoachFieldsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()

	it := &weaveItem{
		ID:                 1,
		Title:              "coach-test",
		State:              "submitted",
		Created:            time.Now().UTC(),
		CoachTotalCalls:    142,
		CoachDistinctCalls: 17,
		CoachRepeatRatio:   8.35,
		CoachSteers:        3,
		CoachRecovered:     true,
		CoachMode:          "pty",
	}

	q := &weaveQueue{NextID: 2, Items: []*weaveItem{it}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(loaded.Items))
	}
	got := loaded.Items[0]

	if got.CoachTotalCalls != 142 {
		t.Errorf("CoachTotalCalls = %d, want 142", got.CoachTotalCalls)
	}
	if got.CoachDistinctCalls != 17 {
		t.Errorf("CoachDistinctCalls = %d, want 17", got.CoachDistinctCalls)
	}
	if got.CoachRepeatRatio != 8.35 {
		t.Errorf("CoachRepeatRatio = %f, want 8.35", got.CoachRepeatRatio)
	}
	if got.CoachSteers != 3 {
		t.Errorf("CoachSteers = %d, want 3", got.CoachSteers)
	}
	if !got.CoachRecovered {
		t.Error("CoachRecovered = false, want true (steered and submitted)")
	}
	if got.CoachMode != "pty" {
		t.Errorf("CoachMode = %q, want \"pty\"", got.CoachMode)
	}
}

// CoachRecovered must be true only when steered >= 1 AND the run
// converged (submitted/done/merged). Not steered → not recovered;
// steered but failed → not recovered.
func TestCoachRecoveredLogic(t *testing.T) {
	tests := []struct {
		name      string
		steers    int
		state     string
		recovered bool
	}{
		{"no steers, submitted", 0, "submitted", false},
		{"steered, submitted", 2, "submitted", true},
		{"steered, done", 1, "done", true},
		{"steered, merged", 1, "merged", true},
		{"steered, failed", 3, "failed", false},
		{"steered, killed", 1, "killed", false},
		{"no steers, failed", 0, "failed", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := &weaveItem{State: tt.state}
			steered := tt.steers >= 1
			converged := it.State == "submitted" || it.State == "done" || it.State == "merged"
			if steered && converged {
				it.CoachRecovered = true
			}
			if it.CoachRecovered != tt.recovered {
				t.Errorf("steers=%d state=%s: CoachRecovered = %v, want %v",
					tt.steers, tt.state, it.CoachRecovered, tt.recovered)
			}
		})
	}
}

// Zero coach values (no coach attached) must not be written into the
// JSON (omitempty) so legacy queues without coach fields stay clean.
func TestCoachFieldsOmitZeroValues(t *testing.T) {
	dir := t.TempDir()

	it := &weaveItem{
		ID:      1,
		Title:   "no-coach",
		State:   "submitted",
		Created: time.Now().UTC(),
	}

	q := &weaveQueue{NextID: 2, Items: []*weaveItem{it}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal top: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(m["items"], &items); err != nil {
		t.Fatalf("unmarshal items: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("empty items")
	}

	for _, key := range []string{
		"coach_total_calls", "coach_distinct_calls",
		"coach_repeat_ratio", "coach_steers",
		"coach_recovered", "coach_mode",
	} {
		if _, ok := items[0][key]; ok {
			t.Errorf("key %q should be omitted for zero coach values", key)
		}
	}
}
