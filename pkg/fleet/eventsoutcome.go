package fleet

// WHAT THE TURN ACTUALLY DID — read from the stream, not inferred from an exit.
//
// This tree already learned the expensive version of this lesson: "all three
// harnesses EXITED 0 WHEN THEY FAILED" (dhnt/CLAUDE.md, from the three-harness
// A/B). An agent CLI that gave up, hit a limit, or answered the wrong question
// still returns 0, so a caller reading the exit code learns nothing and reports
// success.
//
// The structured stream carries the fact the exit code does not. Measured
// 2026-07-31 on the terminal events:
//
//	claude   {"type":"result","subtype":"success","is_error":false, ...}
//	agy      {"event":"result","result":{"status":"SUCCESS", ...}}
//	codex    {"type":"turn.completed","usage":{...}}      ← no verdict field seen
//
// # Only SUCCESS is declarable, and absence is never failure
//
// The measured runs all succeeded, so the success spellings are facts and the
// FAILURE spellings are not — nobody has seen one. Declaring a guess at them
// would put an unverified string on the one path whose entire purpose is to
// stop trusting unverified claims.
//
// So OK lists the values known to mean success, and anything else is
// UNVERIFIED — not failure. That asymmetry is the fleet-evidence rule applied
// to itself: absence of a success signal is not evidence of failure, and a
// caller must be able to tell "it said it failed" from "it did not say". The
// second still needs a gate; the first does not.
//
// codex declares no path at all, because reaching turn.completed is the only
// signal observed and it has not been seen to emit a failing terminal event. A
// tool with no declared path yields Unverified, which is the honest answer.

import (
	"encoding/json"
	"strings"
)

// EventsOutcome locates the verdict inside a tool's terminal event.
type EventsOutcome struct {
	// Path is a dotted path into the terminal event object — "is_error" for
	// claude, "result.status" for agy. Nesting differs per tool, so the path is
	// declared rather than assumed.
	Path string `yaml:"path,omitempty" json:"path,omitempty" doc:"dotted path to the outcome value"`
	// OK are the values at Path that MEAN success, rendered as strings so one
	// declaration covers a bool (`false`) and an enum (`SUCCESS`) alike.
	OK []string `yaml:"ok,omitempty" json:"ok,omitempty" doc:"values that mean success"`
}

// Verdict is what a terminal event says about the turn.
type Verdict string

const (
	// VerdictSucceeded — the tool reported success, in its own words.
	VerdictSucceeded Verdict = "succeeded"
	// VerdictUnverified — no success signal was found. NOT failure: the tool
	// may have failed, or may simply not say. Either way the caller still owes
	// a gate, which is the same conclusion the evidence invariant reaches.
	VerdictUnverified Verdict = "unverified"
)

// Declared reports an outcome rule worth consulting.
func (o EventsOutcome) Declared() bool { return strings.TrimSpace(o.Path) != "" && len(o.OK) > 0 }

// Read extracts the verdict from a terminal event line.
//
// Everything that is not a recognised success reads as Unverified — an
// unparseable line, a missing path, a value nobody declared. There is
// deliberately no way for this to return "failed": that would require a failure
// spelling nobody has observed, and inventing one is exactly the unverified
// claim this function exists to replace.
func (o EventsOutcome) Read(line []byte) Verdict {
	if !o.Declared() {
		return VerdictUnverified
	}
	var obj any
	if err := json.Unmarshal(line, &obj); err != nil {
		return VerdictUnverified
	}
	got, ok := lookupPath(obj, strings.Split(o.Path, "."))
	if !ok {
		return VerdictUnverified
	}
	for _, want := range o.OK {
		if got == want {
			return VerdictSucceeded
		}
	}
	return VerdictUnverified
}

// lookupPath walks a dotted path and renders the leaf as a string, so a bool
// and an enum compare the same way.
func lookupPath(v any, parts []string) (string, bool) {
	for _, p := range parts {
		m, ok := v.(map[string]any)
		if !ok {
			return "", false
		}
		v, ok = m[p]
		if !ok {
			return "", false
		}
	}
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}
