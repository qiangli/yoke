// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package judge renders a SEMANTIC verdict on a piece of work: is it any good?
//
// # gate decides pass/fail. judge decides good/bad.
//
// bashy could VERIFY but it could not JUDGE. `gate` runs a command and lets the exit
// status be the verdict — mechanical, reproducible, and exactly right for "do the tests
// pass". `weave review` sounds like the missing half and isn't: it clones the workspace
// to a clean room and RE-RUNS the verify command, never launching an agent. It is a
// re-verification (and `weave reverify` already existed doing nearly the same thing).
// Nothing in the tree ever read a diff, a plan, or a failure and formed an opinion.
//
// The role existed anyway — as ad-hoc prompting. The proof is in this project's own
// docs: JUDGE-REPORT-R6.md, JUDGE-REPORT-R7.md, QA-REPORT-R10.md. A role with
// artifacts, campaigns, and no verb. This is the verb.
//
//	gate   — does it PASS?    mechanical · reproducible · safe to block a merge on
//	judge  — is it GOOD?      semantic   · an opinion    · advisory unless you say otherwise
//
// Together they finally encode in two commands what the conductor playbook keeps
// writing in prose: SANDBOX-GREEN IS NOT MERGEABLE.
//
// # Independent, not deliberative — and that is why this is not `meet`
//
// The panel does not confer. Each reviewer sees the artifact cold and never sees
// another's opinion. That is a deliberate rejection of the obvious design (route it
// through `meet`, which already has a chair, turns and polls): deliberation produces
// ANCHORING — the first opinion voiced drags the rest toward it, and a panel that
// converges is not the same as a panel that agrees. For judging, independence is the
// whole value. `meet` is for reaching a decision together; `judge` is for finding out
// whether N reviewers independently see the same problem.
//
// # A failed reviewer is never an approval
//
// If a reviewer errors, times out, or returns something unparseable, its opinion is an
// ERROR — it is not counted, and it is not silently treated as consent. The failure
// mode this prevents is the one that matters: a panel where two of three reviewers
// crashed, the third said "looks fine", and the work merged on what looked like
// unanimous approval.
package judge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the stable envelope name.
const SchemaVersion = "bashy-judge-v1"

// Verdict is the closed vocabulary a reviewer may return.
//
// Three, not a 1-10 score. A number invites averaging, and the average of "this is
// wrong" and "this is fine" is a meaningless 5 that nobody can act on. Each of these
// says what to DO next.
type Verdict string

const (
	Approve Verdict = "approve" // ship it
	Revise  Verdict = "revise"  // fixable problems, named below
	Reject  Verdict = "reject"  // wrong approach; revising will not save it
	Errored Verdict = "error"   // the reviewer failed — NOT an opinion, and never consent
)

// severity ranks a finding. "blocker" is the only one that can force a verdict.
const (
	SevBlocker = "blocker"
	SevMajor   = "major"
	SevMinor   = "minor"
	SevNit     = "nit"
)

func ValidVerdict(v Verdict) bool {
	switch v {
	case Approve, Revise, Reject:
		return true
	}
	return false
}

// severity of a verdict, for the conservative tie-break.
func rank(v Verdict) int {
	switch v {
	case Reject:
		return 3
	case Revise:
		return 2
	case Approve:
		return 1
	}
	return 0
}

// Finding is one concrete problem. A judge that says "looks a bit off" is noise; a
// finding names a place and a defect.
type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Summary  string `json:"summary"`
}

// Opinion is ONE reviewer's independent verdict.
type Opinion struct {
	Agent    string    `json:"agent"`
	Verdict  Verdict   `json:"verdict"`
	Findings []Finding `json:"findings,omitempty"`
	Notes    string    `json:"notes,omitempty"`
	Error    string    `json:"error,omitempty"` // set iff Verdict == Errored
	Took     string    `json:"took,omitempty"`
}

// Report is the panel's combined judgment.
type Report struct {
	SchemaVersion string    `json:"schema_version"`
	Subject       string    `json:"subject"`
	Stage         string    `json:"stage"`
	Verdict       Verdict   `json:"verdict"`
	Unanimous     bool      `json:"unanimous"`
	Panel         []Opinion `json:"panel"`
	Findings      []Finding `json:"findings,omitempty"`
	Errors        int       `json:"errors,omitempty"`
	JudgedAt      time.Time `json:"judged_at"`
}

