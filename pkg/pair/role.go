// Package pair runs work through two agents: one proposes and one pairs with
// it in a declared role. An optional command gate can verify the outcome.
//
// It replaces `judge`, which could only ever TALK.
//
// # 1. The pair ACTS. It does not voice an opinion.
//
// This is the whole point, and it is what `judge` could not do. A judge reads your work and
// returns approve/revise/reject. A pair takes the keyboard.
//
//	judge:  "this breaks on empty input"          <- a CLAIM. Someone must now adjudicate it.
//	pair:   a test that FAILS on empty input       <- a PROOF. The gate reads it. Nobody adjudicates.
//
// That difference dissolves the hardest problem in every critic pattern: a critic's findings
// are unverifiable assertions, so you need a second critic to check the first, and a third to
// break the tie. Make the critic ACT and the regress collapses:
//
//	the test fails -> the bug is REAL. Proven, by execution, for free.
//	the test passes -> the pair was WRONG. Discarded. It cost tokens and nothing else.
//
// A finding that must be believed needs a judge. A finding that RUNS needs no one.
//
// So roles carry an Acts bit, and the evidence they produce is ranked accordingly — DIFF
// beats PROBE beats VERDICT, the same ladder the fleet-evidence-invariant already uses. An
// acting pair is not a fancier reviewer; it is a different and stronger instrument.
//
// # 2. The pair may never approve
//
// Google Cloud's review-and-critique pattern terminates "when the critic agent approves the
// content." That is the exact failure this codebase has spent its life fighting — a MODEL
// ASSERTING SUCCESS:
//
//	"all three harnesses exit 0 when they fail"   (docs/harness-ab-deepseek.md)
//	...and then ycode did it too
//	...and then codex, asked to build a feature, shipped a RED TEST, blamed the sandbox,
//	   and exited 0
//
// Agency and authority are ORTHOGONAL, and conflating them is how that keeps happening. A
// real pair programmer can grab the keyboard and rewrite your function — and still cannot
// declare the branch shippable. CI does that.
//
//	the pair may ACT freely, and REJECT or ADVISE.
//	the pair may never APPROVE. An ungated result is advisory; an opted-in gate is authoritative.
//
// # 3. Does the pair SEE the proposal?
//
//	refute / break / fix / validate  ->  YES. They are attacking, repairing, or checking it.
//	second-opinion                   ->  NO.  BLIND, and that is the feature.
//
// A "second opinion" that reads your answer first is not a second opinion. It is a review,
// anchored to a conclusion it did not reach. Measured: in a four-L4 design meeting the
// hypothesis was stated up front, three of the four AGREED, and all three were wrong. The
// one that disagreed was the only one worth paying for.
//
// A blind pair cannot agree with you. It can only CONVERGE with you — and convergence is
// evidence in a way agreement never is.
package pair

import (
	"fmt"
	"sort"
	"strings"
)

// Authority is what a pair may conclude. It is never "approve".
type Authority string

const (
	// AuthorityReject: the pair may block. It can never pass.
	AuthorityReject Authority = "reject"

	// AuthorityAdvise: the pair may only inform. It can neither block nor pass.
	AuthorityAdvise Authority = "advise"
)

// Evidence is what a role PRODUCES, ranked by how hard it is to fake.
//
// This is the fleet-evidence-invariant ladder. A role's evidence type is the single best
// predictor of whether it was worth running.
type Evidence string

const (
	// EvidenceDiff — code. A failing test, a fix, an independent implementation. It can be
	// RUN, so it verifies itself and needs no adjudication. Prefer this.
	EvidenceDiff Evidence = "diff"

	// EvidenceProbe — a command and its output. Weaker than a diff (nothing durable is
	// left behind) but still a fact, not an assertion.
	EvidenceProbe Evidence = "probe"

	// EvidenceVerdict — prose. An opinion. The weakest thing a pair can produce, and all
	// `judge` could ever produce. Someone must now decide whether to believe it.
	EvidenceVerdict Evidence = "verdict"
)

// Rank orders evidence, strongest first. Used to warn when a cheap role was chosen for
// expensive work.
func (e Evidence) Rank() int {
	switch e {
	case EvidenceDiff:
		return 3
	case EvidenceProbe:
		return 2
	case EvidenceVerdict:
		return 1
	}
	return 0
}

