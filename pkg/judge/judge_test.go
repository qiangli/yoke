// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package judge

import (
	"strings"
	"testing"
)

func op(agent string, v Verdict, f ...Finding) Opinion {
	return Opinion{Agent: agent, Verdict: v, Findings: f}
}

// THE SAFETY PROPERTY. A reviewer that crashed did not approve anything.
//
// The failure this prevents is precise and plausible: a panel of three where two
// reviewers time out, the third says "looks fine", and the work merges on what reads
// like consensus. Silence is not consent.
func TestAFailedReviewerIsNeverAnApproval(t *testing.T) {
	r := Combine([]Opinion{
		{Agent: "codex", Verdict: Errored, Error: "timeout"},
		{Agent: "gemini", Verdict: Errored, Error: "exit 1"},
		op("claude", Approve),
	})
	if r.Errors != 2 {
		t.Fatalf("errors = %d, want 2", r.Errors)
	}
	if r.Unanimous {
		t.Fatal("a panel where 2 of 3 reviewers FAILED reported itself unanimous — that reads as consensus and is not one")
	}
	if r.Verdict != Approve {
		t.Fatalf("verdict = %q; the one reviewer that ran said approve", r.Verdict)
	}
	// And the errors must be visible in the report, not swallowed.
	if len(r.Panel) != 3 {
		t.Fatal("the failed reviewers vanished from the panel — the reader cannot see the vote was thin")
	}
}

// A panel where EVERY reviewer failed has NO verdict. It must never fall through to
// approve, and it must be distinguishable from a rejection: a conductor RETRIES a judge
// failure and does NOT retry a rejection.
func TestAnAllFailedPanelHasNoVerdict(t *testing.T) {
	r := Combine([]Opinion{
		{Agent: "a", Verdict: Errored, Error: "boom"},
		{Agent: "b", Verdict: Errored, Error: "boom"},
	})
	if r.Verdict != Errored {
		t.Fatalf("verdict = %q, want %q — an all-failed panel that reports any real verdict is lying", r.Verdict, Errored)
	}
	if r.Verdict == Approve {
		t.Fatal("a panel of total failure APPROVED the work")
	}
}

// Majority wins.
func TestMajorityWins(t *testing.T) {
	r := Combine([]Opinion{op("a", Approve), op("b", Approve), op("c", Reject)})
	if r.Verdict != Approve {
		t.Fatalf("verdict = %q, want approve (2-1)", r.Verdict)
	}
	if r.Unanimous {
		t.Fatal("a 2-1 split reported itself unanimous")
	}
}

// A TIE breaks toward the more conservative verdict. If one competent reviewer says
// this is wrong and another says it is fine, the honest summary is "not clearly good" —
// not "approved". A split panel is not an endorsement.
func TestATieBreaksConservative(t *testing.T) {
	r := Combine([]Opinion{op("a", Approve), op("b", Reject)})
	if r.Verdict != Reject {
		t.Fatalf("a 1-1 approve/reject tie resolved to %q — a split panel must not read as approval", r.Verdict)
	}
	r = Combine([]Opinion{op("a", Approve), op("b", Revise)})
	if r.Verdict != Revise {
		t.Fatalf("a 1-1 approve/revise tie resolved to %q, want revise", r.Verdict)
	}
}

// A reviewer that names a BLOCKER and then approves has contradicted itself. Take the
// finding, not the label — the finding is the part with evidence attached.
func TestABlockerCannotCoexistWithApprove(t *testing.T) {
	r := Combine([]Opinion{
		op("a", Approve, Finding{Severity: SevBlocker, File: "x.go", Summary: "drops the error"}),
	})
	if r.Verdict == Approve {
		t.Fatal("a review naming a BLOCKER was recorded as approved — the label beat the evidence")
	}
	if r.Verdict != Revise {
		t.Fatalf("verdict = %q, want revise", r.Verdict)
	}
}

// Findings surface worst-first: a reader who stops after one line must see the blocker.
func TestFindingsAreRankedWorstFirst(t *testing.T) {
	r := Combine([]Opinion{op("a", Revise,
		Finding{Severity: SevNit, Summary: "naming"},
		Finding{Severity: SevBlocker, Summary: "data loss"},
		Finding{Severity: SevMinor, Summary: "log noise"},
	)})
	if r.Findings[0].Severity != SevBlocker {
		t.Fatalf("first finding is %q, want the blocker — the reader who reads one line must see the worst",
			r.Findings[0].Severity)
	}
}

