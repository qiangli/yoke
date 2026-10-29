package capability

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
)

func TestDelegateIdentity(t *testing.T) {
	cases := []struct {
		tool, model string
		want        string
	}{
		{"codex", "gpt6-sol", "genie-delegate:codex-gpt6-sol"},
		{"claude", "opus5.5", "genie-delegate:claude-opus5.5"},
		{"agy", "gemini-3.8", "genie-delegate:agy-gemini-3.8"},
		{"codex:gpt6-sol", "", "genie-delegate:codex-gpt6-sol"},
		{"genie-delegate:codex-gpt6-sol", "", "genie-delegate:codex-gpt6-sol"},
		{"codex-gpt6-sol", "", "genie-delegate:codex-gpt6-sol"},
	}

	for _, tc := range cases {
		got := DelegateIdentity(tc.tool, tc.model)
		if got != tc.want {
			t.Errorf("DelegateIdentity(%q, %q) = %q, want %q", tc.tool, tc.model, got, tc.want)
		}
		if err := ladderRecordCanonicalAgent(got); err != nil {
			t.Errorf("ladderRecordCanonicalAgent(%q) rejected delegate identity: %v", got, err)
		}
		if !IsDelegateIdentity(got) {
			t.Errorf("IsDelegateIdentity(%q) = false, want true", got)
		}
	}
}

func TestParseDelegateIdentity(t *testing.T) {
	tool, model, ok := ParseDelegateIdentity("genie-delegate:codex-gpt6-sol")
	if !ok || tool != "codex" || model != "gpt6-sol" {
		t.Fatalf("ParseDelegateIdentity = (%q, %q, %v), want (codex, gpt6-sol, true)", tool, model, ok)
	}

	if _, _, ok := ParseDelegateIdentity("ycode:gpt6-sol"); ok {
		t.Fatal("ParseDelegateIdentity accepted non-delegate identity")
	}
}

func TestRecordDelegateRunWritesOneEventUnderDelegateIdentityOnly(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "events.jsonl")

	ev := ladder.Event{
		Story:   "story-delegate-1",
		Points:  ladder.Points(3),
		Outcome: 1,
		Season:  1,
		At:      time.Date(2026, 10, 29, 7, 0, 0, 0, time.UTC),
	}

	if err := RecordDelegateRun("codex", "gpt6-sol", ev, storePath); err != nil {
		t.Fatalf("RecordDelegateRun failed: %v", err)
	}

	st, err := ladder.OpenStore(storePath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	events, err := st.Read()
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events count = %d, want exactly 1", len(events))
	}

	got := events[0]
	wantIdentity := "genie-delegate:codex-gpt6-sol"
	if got.Agent != wantIdentity {
		t.Errorf("Agent = %q, want %q", got.Agent, wantIdentity)
	}
	if got.RatingAgent() != wantIdentity {
		t.Errorf("RatingAgent = %q, want %q", got.RatingAgent(), wantIdentity)
	}
	if got.Kind != ladder.EventKindDelivery {
		t.Errorf("Kind = %q, want %q", got.Kind, ladder.EventKindDelivery)
	}
	if got.Duty != ladder.DutyCode {
		t.Errorf("Duty = %q, want %q", got.Duty, ladder.DutyCode)
	}
	if got.Points != 3 || got.Outcome != 1 {
		t.Errorf("Points/Outcome = %d/%g, want 3/1", got.Points, got.Outcome)
	}
}