// Combine reduces N independent opinions to one verdict.
//
// Majority wins. On a tie the MORE CONSERVATIVE verdict wins, because a split panel on
// a change is not an endorsement of it — if one competent reviewer says this is wrong
// and another says it is fine, the honest summary is "not clearly good", not "approved".
//
// Errored reviewers are EXCLUDED from the vote and counted separately. A panel whose
// reviewers all failed has NO verdict — it reports "error", never "approve". Silence is
// not consent.
func Combine(ops []Opinion) Report {
	r := Report{
		SchemaVersion: SchemaVersion,
		Panel:         ops,
		JudgedAt:      time.Now().UTC(),
	}
	votes := map[Verdict]int{}
	var voters []Opinion
	for _, o := range ops {
		if o.Verdict == Errored || !ValidVerdict(o.Verdict) {
			r.Errors++
			continue
		}
		votes[o.Verdict]++
		voters = append(voters, o)
		r.Findings = append(r.Findings, o.Findings...)
	}
	if len(voters) == 0 {
		// Every reviewer failed. This is NOT an approval, and it must be loud.
		r.Verdict = Errored
		return r
	}

	best, bestN := Verdict(""), 0
	for v, n := range votes {
		switch {
		case n > bestN:
			best, bestN = v, n
		case n == bestN && rank(v) > rank(best):
			best = v // tie -> the more conservative verdict
		}
	}
	r.Verdict = best
	// Unanimous means EVERY reviewer agreed — so a panel with errors is never unanimous,
	// even when everyone who survived said the same thing. Two of three reviewers timing
	// out and the third saying "looks fine" is a thin vote, and reporting it as consensus
	// is how a merge gets waved through on one opinion wearing three hats.
	r.Unanimous = votes[best] == len(voters) && r.Errors == 0

	// A blocker finding cannot coexist with "approve". A reviewer that names a blocker
	// and then approves has contradicted itself; take the finding, not the label.
	if r.Verdict == Approve {
		for _, f := range r.Findings {
			if f.Severity == SevBlocker {
				r.Verdict = Revise
				break
			}
		}
	}
	sortFindings(r.Findings)
	return r
}

func sortFindings(f []Finding) {
	order := map[string]int{SevBlocker: 0, SevMajor: 1, SevMinor: 2, SevNit: 3}
	sort.SliceStable(f, func(i, j int) bool { return order[f[i].Severity] < order[f[j].Severity] })
}

// Blocking reports whether this verdict should stop the work, when the caller has asked
// for the verdict to be binding (`--gate`).
func (r Report) Blocking() bool {
	return r.Verdict != Approve
}

// ---------------------------------------------------------------------------
// Parsing a reviewer's answer
// ---------------------------------------------------------------------------

// ParseOpinion extracts a verdict from a reviewer's raw output.
//
// Agent CLIs wrap JSON in prose, in ``` fences, or in a "Here is my review:" preamble —
// so this hunts for the first balanced JSON object rather than demanding a clean parse.
//
// It is DELIBERATELY strict about one thing: if no verdict can be read, the result is an
// ERROR, never a default. A parser that quietly defaulted to "approve" would turn every
// malformed answer into consent, which is the exact failure this package refuses to have.
func ParseOpinion(agent, raw string) Opinion {
	obj := extractJSON(raw)
	if obj == "" {
		return Opinion{Agent: agent, Verdict: Errored,
			Error: "no JSON verdict found in the reviewer's output"}
	}
	var got struct {
		Verdict  string    `json:"verdict"`
		Findings []Finding `json:"findings"`
		Notes    string    `json:"notes"`
	}
	if err := json.Unmarshal([]byte(obj), &got); err != nil {
		return Opinion{Agent: agent, Verdict: Errored, Error: "unparseable verdict: " + err.Error()}
	}
	v := Verdict(strings.ToLower(strings.TrimSpace(got.Verdict)))
	if !ValidVerdict(v) {
		return Opinion{Agent: agent, Verdict: Errored,
			Error: fmt.Sprintf("reviewer returned an unknown verdict %q (want approve|revise|reject)", got.Verdict)}
	}
	for i := range got.Findings {
		if got.Findings[i].Severity == "" {
			got.Findings[i].Severity = SevMinor
		}
	}
	return Opinion{Agent: agent, Verdict: v, Findings: got.Findings, Notes: got.Notes}
}

// extractJSON returns the first balanced {...} object in s, ignoring braces inside
// strings. A naive first-{-to-last-} slice breaks on prose that follows the object, and
// a regexp cannot balance braces at all.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// brace inside a string literal is not structure
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
