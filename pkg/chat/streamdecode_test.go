package chat

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const (
	museDelta = `{"schema_version":1,"id":"%s","stream":{"kind":"session","id":"s"},"sequence":%d,"recorded_at":%d,"record_type":"status","payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":%q}}`
	museMeta  = `{"schema_version":1,"id":"%s","stream":{"kind":"session","id":"s"},"sequence":%d,"recorded_at":%d,"record_type":"event","payload_type":"task.lifecycle.started","payload":{"kind":"task_lifecycle","task_id":"t-%d","event":{"kind":"started"}}}`
	museFinal = `{"schema_version":1,"id":"x","stream":{"kind":"session","id":"s"},"sequence":9,"recorded_at":9,"record_type":"event","payload_type":"run.terminal.completed","payload":{"kind":"run_terminal","text":"whole answer"}}`
)

func museMetaLine(n int) string {
	return fmt.Sprintf(museMeta, fmt.Sprintf("00000000-0000-7000-8000-%012d", n), n, 1780531400000000+n, n)
}

func museDeltaLine(n int, text string) string {
	return fmt.Sprintf(museDelta, fmt.Sprintf("00000000-0000-7000-8000-%012d", n), n, 1780531400000000+n, text)
}

func TestDecodeStreamLineMuseFixture(t *testing.T) {
	body, err := os.ReadFile("../cligw/testdata/muse-exec-json-ok.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var deltas, finals string
	for _, ln := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		d := DecodeStreamLine(ln)
		if d.Kind == StreamPlain || d.Vendor != "muse" {
			t.Fatalf("every fixture line is Muse transport, got %+v for %.80s", d, ln)
		}
		switch d.Kind {
		case StreamDelta:
			deltas += d.Text
		case StreamFinal:
			finals += d.Text
		default:
			if d.Text != "" {
				t.Fatalf("meta record exposed text %q", d.Text)
			}
		}
	}
	if deltas != "ok" || finals != "ok" {
		t.Fatalf("deltas=%q finals=%q, want ok/ok", deltas, finals)
	}
}

func TestDecodeStreamLineAGYFixture(t *testing.T) {
	body, err := os.ReadFile("../cligw/testdata/agy-stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var deltas, finals string
	for _, ln := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		d := DecodeStreamLine(ln)
		if d.Kind == StreamPlain || d.Vendor != "agy" {
			t.Fatalf("every fixture line is AGY transport, got %+v for %.80s", d, ln)
		}
		switch d.Kind {
		case StreamDelta:
			deltas += d.Text
		case StreamFinal:
			finals += d.Text
		}
		if strings.Contains(d.Text, "PRIVATE") {
			t.Fatalf("prompt echo or thinking leaked: %q", d.Text)
		}
	}
	if deltas != "ok" || finals != "ok" {
		t.Fatalf("deltas=%q finals=%q, want ok/ok", deltas, finals)
	}
}

func TestDecodeStreamLinePreservesForeignJSON(t *testing.T) {
	for _, ln := range []string{
		`{"answer":"42"}`,
		`{"event":"deploy","status":"done"}`,
		`{"event":{"type":"stream_event"},"session_id":"s"}`,
		`{"payload_type":"run.output.delta","payload":{"text":"no envelope"}}`,
		`{"schema_version":"bashy-hint-v1","kind":"hint"}`,
		`{"event":"step_update","step_update":"not an object"}`,
		`plain prose`,
	} {
		if d := DecodeStreamLine(ln); d.Kind != StreamPlain {
			t.Errorf("%q must stay plain, got %+v", ln, d)
		}
	}
}

// Fragments keep their whitespace: it is part of the answer.
func TestDecodeStreamLineDeltaIsVerbatim(t *testing.T) {
	d := DecodeStreamLine(museDeltaLine(1, " world "))
	if d.Kind != StreamDelta || d.Text != " world " {
		t.Fatalf("got %+v", d)
	}
}