func TestDelegatedRunNoCrossCreditingWithYcodeOrVendor(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "events.jsonl")
	st, err := ladder.OpenStore(storePath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	baseAt := time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)
	// 1. Existing events for ycode:gpt6-sol and codex:gpt6-sol
	ycodeEv := ladder.Event{
		ID:       "yc-1",
		Kind:     ladder.EventKindDelivery,
		Agent:    "ycode:gpt6-sol",
		Story:    "story-yc",
		Points:   ladder.Points(2),
		Outcome:  1,
		Duty:     ladder.DutyCode,
		Season:   1,
		At:       baseAt,
		Reviewer: "operator",
	}
	vendorEv := ladder.Event{
		ID:       "ven-1",
		Kind:     ladder.EventKindDelivery,
		Agent:    "codex:gpt6-sol",
		Story:    "story-ven",
		Points:   ladder.Points(2),
		Outcome:  1,
		Duty:     ladder.DutyCode,
		Season:   1,
		At:       baseAt.Add(time.Minute),
		Reviewer: "operator",
	}
	if err := st.Append(ycodeEv); err != nil {
		t.Fatalf("st.Append(ycodeEv) failed: %v", err)
	}
	if err := st.Append(vendorEv); err != nil {
		t.Fatalf("st.Append(vendorEv) failed: %v", err)
	}

	// Verify baseline replay before delegation
	eventsBefore, _ := st.Read()
	repBefore := ladder.Replay(eventsBefore, 1)
	if repBefore.Agents["ycode:gpt6-sol"].Standings[ladder.DutyCode].Events != 1 {
		t.Fatal("baseline ycode events != 1")
	}
	if repBefore.Agents["codex:gpt6-sol"].Standings[ladder.DutyCode].Events != 1 {
		t.Fatal("baseline vendor events != 1")
	}
	ycodeRatingBefore := repBefore.Agents["ycode:gpt6-sol"].Standings[ladder.DutyCode].R
	vendorRatingBefore := repBefore.Agents["codex:gpt6-sol"].Standings[ladder.DutyCode].R

	// 2. Record delegated run under codex / gpt6-sol
	delegateEv := ladder.Event{
		Story:    "story-del",
		Points:   ladder.Points(3),
		Outcome:  1,
		Duty:     ladder.DutyCode,
		Season:   1,
		At:       baseAt.Add(2 * time.Minute),
		Reviewer: "operator",
	}
	if err := RecordDelegateRun("codex", "gpt6-sol", delegateEv, storePath); err != nil {
		t.Fatalf("RecordDelegateRun failed: %v", err)
	}

	// 3. Replay updated events
	eventsAfter, err := st.Read()
	if err != nil {
		t.Fatalf("st.Read() failed: %v", err)
	}
	if len(eventsAfter) != 3 {
		t.Fatalf("expected 3 events, got %d", len(eventsAfter))
	}

	repAfter := ladder.Replay(eventsAfter, 1)

	// ycode row must be completely unchanged
	ycAfter := repAfter.Agents["ycode:gpt6-sol"]
	if ycAfter.Standings[ladder.DutyCode].Events != 1 {
		t.Errorf("ycode events changed to %d, want 1 (cross-credited)", ycAfter.Standings[ladder.DutyCode].Events)
	}
	if ycAfter.Standings[ladder.DutyCode].R != ycodeRatingBefore {
		t.Errorf("ycode rating changed from %g to %g (cross-credited)", ycodeRatingBefore, ycAfter.Standings[ladder.DutyCode].R)
	}

	// vendor row must be completely unchanged
	venAfter := repAfter.Agents["codex:gpt6-sol"]
	if venAfter.Standings[ladder.DutyCode].Events != 1 {
		t.Errorf("vendor events changed to %d, want 1 (cross-credited)", venAfter.Standings[ladder.DutyCode].Events)
	}
	if venAfter.Standings[ladder.DutyCode].R != vendorRatingBefore {
		t.Errorf("vendor rating changed from %g to %g (cross-credited)", vendorRatingBefore, venAfter.Standings[ladder.DutyCode].R)
	}

	// delegate row must have its own standing
	delAfter := repAfter.Agents["genie-delegate:codex-gpt6-sol"]
	if delAfter == nil {
		t.Fatal("genie-delegate:codex-gpt6-sol missing from replay results")
	}
	if delAfter.Standings[ladder.DutyCode].Events != 1 {
		t.Errorf("delegate events = %d, want 1", delAfter.Standings[ladder.DutyCode].Events)
	}
}

func TestFamilyIDRatingAgentResolutionCannotFoldDelegate(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "events.jsonl")

	// Even if an event carries FamilyID pointing to ycode or vendor family,
	// RatingAgent() and RecordDelegateRun MUST NOT fold it.
	ev := ladder.Event{
		Story:        "story-family",
		Points:       ladder.Points(2),
		Outcome:      1,
		FamilyID:     "family:ycode-frozen-v1",
		InstanceUUID: "uuid-ycode-inst",
		Season:       1,
		At:           time.Date(2026, 10, 29, 7, 0, 0, 0, time.UTC),
	}

	if err := RecordDelegateRun("codex", "gpt6-sol", ev, storePath); err != nil {
		t.Fatalf("RecordDelegateRun failed: %v", err)
	}

	st, _ := ladder.OpenStore(storePath)
	events, _ := st.Read()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	readEv := events[0]
	if readEv.RatingAgent() != "genie-delegate:codex-gpt6-sol" {
		t.Fatalf("RatingAgent = %q, want genie-delegate:codex-gpt6-sol", readEv.RatingAgent())
	}

	// Also test raw Event RatingAgent directly with FamilyID set
	rawEv := ladder.Event{
		Agent:    "genie-delegate:codex-gpt6-sol",
		FamilyID: "family:vendor-frozen-v1",
	}
	if rawEv.RatingAgent() != "genie-delegate:codex-gpt6-sol" {
		t.Fatalf("rawEv.RatingAgent = %q, want genie-delegate:codex-gpt6-sol", rawEv.RatingAgent())
	}

	rep := ladder.Replay([]ladder.Event{readEv}, 1)
	if rep.Agents["family:ycode-frozen-v1"] != nil {
		t.Fatal("event folded into family:ycode-frozen-v1")
	}
	if rep.Agents["genie-delegate:codex-gpt6-sol"] == nil {
		t.Fatal("event missing from genie-delegate:codex-gpt6-sol")
	}
}

