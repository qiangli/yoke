package watch

import (
	"testing"
	"time"
)

// isolatePresets points the local ring at an empty directory, so these tests
// read the embedded baseline and never whatever the host operator installed.
func isolatePresets(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_WATCH_DIR", t.TempDir())
}

type srcRow struct {
	typ      string
	target   string
	backfill time.Duration
}

func sourceRows(f RuleFile) []srcRow {
	out := make([]srcRow, 0, len(f.Sources))
	for _, s := range f.Sources {
		out = append(out, srcRow{s.Type, s.Target, s.Backfill.D()})
	}
	return out
}

func sameRows(got, want []srcRow) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sprintPreset(t *testing.T) RuleFile {
	t.Helper()
	preset, err := LoadPreset("sprint")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	return preset.File
}

func TestNarrowSourcesWithNoSelectionKeepsEveryPresetSource(t *testing.T) {
	isolatePresets(t)
	file := sprintPreset(t)
	want := sourceRows(file)
	if got := sourceRows(file.NarrowSources(nil)); !sameRows(got, want) {
		t.Fatalf("sources = %+v, want the preset's own %+v", got, want)
	}
}

func TestNarrowSourcesReplacesThePresetSourceOfThatKind(t *testing.T) {
	isolatePresets(t)
	file := sprintPreset(t)

	// One explicit card: the preset's own unfiltered sprint-thread source is
	// REPLACED, not kept beside it — otherwise every card on the host is
	// reported to a watcher that asked for one.
	got := sourceRows(file.NarrowSources([]SourceSpec{{Type: "sprint-thread", Target: "472"}}))
	want := []srcRow{
		{"sprint-thread", "472", 2 * time.Hour},
		{"mb", "", 2 * time.Hour},
	}
	if !sameRows(got, want) {
		t.Fatalf("sources = %+v, want %+v", got, want)
	}
}

func TestNarrowSourcesKeepsOtherKinds(t *testing.T) {
	isolatePresets(t)
	file := sprintPreset(t)

	// An mb selection says nothing about sprint threads: the preset's
	// sprint-thread source stays exactly as it shipped.
	got := sourceRows(file.NarrowSources([]SourceSpec{{Type: "mb", Target: "seat-x"}}))
	want := []srcRow{
		{"sprint-thread", "", 2 * time.Hour},
		{"mb", "seat-x", 2 * time.Hour},
	}
	if !sameRows(got, want) {
		t.Fatalf("sources = %+v, want %+v", got, want)
	}
}

func TestNarrowSourcesSeveralTargetsOfOneKind(t *testing.T) {
	isolatePresets(t)
	file := sprintPreset(t)
	got := sourceRows(file.NarrowSources([]SourceSpec{
		{Type: "sprint-thread", Target: "472"},
		{Type: "sprint-thread", Target: "480"},
		{Type: "mb", Target: "seat-x"},
	}))
	want := []srcRow{
		{"sprint-thread", "472", 2 * time.Hour},
		{"sprint-thread", "480", 2 * time.Hour},
		{"mb", "seat-x", 2 * time.Hour},
	}
	if !sameRows(got, want) {
		t.Fatalf("sources = %+v, want %+v", got, want)
	}
}

func TestNarrowSourcesSelectionBackfillWins(t *testing.T) {
	isolatePresets(t)
	file := sprintPreset(t)
	got := sourceRows(file.NarrowSources([]SourceSpec{
		{Type: "sprint-thread", Target: "472", Backfill: Duration(10 * time.Minute)},
	}))
	want := []srcRow{
		{"sprint-thread", "472", 10 * time.Minute},
		{"mb", "", 2 * time.Hour},
	}
	if !sameRows(got, want) {
		t.Fatalf("sources = %+v, want %+v", got, want)
	}
}

func TestNarrowSourcesAddsAKindTheFileDoesNotHave(t *testing.T) {
	file := RuleFile{Sources: []SourceSpec{{Type: "mb", Backfill: Duration(time.Hour)}}}
	got := sourceRows(file.NarrowSources([]SourceSpec{{Type: "sprint-thread", Target: "472"}}))
	want := []srcRow{
		{"mb", "", time.Hour},
		{"sprint-thread", "472", 0},
	}
	if !sameRows(got, want) {
		t.Fatalf("sources = %+v, want %+v", got, want)
	}
}

func TestNarrowSourcesDoesNotMutateTheFile(t *testing.T) {
	isolatePresets(t)
	file := sprintPreset(t)
	before := sourceRows(file)
	file.NarrowSources([]SourceSpec{{Type: "sprint-thread", Target: "472"}})
	if got := sourceRows(file); !sameRows(got, before) {
		t.Fatalf("the rule file was mutated: %+v, want %+v", got, before)
	}
}