// A productive Muse run is hundreds of envelopes that never repeat byte-for-byte
// (ids, sequence, timestamps) and carry ~1KB of machinery each. None of that is
// progress, and none of it is spend: the coach must stay quiet and report a token
// estimate that reflects the answer, not the wrapping.
func TestCoachIgnoresUniqueEnvelopeIDs(t *testing.T) {
	c := newCoach(DefaultCoachPolicy())
	for i := 0; i < 1200; i++ {
		if recs := c.feedLine(museMetaLine(i), true); len(recs) != 0 {
			t.Fatalf("transport envelope %d tripped the coach: %+v", i, recs[0])
		}
	}
	if c.Unresolved() || len(c.Report().Steers) != 0 {
		t.Fatal("coach steered a run that only emitted lifecycle envelopes")
	}
	if got := c.OutputTokens(); got != 0 {
		t.Fatalf("envelope bytes counted as output tokens: %d", got)
	}
	if rep := c.Report(); rep.Total != 0 {
		t.Fatalf("envelopes counted as output lines: %+v", rep)
	}
}

// Genuine progress streamed as deltas is reassembled into lines and counted once.
func TestCoachReassemblesDeltasIntoLines(t *testing.T) {
	c := newCoach(DefaultCoachPolicy())
	for i, frag := range []string{"Implemented the ", "parser and its ", "tests.\nNext, wiring ", "the sink.\n"} {
		c.feedLine(museDeltaLine(i, frag), true)
	}
	rep := c.Report()
	if rep.Total != 2 || rep.Distinct != 2 {
		t.Fatalf("want 2 reassembled lines, got total=%d distinct=%d", rep.Total, rep.Distinct)
	}
	if got := c.OutputTokens(); got == 0 || got > 20 {
		t.Fatalf("token estimate should reflect answer bytes only, got %d", got)
	}
}

// A real loop wrapped in structured events — the same action re-emitted with a
// fresh id and sequence each time — must still trip: unique ids are not novelty.
func TestCoachDetectsRepeatedStructuredEventsDespiteIDs(t *testing.T) {
	pol := DefaultCoachPolicy()
	pol.SoftTokenBudget, pol.SoftStepBudget = 0, 0
	c := newCoach(pol)
	tripped := false
	for i := 0; i < 80 && !tripped; i++ {
		ln := fmt.Sprintf(`{"id":"evt-%d","seq":%d,"ts":%d,"type":"tool_call","name":"read_file","input":{"path":"main.go"},"call_id":"call_%d"}`, i, i, 1000+i, i)
		tripped = len(c.feedLine(ln, true)) > 0
	}
	if !tripped {
		t.Fatal("80 repeats of one action never tripped the coach: ids were read as progress")
	}
	if rep := c.Report(); rep.Distinct != 1 {
		t.Fatalf("one repeated action must be 1 distinct, got %d", rep.Distinct)
	}
}

// Repeating the same answer line through delta fragments is a loop too.
func TestCoachDetectsRepeatedAnswerDeltas(t *testing.T) {
	pol := DefaultCoachPolicy()
	pol.SoftTokenBudget, pol.SoftStepBudget = 0, 0
	c := newCoach(pol)
	tripped := false
	for i := 0; i < 200 && !tripped; i++ {
		// Alternate two lines so the consecutive-redraw dedup does not hide the loop.
		line := "Retrying the failing build step again\n"
		if i%2 == 1 {
			line = "Still nothing different to report here\n"
		}
		tripped = len(c.feedLine(museDeltaLine(i, line), true)) > 0
	}
	if !tripped {
		t.Fatal("a repeated answer streamed as deltas never tripped the coach")
	}
}

// Distinct work in structured events is not a loop.
func TestCoachQuietOnDistinctStructuredActions(t *testing.T) {
	c := newCoach(DefaultCoachPolicy())
	for i := 0; i < 60; i++ {
		ln := fmt.Sprintf(`{"id":"evt-%d","seq":%d,"type":"tool_call","name":"read_file","input":{"path":"pkg/file%d.go"}}`, i, i, i)
		if recs := c.feedLine(ln, true); len(recs) != 0 {
			t.Fatalf("distinct action %d tripped: %+v", i, recs[0])
		}
	}
}

// A key named "error" holding null is the absence of an error.
func TestSemanticJSONLineErrorIsNotKeywordMatch(t *testing.T) {
	if _, _, isErr, ok := semanticJSONLine(`{"type":"tool_result","error":null,"is_error":false,"output":"fine"}`); !ok || isErr {
		t.Fatalf("healthy envelope read as failure: ok=%v isErr=%v", ok, isErr)
	}
	if _, _, isErr, _ := semanticJSONLine(`{"type":"tool_result","is_error":true}`); !isErr {
		t.Fatal("is_error:true must read as failure")
	}
}