func TestDutyBoardShowsDelegateRowSeparately(t *testing.T) {
	baseAt := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	events := []ladder.Event{
		{
			ID:       "yc-1",
			Kind:     ladder.EventKindDelivery,
			Agent:    "ycode:gpt6-sol",
			Story:    "story-yc",
			Points:   ladder.Points(3),
			Outcome:  1,
			Duty:     ladder.DutyCode,
			Season:   1,
			At:       baseAt,
			Reviewer: "operator",
		},
		{
			ID:       "ven-1",
			Kind:     ladder.EventKindDelivery,
			Agent:    "codex:gpt6-sol",
			Story:    "story-ven",
			Points:   ladder.Points(3),
			Outcome:  1,
			Duty:     ladder.DutyCode,
			Season:   1,
			At:       baseAt.Add(time.Minute),
			Reviewer: "operator",
		},
		{
			ID:       "del-1",
			Kind:     ladder.EventKindDelivery,
			Agent:    "genie-delegate:codex-gpt6-sol",
			Story:    "story-del",
			Points:   ladder.Points(3),
			Outcome:  1,
			Duty:     ladder.DutyCode,
			Season:   1,
			At:       baseAt.Add(2 * time.Minute),
			Reviewer: "operator",
		},
	}

	board := ComputeDutyBoard(events, 1, nil, []ladder.Duty{ladder.DutyCode})
	rows := board.Duties[string(ladder.DutyCode)]

	var hasYcode, hasVendor, hasDelegate bool
	for _, r := range rows {
		switch r.Agent {
		case "ycode:gpt6-sol":
			hasYcode = true
			if r.Events != 1 {
				t.Errorf("ycode row events = %d, want 1", r.Events)
			}
		case "codex:gpt6-sol":
			hasVendor = true
			if r.Events != 1 {
				t.Errorf("vendor row events = %d, want 1", r.Events)
			}
		case "genie-delegate:codex-gpt6-sol":
			hasDelegate = true
			if r.Events != 1 {
				t.Errorf("delegate row events = %d, want 1", r.Events)
			}
		}
	}

	if !hasYcode {
		t.Error("ComputeDutyBoard missing ycode:gpt6-sol row")
	}
	if !hasVendor {
		t.Error("ComputeDutyBoard missing codex:gpt6-sol row")
	}
	if !hasDelegate {
		t.Error("ComputeDutyBoard missing genie-delegate:codex-gpt6-sol row")
	}
}

func TestRecordDelegateEvent(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "events.jsonl")

	ev := ladder.Event{
		Agent:   "codex:gpt6-sol",
		Story:   "story-event",
		Points:  ladder.Points(2),
		Outcome: 1,
		Season:  1,
	}

	if err := RecordDelegateEvent(ev, storePath); err != nil {
		t.Fatalf("RecordDelegateEvent failed: %v", err)
	}

	st, _ := ladder.OpenStore(storePath)
	events, _ := st.Read()
	if len(events) != 1 {
		t.Fatalf("events count = %d, want 1", len(events))
	}
	if events[0].Agent != "genie-delegate:codex-gpt6-sol" {
		t.Fatalf("Agent = %q, want genie-delegate:codex-gpt6-sol", events[0].Agent)
	}
}

func TestRecordDelegateLedger(t *testing.T) {
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())

	pass := true
	r := RunRecord{
		Source:   SourceWeave,
		GatePass: &pass,
	}

	if err := RecordDelegateLedger("codex", "gpt6-sol", r); err != nil {
		t.Fatalf("RecordDelegateLedger failed: %v", err)
	}

	records, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger failed: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records count = %d, want 1", len(records))
	}
	if records[0].Agent != "genie-delegate:codex-gpt6-sol" {
		t.Fatalf("Agent = %q, want genie-delegate:codex-gpt6-sol", records[0].Agent)
	}
}
