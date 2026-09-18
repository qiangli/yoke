package fleet

// HOW A TOOL SAYS "THE TURN ENDED".
//
// Every agent CLI that streams structured events announces a completed turn,
// and no two spell it the same way. Measured on the wire 2026-07-31:
//
//	ycode    {"type":"turn.end"}
//	codex    {"type":"turn.completed","usage":{…}}
//	claude   {"type":"result","subtype":"success","stop_reason":"end_turn"}
//	agy      {"event":"result","result":{"status":"SUCCESS",…}}
//
// Note the KEY differs, not only the value: agy nests its kind under `event`
// where the others use `type`. A matcher hard-coded to `type` would never fire
// on agy — and would do so SILENTLY, because a boundary that never arrives
// looks exactly like a tool still thinking. The reader would quietly fall back
// to the 25-second silence heuristic it was meant to replace, and report
// nothing. That failure mode is the reason this is declared per tool rather
// than sniffed.

import (
	"encoding/json"
	"strings"
)

// EventsDone matches the event that means a turn finished.
type EventsDone struct {
	// Field is the JSON key carrying the event kind — "type" for most, "event"
	// for agy. Empty defaults to "type", which is the majority spelling.
	Field string `yaml:"field,omitempty" json:"field,omitempty" doc:"JSON key carrying the event kind"`
	// Values are the kinds that mean the turn ENDED. Any match is a boundary.
	Values []string `yaml:"values,omitempty" json:"values,omitempty" doc:"event kinds that end a turn"`
}

// Declared reports a usable matcher. An EventsDone with no values matches
// nothing, and must never be treated as "matches everything" — that would end
// every turn on its first event.
func (d EventsDone) Declared() bool { return len(d.Values) > 0 }

// key is the JSON field to read, defaulting to the majority spelling.
func (d EventsDone) key() string {
	if f := strings.TrimSpace(d.Field); f != "" {
		return f
	}
	return "type"
}

// Match reports whether one NDJSON line announces the end of a turn.
//
// A line that is not JSON, or carries no kind, is NOT a boundary. Tools emit
// banners, warnings and progress noise on the same stream, and treating an
// unparseable line as a turn end would cut a run off mid-thought — the same
// class of error as the silence heuristic, arriving faster.
func (d EventsDone) Match(line []byte) bool {
	if !d.Declared() {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(line, &obj); err != nil {
		return false
	}
	raw, ok := obj[d.key()]
	if !ok {
		return false
	}
	var kind string
	if err := json.Unmarshal(raw, &kind); err != nil {
		return false
	}
	for _, v := range d.Values {
		if kind == v {
			return true
		}
	}
	return false
}

// StreamsEvents reports a tool that emits structured events at all, by either
// route — a side-channel file or its own stdout.
func (t Tool) StreamsEvents() bool {
	return t.HasEventsArg() || strings.TrimSpace(t.CLI.Launch.EventsStdout) != ""
}

// EventsOnStdout reports a tool whose event stream IS its stdout.
//
// The distinction matters to the launcher, not just to the argv: for these
// tools stdout stops being a transcript to scrape and becomes a stream to
// parse, so it must be a pipe rather than a pty and nothing else may write to
// it.
func (t Tool) EventsOnStdout() bool {
	return strings.TrimSpace(t.CLI.Launch.EventsStdout) != ""
}
