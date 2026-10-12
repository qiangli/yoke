package chat

import (
	"fmt"
	"os"
	"regexp"
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
		`{"event":"init","init":{"x":1}}`,
		`{"event":"init","init":{}}`,
		`{"event":"step_update","step_update":{"text_delta":"an answer"}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"no index or state"}}`,
		`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"x"},"owner":"other"}`,
		`{"event":"result","result":{"response":"42"}}`,
		`{"event":"result","result":{"status":"ok"},"extra":1}`,
		`{"event":"deploy","deploy":{"model":"m"}}`,
		`{"schema_version":1,"stream":{"kind":"x"},"payload":{"k":1}}`,
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

// --- semantic activity: tool/error records survive inside Muse transport ----

const (
	museTool = `{"schema_version":1,"id":"%s","stream":{"kind":"session","id":"s"},"sequence":%d,"recorded_at":%d,"record_type":"event","payload_type":"%s","payload":{"kind":"tool","command_id":"c-%d","tool_call_id":"call-%d","name":"run_tests","input":{"cmd":"go test ./..."},"is_error":%t,"output":%q,"duration_ms":%d}}`
	museFail = `{"schema_version":1,"id":"%s","stream":{"kind":"session","id":"s"},"sequence":%d,"recorded_at":%d,"record_type":"event","payload_type":"task.lifecycle.failed","payload_schema_version":1,"payload":{"kind":"task_lifecycle","command_id":"c","task_id":"t-%d","event":{"kind":"failed","task_id":"t-%d","error":"build step exited 2"}}}`
)

var reSequence = regexp.MustCompile(`"sequence":\d+,`)

func museToolLine(n int, payloadType string, isErr bool, out string) string {
	return fmt.Sprintf(museTool, fmt.Sprintf("00000000-0000-7000-8000-%012d", n), n, 1780531400000000+n, payloadType, n, n, isErr, out, 10+n)
}

func museFailLine(n int) string {
	return fmt.Sprintf(museFail, fmt.Sprintf("00000000-0000-7000-8000-%012d", n), n, 1780531400000000+n, n, n)
}

// Every non-answer record of the real Muse fixture is lifecycle machinery: no
// activity, so a healthy run can never be mistaken for a loop of it.
func TestDecodeStreamLineMuseFixtureHasNoActivity(t *testing.T) {
	body, err := os.ReadFile("../cligw/testdata/muse-exec-json-ok.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if d := DecodeStreamLine(ln); d.Kind == StreamMeta && (d.Activity != "" || d.Failed) {
			t.Fatalf("lifecycle record read as activity: %+v for %.100s", d, ln)
		}
	}
}

// Tool and failure records are activity; identity, timing and thinking/usage are not.
func TestDecodeStreamLineMuseActivityClassification(t *testing.T) {
	a := DecodeStreamLine(museToolLine(1, "tool.call.completed", true, "exit 1"))
	b := DecodeStreamLine(museToolLine(2, "tool.call.completed", true, "exit 1"))
	if a.Kind != StreamMeta || a.Activity == "" || !a.Failed {
		t.Fatalf("failed tool call must be failed activity: %+v", a)
	}
	if a.Activity != b.Activity {
		t.Fatalf("ids/timestamps/duration leaked into the signature:\n%s\n%s", a.Activity, b.Activity)
	}
	if c := DecodeStreamLine(museToolLine(3, "tool.call.completed", true, "exit 2")); c.Activity == a.Activity {
		t.Fatal("a different error must be a different activity")
	}
	if ok := DecodeStreamLine(museToolLine(4, "tool.call.started", false, "")); ok.Activity == "" || ok.Failed {
		t.Fatalf("a healthy tool call is activity but not failure: %+v", ok)
	}
	if f := DecodeStreamLine(museFailLine(5)); f.Activity == "" || !f.Failed {
		t.Fatalf("a failed task lifecycle is failed activity: %+v", f)
	}
	for _, pt := range []string{"tool.thinking.delta", "run.usage.updated", "run.thought.delta"} {
		if d := DecodeStreamLine(museToolLine(6, pt, false, "x")); d.Activity != "" || d.Failed {
			t.Fatalf("%s is interior state, got %+v", pt, d)
		}
	}
}

// A productive run: the real fixture's envelope sequence replayed hundreds of
// times with fresh ids/sequence/timestamps never trips, and costs nothing.
func TestCoachQuietOnReplayedRealMuseFixture(t *testing.T) {
	body, err := os.ReadFile("../cligw/testdata/muse-exec-json-ok.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	c := newCoach(DefaultCoachPolicy())
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	for round := 0; round < 30; round++ {
		for i, ln := range lines {
			ln = strings.ReplaceAll(ln, "00000000-0000-7000-8000-0000000000", fmt.Sprintf("%08x-0000-7000-8000-0000000000", round))
			ln = reSequence.ReplaceAllString(ln, fmt.Sprintf(`"sequence":%d,`, round*1000+i))
			if d := DecodeStreamLine(ln); d.Kind == StreamDelta || d.Kind == StreamFinal {
				continue // the answer itself is exercised elsewhere
			}
			if recs := c.feedLine(ln, true); len(recs) != 0 {
				t.Fatalf("round %d line %d tripped on lifecycle transport: %+v", round, i, recs[0])
			}
		}
	}
	if c.Report().Total != 0 || c.OutputTokens() != 0 {
		t.Fatalf("lifecycle transport counted: %+v tokens=%d", c.Report(), c.OutputTokens())
	}
}

// The same failing tool call re-emitted with fresh ids is a real loop and must
// still trip, with the failures visible to the error-ratio signal.
func TestCoachDetectsRepeatedFailedMuseToolDespiteIDs(t *testing.T) {
	pol := DefaultCoachPolicy()
	pol.SoftTokenBudget, pol.SoftStepBudget = 0, 0
	c := newCoach(pol)
	tripped := false
	for i := 0; i < 80 && !tripped; i++ {
		tripped = len(c.feedLine(museToolLine(i, "tool.call.completed", true, "go: cannot find main module"), true)) > 0
	}
	if !tripped {
		t.Fatal("80 identical failing tool calls inside Muse transport never tripped the coach")
	}
	if rep := c.Report(); rep.Distinct != 1 {
		t.Fatalf("one repeated action must be 1 distinct, got %d", rep.Distinct)
	}
}

func TestCoachDetectsRepeatedMuseTaskFailureAmidLifecycle(t *testing.T) {
	pol := DefaultCoachPolicy()
	pol.SoftTokenBudget, pol.SoftStepBudget = 0, 0
	c := newCoach(pol)
	tripped := false
	for i := 0; i < 80 && !tripped; i++ {
		c.feedLine(museMetaLine(i), true) // healthy lifecycle noise in between
		tripped = len(c.feedLine(museFailLine(i), true)) > 0
	}
	if !tripped {
		t.Fatal("a task that keeps failing never tripped the coach")
	}
}

// Distinct tool work through Muse transport is progress, not a loop.
func TestCoachQuietOnDistinctMuseToolCalls(t *testing.T) {
	c := newCoach(DefaultCoachPolicy())
	for i := 0; i < 60; i++ {
		ln := strings.Replace(museToolLine(i, "tool.call.completed", false, "ok"), "go test ./...", fmt.Sprintf("go test ./pkg/p%d", i), 1)
		if recs := c.feedLine(ln, true); len(recs) != 0 {
			t.Fatalf("distinct tool call %d tripped: %+v", i, recs[0])
		}
	}
}