// Role is what the second agent is ASKED TO DO. Everything here varies freely — this is the
// generic axis: declare a role, get a pair that plays it.
//
// What does NOT vary is enforced by Validate.
type Role struct {
	Name string `yaml:"name" json:"name"`

	// Summary is one line, shown by `bashy pair roles`.
	Summary string `yaml:"summary" json:"summary"`

	// Acts decides whether the pair gets the keyboard: a workspace, write access, and the
	// tools to run what it wrote.
	//
	// FALSE makes it a commentator — cheap, fast, and its output is an unverified claim.
	// TRUE makes it a collaborator, and its output is something the gate can execute.
	//
	// This is the axis `judge` never had, and it is the reason a pair can beat two agents
	// working separately.
	Acts bool `yaml:"acts" json:"acts"`

	// SeesProposal decides whether the pair is shown the proposer's work. FALSE is not a
	// weaker setting — it is the entire content of a second opinion. See the package doc.
	SeesProposal bool `yaml:"sees_proposal" json:"sees_proposal"`

	// Authority: reject or advise. NEVER approve. Validate enforces it.
	Authority Authority `yaml:"authority" json:"authority"`

	// Evidence is what this role is expected to produce.
	Evidence Evidence `yaml:"evidence" json:"evidence"`

	// Brief is the instruction. The VERB in it matters more than anything else in this
	// struct: "review this" gets you approval, because a reviewer is looking for reasons to
	// agree. "REFUTE this" killed one of four planned deletions and found two holes that
	// would have shipped.
	Brief string `yaml:"brief" json:"brief"`

	// Capability routes the pair through the capability matrix. Empty = the proposer's.
	Capability string `yaml:"capability,omitempty" json:"capability,omitempty"`
}

// Validate enforces the contract. A role that breaks it is not a weaker role — it is a
// different and unsafe pattern wearing this one's name.
func (r Role) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("pair role: name is required")
	}
	if strings.TrimSpace(r.Brief) == "" {
		return fmt.Errorf("pair role %q: brief is required — a pair given no instruction "+
			"defaults to agreeing with you", r.Name)
	}

	switch r.Authority {
	case AuthorityReject, AuthorityAdvise:
	case "approve", "":
		return fmt.Errorf("pair role %q: authority %q is not permitted.\n"+
			"A PAIR MAY NEVER APPROVE. It may act freely — edit, test, fix — but only a GATE may "+
			"say the work is done, and a gate is a command, not a model. Agency and authority are "+
			"different axes; a model asserting success is the failure this package exists to prevent",
			r.Name, r.Authority)
	default:
		return fmt.Errorf("pair role %q: unknown authority %q (want reject or advise)", r.Name, r.Authority)
	}

	switch r.Evidence {
	case EvidenceDiff, EvidenceProbe, EvidenceVerdict:
	case "":
		return fmt.Errorf("pair role %q: evidence is required (diff, probe, or verdict)", r.Name)
	default:
		return fmt.Errorf("pair role %q: unknown evidence %q", r.Name, r.Evidence)
	}

	// A role that cannot act cannot produce a diff. It can only describe one.
	if r.Evidence == EvidenceDiff && !r.Acts {
		return fmt.Errorf("pair role %q: evidence is `diff` but acts is false — a pair with no "+
			"keyboard cannot produce code, only a description of code. Set acts: true, or lower "+
			"the evidence to `verdict` and be honest that the output is an opinion", r.Name)
	}

	// A "second opinion" that has read your answer is a review. The name would be a lie.
	if r.Name == "second-opinion" && r.SeesProposal {
		return fmt.Errorf("pair role %q: sees_proposal must be false — an answer that has READ the "+
			"first answer is anchored to it, and anchored agreement is not evidence", r.Name)
	}
	return nil
}