// ---------------------------------------------------------------------------
// Parsing what an agent CLI actually returns
// ---------------------------------------------------------------------------

// Agent CLIs wrap JSON in prose and fences. The parser must find the verdict anyway.
func TestParseToleratesRealAgentOutput(t *testing.T) {
	cases := map[string]string{
		"bare":     `{"verdict":"approve","notes":"fine"}`,
		"fenced":   "Here is my review:\n```json\n{\"verdict\":\"approve\",\"notes\":\"fine\"}\n```\n",
		"preamble": "I reviewed the diff.\n\n{\"verdict\":\"approve\"}\n\nHope that helps!",
		"nested":   `{"verdict":"revise","findings":[{"severity":"major","file":"a.go","line":3,"summary":"x"}]}`,
	}
	for name, raw := range cases {
		got := ParseOpinion("codex", raw)
		if got.Verdict == Errored {
			t.Errorf("%s: could not parse a real-shaped answer: %s\n%s", name, got.Error, raw)
		}
	}
}

// A brace inside a STRING must not end the object. `"summary": "use } carefully"` is
// exactly the kind of content a code review contains.
func TestParseHandlesBracesInsideStrings(t *testing.T) {
	raw := `{"verdict":"revise","findings":[{"severity":"major","summary":"the closing } is misplaced"}],"notes":"ok"}`
	got := ParseOpinion("codex", raw)
	if got.Verdict != Revise {
		t.Fatalf("verdict = %q (%s) — a brace inside a string truncated the object", got.Verdict, got.Error)
	}
	if len(got.Findings) != 1 || !strings.Contains(got.Findings[0].Summary, "misplaced") {
		t.Fatalf("findings = %+v", got.Findings)
	}
}

// THE OTHER SAFETY PROPERTY. Unparseable output is an ERROR, never a default.
//
// A parser that quietly defaulted to "approve" would turn every malformed answer — and
// every rate-limit page, and every "I'm sorry, I can't help with that" — into consent.
func TestUnreadableOutputIsAnErrorNotAnApproval(t *testing.T) {
	for _, raw := range []string{
		"",
		"I think it looks pretty good to me!",
		`{"verdict":"lgtm"}`,   // not in the closed vocabulary
		`{"verdict":"approve"`, // truncated
		"Rate limit exceeded. Please try again later.",
	} {
		got := ParseOpinion("codex", raw)
		if got.Verdict != Errored {
			t.Errorf("ParseOpinion(%q) = %q, want error — anything unreadable that becomes a verdict is a silent approval",
				raw, got.Verdict)
		}
		if got.Verdict == Approve {
			t.Errorf("ParseOpinion(%q) APPROVED", raw)
		}
	}
}

// A finding with no severity is a minor, not a blocker and not nothing.
func TestFindingWithoutSeverityDefaultsToMinor(t *testing.T) {
	got := ParseOpinion("a", `{"verdict":"revise","findings":[{"summary":"x"}]}`)
	if len(got.Findings) != 1 || got.Findings[0].Severity != SevMinor {
		t.Fatalf("findings = %+v, want one minor", got.Findings)
	}
}

// The rubric must change with the stage. One "review this" prompt does all three jobs
// badly: asked to review a design, a model comments on the code style of the examples.
func TestRubricIsStageSpecific(t *testing.T) {
	plan := Rubric("plan", "s", "c")
	code := Rubric("code", "s", "c")
	test := Rubric("test", "s", "c")
	if plan == code || code == test {
		t.Fatal("the rubric does not change with the stage — judging a plan and judging a diff are different jobs")
	}
	if !strings.Contains(plan, "riskiest assumption") {
		t.Error("the plan rubric does not ask about assumptions")
	}
	if !strings.Contains(test, "made to pass by weakening") {
		t.Error("the test rubric does not guard against a test weakened into passing")
	}
	// Every rubric must impose the same closed verdict contract.
	for _, r := range []string{plan, code, test, Rubric("deploy", "s", "c")} {
		if !strings.Contains(r, `"verdict": "approve" | "revise" | "reject"`) {
			t.Error("a rubric does not pin the closed verdict vocabulary")
		}
	}
}

// Blocking is the caller's contract, and it must be false only for approve.
func TestOnlyApproveIsNonBlocking(t *testing.T) {
	for v, want := range map[Verdict]bool{Approve: false, Revise: true, Reject: true, Errored: true} {
		if got := (Report{Verdict: v}).Blocking(); got != want {
			t.Errorf("Report{%q}.Blocking() = %v, want %v", v, got, want)
		}
	}
}
