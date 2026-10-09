package fleet

// THE EXIT CODE THAT LIES, the other way round.
//
// eventsoutcome.go declares only SUCCESS, because at the time no failure line
// had been observed. One has now. opencode 1.18.30 (Homebrew), measured on
// 2026-09-29 and again 2026-10-08, failed EVERY run — any provider, any
// --model, no --model, a neutral cwd, an empty HOME — and exited 0 each time,
// printing this on its stdout stream:
//
//	{"type":"error","error":{"name":"UnknownError","data":{"message":"Unexpected server error. Check server logs for details."}}}
//
// A caller gating on the exit status read a total failure as success. weave
// survived only because it also requires commits ahead. `events_fail` is the
// recipe's way to say "this event kind IS a failure": a launcher that sees
// one on an otherwise clean exit reports a non-zero exit with the line, so
// every caller — not only the ones that also count commits — learns the truth.
//
// The asymmetry with EventsOutcome is deliberate and preserved: a declared
// fail kind is evidence of failure; its ABSENCE is still not evidence of
// success.

import (
	"bufio"
	"bytes"
	"strings"
)

// FailedEvent scans a tool's captured stdout for the first line whose event
// kind the recipe declares as a failure. ("", false) when none is declared or
// none matched. Non-JSON lines are ignored, exactly as EventsDone ignores
// them: banner text is never a verdict.
func (t Tool) FailedEvent(out []byte) (string, bool) {
	d := t.CLI.Launch.EventsFail
	return d.FirstMatch(out)
}

// FirstMatch returns the first line of out the matcher accepts.
func (d EventsDone) FirstMatch(out []byte) (string, bool) {
	if !d.Declared() {
		return "", false
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		if d.Match(line) {
			return strings.TrimSpace(string(line)), true
		}
	}
	return "", false
}