// BuiltinRoles ship with bashy. Note that only ONE of them is a pure commentator, and it is
// the one to reach for last.
var BuiltinRoles = map[string]Role{
	// THE FLAGSHIP. The pair writes the test that proves the bug.
	"break": {
		Name:         "break",
		Summary:      "WRITE the failing test that proves the bug (the finding verifies itself)",
		Acts:         true,
		SeesProposal: true,
		Authority:    AuthorityReject,
		Evidence:     EvidenceDiff,
		Capability:   "coding",
		Brief: `Break this code. Not by describing how — by WRITING THE TEST THAT FAILS.

You have the keyboard. A paragraph explaining that something breaks is a claim someone else
must now adjudicate. A test that breaks is a fact, and the gate reads it without asking
anyone's opinion. Produce the fact.

For each defect you believe is real:
  1. Write a test that FAILS on the current code.
  2. Run it. Watch it fail. If it passes, YOU WERE WRONG — delete it and move on. That is a
     success, not an embarrassment; you just cost the operator a few tokens instead of a
     production incident.
  3. Leave it failing. Do NOT fix the code — that is the proposer's job, and a fix authored
     by the same agent that found the bug is a fix nobody checked.

Aim where tests do not usually go:
  - the boundary, and one past it
  - the empty case, the single case, the enormous case
  - the path where a value is ABSENT — and what the code concludes from its absence
  - any existing test that asserts something about its FIXTURE rather than about production

You may not approve. If you attacked it honestly and it held, say so plainly and write no
test. An invented test is worse than no test: it will be maintained forever.`,
	},

	"fix": {
		Name:         "fix",
		Summary:      "repair what you find, and prove the repair with a test",
		Acts:         true,
		SeesProposal: true,
		Authority:    AuthorityReject,
		Evidence:     EvidenceDiff,
		Capability:   "coding",
		Brief: `Find what is wrong with this work and FIX it. You have the keyboard.

Do not write a review. Write the patch.

For each fix:
  - First write a test that fails on the CURRENT code. If you cannot make one fail, you have
    not found a bug — you have found a preference. Leave it alone.
  - Then fix the code until that test passes.
  - Leave both. The test is the evidence that the fix was necessary; without it your patch is
    indistinguishable from a rewrite you happened to prefer.

You may not approve your own work or declare the whole thing done. If the caller supplied a
gate, that command determines the verified outcome; otherwise your result is advisory.`,
	},

	"second-opinion": {
		Name:         "second-opinion",
		Summary:      "solve it independently and BLIND, so convergence means something",
		Acts:         true,
		SeesProposal: false, // load-bearing. See the package doc.
		Authority:    AuthorityAdvise,
		Evidence:     EvidenceDiff,
		Capability:   "coding",
		Brief: `Solve this problem yourself, from scratch.

You have NOT been shown any existing solution, and that is deliberate. A second opinion that
has read the first opinion is not a second opinion — it is a review, anchored to a conclusion
it did not reach on its own.

Write real code, not a sketch. Two implementations can be DIFFED; two opinions can only be
argued about.

Where you are uncertain, say so and say why. Do not hedge toward what you imagine someone
else concluded — you do not know what they concluded, and guessing throws away the only thing
that makes your answer worth having.`,
	},

	"validate": {
		Name:         "validate",
		Summary:      "check each claim by RUNNING something, not by reading",
		Acts:         true,
		SeesProposal: true,
		Authority:    AuthorityReject,
		Evidence:     EvidenceProbe,
		Capability:   "code-review",
		Brief: `Verify or REFUTE each claim below. Check them by RUNNING something.

You have a shell. Use it. "It looks correct" is not a verification — it is a refusal to
verify, dressed up as one.

For each claim: CONFIRMED or REFUTED, and the command you ran plus its output. Where no
command can settle it, say that explicitly rather than substituting your impression.

Default to REFUTED when you could not establish it. A claim you were unable to check is not a
claim that held — and a success state reached by the ABSENCE of evidence is the specific bug
class this whole codebase exists to stamp out.`,
	},

	// The commentator. Cheapest, weakest, and the only thing `judge` could ever do.
	"refute": {
		Name:         "refute",
		Summary:      "attack it in prose — cheap, fast, and produces an unverified claim",
		Acts:         false,
		SeesProposal: true,
		Authority:    AuthorityReject,
		Evidence:     EvidenceVerdict,
		Capability:   "code-review",
		Brief: `Your job is to REFUTE this work. Find what BREAKS.

Do not review it. A reviewer looks for reasons to approve; you are looking for the defect that
ships.

Rules:
  - file:line, or it did not happen.
  - Every finding needs a FAILURE SCENARIO: concrete inputs, and the wrong output they
    produce. "This could be clearer" is not a finding.
  - You may not approve. If the caller supplied a gate, it decides the verified outcome.
    Otherwise your result is advisory.
  - If you attacked it and it held, SAY SO. Inventing a finding to fill the slot is worse than
    an empty slot.

Look hardest for a SUCCESS STATE REACHED BY THE ABSENCE OF EVIDENCE: a field declared and
never written; a limit that binds and exits 0; a number used in a decision whose source nobody
records; a test that passes because its fixture does something production cannot. A test suite
cannot catch those — the tests pass. That is what you are for.`,
	},
}

// ResolveRole finds a role by name and validates it.
func ResolveRole(name string) (Role, error) {
	r, ok := BuiltinRoles[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Role{}, fmt.Errorf("unknown pair role %q (have: %s)", name, strings.Join(RoleNames(), ", "))
	}
	if err := r.Validate(); err != nil {
		return Role{}, err
	}
	return r, nil
}

// RoleNames lists the built-in roles, sorted.
func RoleNames() []string {
	out := make([]string, 0, len(BuiltinRoles))
	for n := range BuiltinRoles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
