package fleet

import "testing"

// THE LINES ARE REAL. Each is a verbatim event captured from the tool on
// 2026-07-31 — not a shape inferred from its --help, which is how a matcher
// ends up asserting a spelling nobody emits.
func TestEventsDone_MatchesEachToolsRealSpelling(t *testing.T) {
	cases := []struct {
		tool string
		done EventsDone
		end  string
		mid  []string
	}{
		{
			tool: "claude",
			done: EventsDone{Field: "type", Values: []string{"result"}},
			end:  `{"type":"result","subtype":"success","stop_reason":"end_turn","is_error":false}`,
			mid: []string{
				`{"type":"system","subtype":"init","cwd":"/tmp"}`,
				`{"type":"assistant","message":{"role":"assistant"}}`,
				`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
			},
		},
		{
			tool: "codex",
			done: EventsDone{Field: "type", Values: []string{"turn.completed"}},
			end:  `{"type":"turn.completed","usage":{"input_tokens":13905}}`,
			mid: []string{
				`{"type":"thread.started","thread_id":"019f"}`,
				`{"type":"turn.started"}`,
				`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`,
			},
		},
		{
			// The one that would break a `type`-only matcher, silently.
			tool: "agy",
			done: EventsDone{Field: "event", Values: []string{"result"}},
			end:  `{"event":"result","result":{"status":"SUCCESS","num_turns":1}}`,
			mid: []string{
				`{"event":"step_update","step_update":{"state":"ACTIVE"}}`,
				`{"event":"step_update","step_update":{"state":"DONE"}}`,
			},
		},
	}
	for _, c := range cases {
		if !c.done.Match([]byte(c.end)) {
			t.Errorf("%s: the real terminal event was NOT matched: %s", c.tool, c.end)
		}
		for _, m := range c.mid {
			if c.done.Match([]byte(m)) {
				t.Errorf("%s: a mid-turn event ended the turn early: %s", c.tool, m)
			}
		}
	}
}

// AGY IS WHY THE FIELD IS DECLARED. A matcher hard-coded to `type` never fires
// on agy, and the failure is silent: a boundary that never arrives looks exactly
// like a tool still thinking, so the reader falls back to the silence heuristic
// it was meant to replace and reports nothing.
func TestEventsDone_WrongFieldNeverFires(t *testing.T) {
	agyEnd := []byte(`{"event":"result","result":{"status":"SUCCESS"}}`)
	if (EventsDone{Field: "type", Values: []string{"result"}}).Match(agyEnd) {
		t.Fatal("a type-keyed matcher must not match agy — this test exists to prove the field is load-bearing")
	}
	if !(EventsDone{Field: "event", Values: []string{"result"}}).Match(agyEnd) {
		t.Error("the event-keyed matcher should match")
	}
}

// An undeclared matcher matches NOTHING. Treating it as "matches everything"
// would end every turn on its first event — worse than the heuristic, and
// instant.
func TestEventsDone_UndeclaredMatchesNothing(t *testing.T) {
	var none EventsDone
	if none.Declared() {
		t.Error("an empty matcher is not declared")
	}
	for _, line := range []string{`{"type":"result"}`, `{"type":"turn.completed"}`, `{}`} {
		if none.Match([]byte(line)) {
			t.Errorf("an undeclared matcher matched %s", line)
		}
	}
}

// Tools emit banners, warnings and progress noise on the same stream. Treating
// an unparseable line as a boundary would cut a run off mid-thought — the same
// class of error as the silence heuristic, arriving faster.
func TestEventsDone_NoiseIsNotABoundary(t *testing.T) {
	d := EventsDone{Field: "type", Values: []string{"result"}}
	for _, line := range []string{
		"Reading additional input from stdin...",
		"",
		"{ not json",
		`{"type":123}`,       // kind is not a string
		`{"other":"result"}`, // right value, wrong key
	} {
		if d.Match([]byte(line)) {
			t.Errorf("noise treated as a turn boundary: %q", line)
		}
	}
}

// The declarations must actually be present on the shipped tools, or the
// measurement above is documentation of something nothing uses.
func TestBaseline_ThreeToolsReportTurnEndOnStdout(t *testing.T) {
	cat := New()
	for _, name := range []string{"claude", "codex", "agy"} {
		tool, ok := cat.Tool(name)
		if !ok {
			t.Errorf("%s missing from the baseline", name)
			continue
		}
		if !tool.EventsOnStdout() {
			t.Errorf("%s: no events_stdout — it would still pay the 25s silence tax", name)
		}
		if !tool.CLI.Launch.EventsDone.Declared() {
			t.Errorf("%s: streams events but never says which one ends a turn", name)
		}
		if !tool.ReportsTurnEnd() {
			t.Errorf("%s: ReportsTurnEnd is false despite a declared stdout stream", name)
		}
	}
}

// THE EXIT CODE IS NOT THE VERDICT — "all three harnesses EXITED 0 WHEN THEY
// FAILED" is recorded in the umbrella's own notes. These lines are the real
// terminal events captured 2026-07-31, and the point is that the stream says
// something the status does not.
func TestEventsOutcome_ReadsTheVerdictFromTheRealEvent(t *testing.T) {
	claude := EventsOutcome{Path: "is_error", OK: []string{"false"}}
	if got := claude.Read([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`)); got != VerdictSucceeded {
		t.Errorf("claude success = %v, want succeeded", got)
	}
	// A bool and an enum must compare the same way, which is why the leaf is
	// rendered as a string.
	if got := claude.Read([]byte(`{"type":"result","is_error":true}`)); got != VerdictUnverified {
		t.Errorf("claude is_error:true = %v, want unverified", got)
	}

	// agy nests one level deeper — the reason the path is declared, not assumed.
	agy := EventsOutcome{Path: "result.status", OK: []string{"SUCCESS"}}
	if got := agy.Read([]byte(`{"event":"result","result":{"status":"SUCCESS","num_turns":1}}`)); got != VerdictSucceeded {
		t.Errorf("agy success = %v, want succeeded", got)
	}
	if got := agy.Read([]byte(`{"event":"result","result":{"status":"CANCELLED"}}`)); got != VerdictUnverified {
		t.Errorf("agy CANCELLED = %v — an undeclared value is unverified", got)
	}
}

// THERE IS NO WAY TO RETURN "FAILED", and that is the design. Only the success
// spellings were observed; a failure spelling would be a guess, and putting an
// unverified string on the path whose purpose is to stop trusting unverified
// claims would defeat the exercise. Absence of success is not evidence of
// failure — it means the caller still owes a gate.
func TestEventsOutcome_AbsenceIsNeverFailure(t *testing.T) {
	o := EventsOutcome{Path: "is_error", OK: []string{"false"}}
	for _, line := range []string{
		`{"type":"result"}`,          // path missing
		`{"is_error":"maybe"}`,       // value nobody declared
		`{"result":{"nested":true}}`, // wrong shape
		"not json",
		"",
	} {
		if got := o.Read([]byte(line)); got != VerdictUnverified {
			t.Errorf("%q = %v, want unverified — never a failure verdict", line, got)
		}
	}
	// And an undeclared rule says nothing at all rather than defaulting either way.
	var none EventsOutcome
	if none.Declared() {
		t.Error("an empty rule is not declared")
	}
	if got := none.Read([]byte(`{"type":"result","is_error":false}`)); got != VerdictUnverified {
		t.Errorf("undeclared = %v, want unverified", got)
	}
}

// codex declares no outcome path on purpose: turn.completed carries usage and
// no verdict, and no failing terminal event has been observed. Pinning it stops
// somebody "completing" the table with a guess.
func TestBaseline_CodexHasNoGuessedOutcome(t *testing.T) {
	cat := New()
	codex, ok := cat.Tool("codex")
	if !ok {
		t.Skip("codex not in the baseline")
	}
	if codex.CLI.Launch.EventsOutcome.Declared() {
		t.Error("codex declares an outcome path — none was ever measured, so it can only be a guess")
	}
	// The tools whose success WAS measured must carry it.
	for _, name := range []string{"claude", "agy"} {
		tool, ok := cat.Tool(name)
		if !ok {
			continue
		}
		if !tool.CLI.Launch.EventsOutcome.Declared() {
			t.Errorf("%s: measured success spelling is not declared", name)
		}
	}
}
