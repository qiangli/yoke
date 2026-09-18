package pair

import (
	"context"
	"strings"
	"testing"
)

// --- the contract ------------------------------------------------------------------------

// A pair may act freely. It may never approve. These are different axes, and conflating them
// is how "the critic said it was fine" becomes a release.
func TestNoRoleMayApprove(t *testing.T) {
	for name, r := range BuiltinRoles {
		if r.Authority != AuthorityReject && r.Authority != AuthorityAdvise {
			t.Errorf("role %q has authority %q — a pair may never approve", name, r.Authority)
		}
	}

	bad := Role{Name: "rubber-stamp", Brief: "b", Authority: "approve", Evidence: EvidenceVerdict}
	err := bad.Validate()
	if err == nil {
		t.Fatal("a role with authority=approve was accepted; the one rule that must never bend, bent")
	}
	if !strings.Contains(err.Error(), "MAY NEVER APPROVE") {
		t.Errorf("the error must say why, loudly. got: %v", err)
	}
}

// A role that cannot act cannot produce a diff — only a description of one.
func TestDiffEvidenceRequiresAgency(t *testing.T) {
	bad := Role{Name: "armchair", Brief: "b", Authority: AuthorityReject, Evidence: EvidenceDiff, Acts: false}
	if err := bad.Validate(); err == nil {
		t.Fatal("a non-acting role claimed diff evidence: a pair with no keyboard cannot write code")
	}
}

// A second opinion that has read the first opinion is a review.
func TestSecondOpinionIsBlind(t *testing.T) {
	if BuiltinRoles["second-opinion"].SeesProposal {
		t.Fatal("second-opinion sees the proposal — it is anchored, and anchored agreement is not evidence")
	}
	bad := BuiltinRoles["second-opinion"]
	bad.SeesProposal = true
	if err := bad.Validate(); err == nil {
		t.Fatal("a sighted second-opinion validated; the name would be a lie")
	}
}

func TestBlindRoleNeverSeesTheProposal(t *testing.T) {
	p := &Plan{Role: BuiltinRoles["second-opinion"], Task: "sort a list"}
	got := p.PairPrompt("PROPOSER_SECRET_ANSWER")
	if strings.Contains(got, "PROPOSER_SECRET_ANSWER") {
		t.Fatal("the blind pair was shown the proposal — the entire value of a second opinion is gone")
	}

	sighted := &Plan{Role: BuiltinRoles["break"], Task: "sort a list"}
	if !strings.Contains(sighted.PairPrompt("PROPOSER_SECRET_ANSWER"), "PROPOSER_SECRET_ANSWER") {
		t.Fatal("the sighted pair was NOT shown the work it is meant to attack")
	}
}

func TestRolesMayRunWithoutAGate(t *testing.T) {
	for _, name := range []string{"break", "second-opinion"} {
		if _, err := NewPlan(nil, BuiltinRoles[name], Agents{Proposer: "a:x", Pair: "b:y"}, "task", ""); err != nil {
			t.Fatalf("role %q was refused a gateless advisory run: %v", name, err)
		}
	}
}

func TestRejectAuthorityWithoutGateIsReportedUngated(t *testing.T) {
	plan, err := NewPlan(nil, BuiltinRoles["break"], Agents{Proposer: "a:x", Pair: "b:y"}, "task", "")
	if err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, _, _ string, _ bool) (string, error) { return "advisory contribution", nil }
	res, err := plan.Run(context.Background(), run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeUngated || !strings.Contains(res.Headline(), "UNGATED") {
		t.Fatalf("gateless reject role was not clearly advisory: %+v", res)
	}
	if prompt := plan.PairPrompt("proposal"); !strings.Contains(prompt, "advisory and unverified") {
		t.Fatalf("gateless reject prompt did not explain its authority:\n%s", prompt)
	}
}

func TestAnAgentCannotPairWithItself(t *testing.T) {
	_, err := NewPlan(nil, BuiltinRoles["break"], Agents{Proposer: "a:x", Pair: "a:x"}, "task", "make test")
	if err == nil || !strings.Contains(err.Error(), "SAME agent") {
		t.Fatalf("an agent was allowed to refute itself; it will agree with the reasoning it just produced. got: %v", err)
	}
}

func TestAgentFlagAliasesPair(t *testing.T) {
	cmd := NewPairCmd()
	if cmd.Flags().Lookup("agent") == nil || cmd.Flags().Lookup("pair") == nil || cmd.Flags().Lookup("yolo") == nil {
		t.Fatal("pair must accept --agent/--pair and an explicit --yolo permission opt-in")
	}
	if got := cmd.Flags().Lookup("verify").Usage; strings.Contains(got, "default") {
		t.Fatalf("--verify help implies an implicit gate: %q", got)
	}
}

// --- the payoff: an acting pair adjudicates itself ---------------------------------------

