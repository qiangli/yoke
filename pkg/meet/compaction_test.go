package meet

import (
	"strings"
	"testing"
)

func TestPreviewOffloadsLongTurns(t *testing.T) {
	short := Event{Kind: "turn", Speaker: "claude", Text: "a concise point.", File: "/tmp/x.txt"}
	if got := preview(short); strings.Contains(got, "file://") {
		t.Errorf("short turn should inline in full, got offloaded: %q", got)
	}
	long := Event{Kind: "turn", Speaker: "claude", Text: strings.Repeat("word ", 400), File: "/tmp/full.txt"}
	got := preview(long)
	if !strings.Contains(got, "file:///tmp/full.txt") {
		t.Errorf("long turn should carry a file:// link, got %q", got)
	}
	if !strings.Contains(got, "chars — full:") || len(got) > previewFull+200 {
		t.Errorf("long turn should be a bounded head/tail preview, got len=%d", len(got))
	}
}

func TestTranscriptContextCollapsesOldTurns(t *testing.T) {
	var ev []Event
	for i := 0; i < 20; i++ {
		ev = append(ev, Event{Kind: "turn", Speaker: "a", Text: strings.Repeat("x", 800), File: "/tmp/f.txt"})
	}
	ctx := transcriptContext(ev)
	// Older turns collapse to a one-line ref ("[full: file://…]"); recent ones get
	// the richer "…chars — full:" preview. Total stays bounded well under 20×800.
	if len(ctx) > 12000 {
		t.Errorf("context not bounded: len=%d", len(ctx))
	}
	if !strings.Contains(ctx, "[full: file://") {
		t.Errorf("expected collapsed older-turn references")
	}
}

// A turn older than the recent window is rendered by briefRef, the ONE context
// path that used to skip transport normalization. A legacy turn whose stored Text
// is still a raw Claude stream-json envelope must not reach the next agent's
// prompt as machine transport (session_id/system/result JSON) — briefRef has to
// normalize it just like preview does. With >=9 turns the oldest falls outside the
// 8-turn recent window, so it is the collapsed briefRef line under test.
func TestTranscriptContextNormalizesOldTransportTurn(t *testing.T) {
	rawLegacyTransport := strings.Join([]string{
		fxClaudeSystemInit,
		fxClaudeRateLimit,
		fxClaudeAssistantText, // "The cache should be write-through."
		fxClaudeResult,
	}, "\n")

	ev := []Event{
		// index 0 — older than the 8 most-recent turns, so it collapses via briefRef.
		{Kind: "turn", Speaker: "claude-fable5", Text: rawLegacyTransport},
	}
	for i := 1; i < 9; i++ { // eight recent turns fill the window after it
		ev = append(ev, Event{Kind: "turn", Speaker: "a", Text: "a recent point."})
	}
	if got := len(ev); got < 9 {
		t.Fatalf("regression needs >=9 events to push the first past the recent window, got %d", got)
	}

	ctx := transcriptContext(ev)
	if !strings.Contains(ctx, "The cache should be write-through.") {
		t.Errorf("the legacy turn's assistant prose must survive normalization:\n%s", ctx)
	}
	for _, leak := range []string{"session_id", "rate_limit_event", `"type":"system"`, "input_tokens", "subtype"} {
		if strings.Contains(ctx, leak) {
			t.Errorf("raw transport %q leaked from an old turn into the replayed context:\n%s", leak, ctx)
		}
	}
}

func TestParseConverge(t *testing.T) {
	out := `DECISIONS:
- ship the P0 verbs
- keep secretary notes-only
ACTIONS:
- claude: file the minutes
OPEN QUESTIONS:
- blind vs sequential default?
SUMMARY:
The group agreed on the P0 scope.
It will iterate from there.`
	syn := parseConverge(out)
	if len(syn.Decisions) != 2 || len(syn.Actions) != 1 || len(syn.OpenQuestions) != 1 {
		t.Fatalf("parse counts: dec=%d act=%d oq=%d", len(syn.Decisions), len(syn.Actions), len(syn.OpenQuestions))
	}
	if syn.Decisions[0].Text != "ship the P0 verbs" || syn.Actions[0] != "claude: file the minutes" {
		t.Errorf("bad items: %v %v", syn.Decisions, syn.Actions)
	}
	if !strings.HasPrefix(syn.Summary, "The group agreed") || !strings.Contains(syn.Summary, "iterate") {
		t.Errorf("summary joined wrong: %q", syn.Summary)
	}
}

func TestParseConvergeNone(t *testing.T) {
	syn := parseConverge("DECISIONS:\nnone\nACTIONS:\nnone\nOPEN QUESTIONS:\nnone\nSUMMARY:\nNothing decided.")
	if len(syn.Decisions)+len(syn.Actions)+len(syn.OpenQuestions) != 0 {
		t.Errorf("'none' should yield no items")
	}
	if syn.Summary != "Nothing decided." {
		t.Errorf("summary=%q", syn.Summary)
	}
}

// The secretary may INFER a decision from consensus, but the reader must always
// be able to tell an inferred decision from a stated one — the label is the guard
// against hallucinated consensus, not the mode.
func TestParseConvergeMarksInferredDecisions(t *testing.T) {
	syn := parseConverge("DECISIONS:\n- ship the P0 verbs\n- (inferred) cert bypasses the atomizer\n" +
		"RISKS:\n- the fd race is unfixed\nCORRECTIONS:\n- chunks=1 is not unchunked\nSUMMARY:\nok.")
	if len(syn.Decisions) != 2 {
		t.Fatalf("want 2 decisions, got %d", len(syn.Decisions))
	}
	if syn.Decisions[0].Inferred {
		t.Error("stated decision must not be marked inferred")
	}
	if !syn.Decisions[1].Inferred || syn.Decisions[1].Text != "cert bypasses the atomizer" {
		t.Errorf("inferred decision mis-parsed: %+v", syn.Decisions[1])
	}
	if len(syn.Risks) != 1 || len(syn.Corrections) != 1 {
		t.Errorf("risks=%v corrections=%v", syn.Risks, syn.Corrections)
	}
}
