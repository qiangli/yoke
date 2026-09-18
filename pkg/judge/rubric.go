// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package judge

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/atlas"
)

// The rubric is what makes this a REVIEW and not a vibe.
//
// Judging a plan, a diff and a failing test are three different jobs, and a single
// "review this" prompt does all three badly: asked to review a design, a model will
// comment on the code style of the examples; asked to review a diff, it will restate
// what the diff does. So the stage selects the questions — the same closed atlas
// vocabulary the verbs and the runs already declare.
//
// Every rubric ends with the same contract: return ONE json object, with a verdict from
// a closed set, and findings that name a place. "Looks good to me" is not a review.

const verdictContract = `
Return ONE JSON object and nothing else. No prose before or after, no markdown fence.

{
  "verdict": "approve" | "revise" | "reject",
  "findings": [
    {"severity": "blocker"|"major"|"minor"|"nit", "file": "path", "line": 0, "summary": "one sentence: what is wrong"}
  ],
  "notes": "one short paragraph: the single most important thing to know"
}

verdict:
  approve  it is correct and can ship as it is
  revise   it has fixable problems -- every one of them must appear in findings
  reject   the approach is wrong; revising will not save it

Rules:
  - A "blocker" finding means it MUST NOT ship. Never pair a blocker with "approve".
  - A finding must name a place and a defect. "Could be cleaner" is not a finding.
  - Do not invent problems to seem useful. An empty findings list with "approve" is a
    perfectly good review, and is better than a manufactured nit.
  - Judge what is here, not what you would have written.`

// Rubric returns the reviewer's instruction for a stage.
func Rubric(stage, subject, content string) string {
	var head string
	switch stage {
	case atlas.StagePlan:
		head = `You are reviewing a PLAN or DESIGN, before anyone builds it.

Ask, in this order:
  1. Does it solve the problem that was actually stated? (Or a neighbouring, easier one?)
  2. What does it NOT handle that it will meet in practice? Name the case.
  3. Is there a materially simpler design that gets most of the value?
  4. What is the riskiest assumption, and what happens if it is wrong?
  5. Is it testable -- could you tell, afterwards, whether it worked?

Do NOT review the prose, the formatting, or the code style of any examples.`

	case atlas.StageTest:
		head = `You are triaging a TEST FAILURE or a test suite.

Ask, in this order:
  1. What EXACTLY failed -- the assertion, the input, the actual vs expected value?
  2. Is the CODE wrong, or is the TEST wrong? Say which, and say why.
  3. Is this a real defect or an environment/flake artifact (timing, ordering, a shared
     temp dir, a missing fixture)? Evidence, not a guess.
  4. What is the smallest change that would make this pass FOR THE RIGHT REASON?
     Deleting the assertion is not that change.

A test that was made to pass by weakening it is a "blocker" finding, not an approval.`

	case atlas.StageDeploy:
		head = `You are reviewing a DEPLOYMENT change.

Ask, in this order:
  1. What breaks if this is applied to a live system RIGHT NOW?
  2. Is it reversible? What is the rollback, and has it been shown to work?
  3. What does it assume about the target that may not hold (state, version, secrets,
     capacity)?
  4. Is anything irreversible or destructive (data migration, deletion) -- and is it
     guarded?

Irreversible + unguarded is always a "blocker".`

	default: // code
		head = `You are reviewing a CODE CHANGE.

Ask, in this order:
  1. Is it CORRECT? Find a concrete input or state where it does the wrong thing.
  2. Does it do what the task asked -- all of it, and nothing extra?
  3. What breaks that used to work? Callers, tests, the on-disk format, the wire format.
  4. Are the edge cases handled: empty, nil, concurrent, error paths, partial failure?
  5. Is it tested where it matters -- and would the test FAIL if the code were wrong?

Style and naming are "nit" at most. Do not spend the review on them.`
	}

	return fmt.Sprintf("%s\n\n--- SUBJECT: %s ---\n\n%s\n\n--- END SUBJECT ---\n%s",
		head, subject, content, verdictContract)
}

// NormalizeStage maps a stage to one this package has a rubric for, defaulting to code.
func NormalizeStage(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case atlas.StagePlan, atlas.StageCode, atlas.StageTest, atlas.StageDeploy:
		return s
	}
	return atlas.StageCode
}