// The three-way that the baseline gate buys, and the reason it is not ceremony.
func TestClassify(t *testing.T) {
	pass := &GateRun{Passed: true}
	fail := &GateRun{Passed: false}
	abstain := &GateRun{Abstained: true}

	for _, tc := range []struct {
		name          string
		before, after *GateRun
		acts          bool
		want          Outcome
		why           string
	}{
		{
			name: "the money case", before: pass, after: fail, acts: true, want: OutcomeProved,
			why: "green, the pair wrote a failing test, red. The gate proved the bug — no judge needed.",
		},
		{
			name: "repaired", before: fail, after: pass, acts: true, want: OutcomeRepaired,
			why: "red, the pair fixed it, green.",
		},
		{
			name: "attacked and held", before: pass, after: pass, acts: true, want: OutcomeHeld,
			why: "the pair found nothing the gate can see. A real, cheap result.",
		},
		{
			name: "red before, red after", before: fail, after: fail, acts: true, want: OutcomeBrokenBefore,
			why: "a second failure on top of a failure is NOT a signal. The pair cannot be credited.",
		},
		{
			name: "no baseline at all", before: nil, after: fail, acts: true, want: OutcomeUnmeasured,
			why: "without a measured baseline, the after gate proves the work is red but cannot " +
				"prove that the baseline was red.",
		},
		{
			name: "baseline abstained", before: abstain, after: pass, acts: true, want: OutcomeUnmeasured,
			why: "a silent zero-exit baseline is not evidence of red or green.",
		},
		{
			name: "after abstained", before: pass, after: abstain, acts: true, want: OutcomeUnmeasured,
			why: "a gate that cannot demonstrate a verdict cannot prove the pair held or broke it.",
		},
		{
			name: "commentator, gate green", before: nil, after: pass, acts: false, want: OutcomeHeld,
			why: "a commentator changed nothing, so the gate speaks about the proposer only.",
		},
		{
			name: "commentator, gate red", before: nil, after: fail, acts: false, want: OutcomeUnmeasured,
			why: "a commentator changed nothing, so the after gate proves the proposer's work is red; " +
				"it does not prove an unmeasured baseline was red.",
		},
		{
			name: "no gate", before: nil, after: nil, acts: true, want: OutcomeUngated,
			why: "nothing checked the claim.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.before, tc.after, tc.acts); got != tc.want {
				t.Errorf("classify = %q, want %q\n  %s", got, tc.want, tc.why)
			}
		})
	}
}

// The gate must run even when the pair is happy. A clean critique is not evidence the code
// works; that is the substitution this package exists to refuse.
func TestGateRunsEvenWhenThePairFindsNothing(t *testing.T) {
	plan, err := NewPlan(nil, BuiltinRoles["break"], Agents{Proposer: "a:x", Pair: "b:y"}, "task", "make test")
	if err != nil {
		t.Fatal(err)
	}

	gateRuns := 0
	run := func(_ context.Context, _, _ string, _ bool) (string, error) {
		return "I attacked it and it held. No test written.", nil
	}
	gate := func(_ context.Context, _ string) (*GateRun, error) {
		gateRuns++
		return &GateRun{Passed: true, Command: "make test"}, nil
	}

	res, err := plan.Run(context.Background(), run, gate)
	if err != nil {
		t.Fatal(err)
	}
	if gateRuns != 2 {
		t.Fatalf("gate ran %d times, want 2 (baseline + after) — an acting pair needs both, or its "+
			"contribution cannot be attributed", gateRuns)
	}
	if res.Outcome != OutcomeHeld {
		t.Errorf("outcome = %q, want %q", res.Outcome, OutcomeHeld)
	}
}

// The end-to-end money case, as a conductor would see it.
func TestActingPairProvesTheBugWithoutAJudge(t *testing.T) {
	plan, err := NewPlan(nil, BuiltinRoles["break"], Agents{Proposer: "claude:opus4.8", Pair: "codex:gpt-5.5"}, "add a parser", "make test")
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	run := func(_ context.Context, _, _ string, acts bool) (string, error) {
		calls++
		if !acts {
			t.Error("the `break` role must have the keyboard — it writes a test, it does not describe one")
		}
		return "wrote TestEmptyInput; it fails", nil
	}
	// Green, then the pair's new test turns it red.
	gate := func(_ context.Context, _ string) (*GateRun, error) {
		if calls == 0 {
			return &GateRun{Passed: true, Command: "make test"}, nil // baseline
		}
		return &GateRun{Passed: false, ExitCode: 1, Command: "make test"}, nil
	}

	res, err := plan.Run(context.Background(), run, gate)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeProved {
		t.Fatalf("outcome = %q, want %q — this is the whole feature", res.Outcome, OutcomeProved)
	}
	if !strings.Contains(res.Headline(), "PROVED") {
		t.Errorf("headline should lead with the proof: %q", res.Headline())
	}
	// And nothing anywhere said "approved".
	if strings.Contains(strings.ToLower(res.Headline()), "approv") {
		t.Error("a pair result used the word 'approve'; only a gate may")
	}
}

func TestEveryBuiltinRoleIsValid(t *testing.T) {
	for name, r := range BuiltinRoles {
		if err := r.Validate(); err != nil {
			t.Errorf("builtin role %q does not satisfy its own contract: %v", name, err)
		}
		if r.Name != name {
			t.Errorf("role keyed %q but named %q", name, r.Name)
		}
	}
}

// Only ONE builtin is a pure commentator, and it should be the one you reach for last.
func TestMostRolesAct(t *testing.T) {
	acting := 0
	for _, r := range BuiltinRoles {
		if r.Acts {
			acting++
		}
	}
	if acting < len(BuiltinRoles)-1 {
		t.Errorf("%d of %d roles act — a pair that only talks is a judge, and judge is what this "+
			"replaces", acting, len(BuiltinRoles))
	}
}
