// weave_capability.go — folds a finalized run's gate evidence into the
// capability matrix (pkg/capability): the leaderboard's self-updating half
// (dhnt/docs/agent-leaderboard-and-role-requirements-plan.md Phase 2 item 2).
//
// The matrix already ranks agents from research priors; this is the writer
// that turns real weave runs into host evidence, the same way meet's
// RecordOperability turns meeting turns into operability evidence. It is
// called from the finalize paths where VerifyExit / SuiteGateExit land: the
// wrapper's terminal finalization, `weave finalize`, and `weave pull`'s
// pair/suite-gate outcomes. (A kill is not a finalize — a killed run is
// resumable and its evidence re-lands when it next finalizes or merges.)
package weave

import (
	"os"
	"path/filepath"

	"github.com/qiangli/yoke/pkg/capability"
)

// weaveRecordCapability folds one finalized run's gate evidence into the
// capability matrix. Best-effort, mirroring meet: every record error is
// discarded, and BASHY_NO_CAPABILITY_RECORD opts the host out entirely.
//
// FLEET-EVIDENCE-INVARIANT: a nil VerifyExit is the ABSENCE of evidence, so
// it records NOTHING — not a pass, and not even an operability sample. A run
// whose verify never ran says nothing about the agent that ran it, and an
// absent signal must never move a cell.
func weaveRecordCapability(it *weaveItem) {
	if it == nil || os.Getenv("BASHY_NO_CAPABILITY_RECORD") != "" {
		return
	}
	if it.VerifyExit == nil {
		return
	}
	// The gate outcome is the leaderboard's coding sample: the substrate's
	// own verify command passed, the suite gate passed (or never ran — a
	// gate that did not run cannot fail), the run left commits ahead of
	// base, and it stayed inside its workspace.
	gatePass := *it.VerifyExit == 0 &&
		(it.SuiteGateExit == nil || *it.SuiteGateExit == 0) &&
		it.CommitsAhead > 0 &&
		!it.IsolationViolated

	agent, ok := weaveCapabilityAgent(it)
	if !ok {
		// No canonical identity: record operability only. Quality columns
		// are model-governed, and writing them against a guessed identity
		// is how a matrix comes to trust agents that never ran. No
		// attribution, no quality row.
		if tool := weaveCapabilityTool(it); tool != "" {
			_ = capability.RecordOperability(tool, gatePass)
		}
		return
	}
	_ = capability.Record(agent, capability.CapCoding, gatePass, 0, 0, capability.NowRFC())
	if it.IsolationViolated {
		_ = capability.Record(agent, capability.CapIsolation, false, 0, 0, capability.NowRFC())
	}
	// A review verdict accrues to the REVIEWER's code-review row, never the
	// coder's. Only a named terminal verdict is evidence: a harness error —
	// like a nil VerifyExit — records nothing.
	if pass, ok := weaveReviewOutcome(it); ok {
		if key, found := weaveCapabilityReviewAgent(it.ReviewAgent); found {
			_ = capability.Record(key, capability.CapCodeReview, pass, 0, 0, capability.NowRFC())
		}
	}

	// And the RAW outcome, for the leaderboard.
	//
	// The matrix folds this same run into an exponential moving average, which
	// is the right shape for routing and the wrong one for ranking: an EMA
	// cannot be un-averaged, so a quality of 0.72 over 13 samples yields no
	// pass count and therefore no confidence interval. The ledger keeps the
	// outcome itself so `bashy leaderboard` can rank on a Wilson bound instead
	// of a point estimate. Best-effort, like everything else here — evidence is
	// a by-product of the work, and it must never fail the work.
	_ = capability.Append(weaveLedgerRecord(it, agent, gatePass))
}

// weaveLedgerRecord projects a finalized run onto the ledger's record.
//
// It carries NO identity by construction: no branch, no issue title, no path,
// no host. The published leaderboard is regenerated from these records into a
// doc that ships in an OSS repo, and the guarantee that holds there is
// structural — the identity-bearing fields do not exist — rather than a
// scrubber someone has to remember to run.
func weaveLedgerRecord(it *weaveItem, agent string, gatePass bool) capability.RunRecord {
	r := capability.RunRecord{
		At:     capability.NowRFC(),
		Agent:  agent,
		Role:   weaveLedgerRole(it),
		Source: capability.SourceWeave,
		// Repo is deliberately LEFT EMPTY. The finalize path is handed an item,
		// not a workspace, so the only repo name reachable from here would come
		// from a path — and a path names a machine and a user. An absent
		// basename costs one grouping dimension; a leaked path costs the OSS
		// hygiene rule. The field stays in the schema for a caller that has a
		// basename honestly to hand.
		GatePass: &gatePass,
		Review:   it.ReviewVerdict,
		// Coach evidence is only comparable within one mode: a pty scrape
		// counts novel output rather than tool calls, so the mode travels with
		// the ratio and the leaderboard pools events-mode records alone.
		CoachMode:   it.CoachMode,
		RepeatRatio: it.CoachRepeatRatio,
		Steers:      it.CoachSteers,
	}
	if it.VerifyExit != nil {
		v := *it.VerifyExit
		r.VerifyExit = &v
	}
	if it.CoachSteers > 0 {
		recovered := it.CoachRecovered
		r.Recovered = &recovered
	}
	if it.PairVerdict != "" {
		// The adversarial pair's verdict is the stronger signal and wins, the
		// same precedence weaveReviewOutcome applies.
		r.Review = it.PairVerdict
	}
	return r
}

// weaveCapabilityAgent resolves the run's canonical matrix identity
// (tool:model) down the attribution ladder:
//
//  1. LaunchSpec.Agent — the fleet nickname the run was launched under —
//     resolved through the catalog to its MatrixKey.
//  2. LaunchSpec.Model — the provider-side id actually selected — resolved
//     through ResolveLaunchModel to the canonical model, keyed by the run's
//     tool. A model the catalog does not know is never guessed.
//
// Anything else has no attribution.
func weaveCapabilityAgent(it *weaveItem) (string, bool) {
	tool, nick, model := "", "", ""
	if it != nil {
		tool = it.Tool
		if it.LaunchSpec != nil {
			nick = it.LaunchSpec.Agent
			model = it.LaunchSpec.Model
			if tool == "" {
				tool = it.LaunchSpec.Tool
			}
		}
	}
	cat := fleetCatalog()
	if nick != "" {
		if a, ok := cat.Agent(nick); ok {
			return a.MatrixKey(), true
		}
	}
	if tool != "" {
		if _, canonical := cat.ResolveLaunchModel(tool, model); canonical != "" {
			if _, ok := cat.Model(canonical); ok {
				return tool + ":" + canonical, true
			}
		}
	}
	return "", false
}

// weaveCapabilityTool is the run's harness, for the unattributed
// operability-only record.
func weaveCapabilityTool(it *weaveItem) string {
	tool := it.Tool
	if tool == "" && it.LaunchSpec != nil {
		tool = it.LaunchSpec.Tool
	}
	return tool
}

// weaveReviewOutcome maps the run's recorded review verdicts to one
// code-review sample for the reviewer. PairVerdict — the adversarial pair's —
// is the ReviewAgent's own act and wins when both are set; ReviewVerdict (the
// conductor's clean-room review) is attributable only because a ReviewAgent
// is recorded on the run. A named terminal verdict is evidence, pass or
// fail; "", harness-error, and broken-before are the absence of attributable
// evidence and record nothing, mirroring the nil-VerifyExit invariant.
func weaveReviewOutcome(it *weaveItem) (pass, ok bool) {
	if it == nil || it.ReviewAgent == "" {
		return false, false
	}
	verdict := it.PairVerdict
	if verdict == "" {
		verdict = it.ReviewVerdict
	}
	switch verdict {
	case string(weavePairPass): // "pass" — also ReviewVerdict's pass spelling
		return true, true
	case string(weavePairRefuted), "blocked":
		return false, true
	}
	return false, false
}

// weaveCapabilityReviewAgent resolves the recorded ReviewAgent (a registry
// binding, occasionally a nickname) to its canonical matrix key. An
// unresolvable reviewer has no attribution — no row.
func weaveCapabilityReviewAgent(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if a, ok := fleetCatalog().Agent(name); ok {
		return a.MatrixKey(), true
	}
	return "", false
}

// weaveLedgerRole records WHICH SEAT this run held, which is the field
// docs/role-capability-evidence-matrix.md §2 names as the one thing missing
// before a conductor claim can be evidence rather than an assumption.
//
// The rule that doc settles on: ATTRIBUTE BY WHAT THE RUN DID, NOT BY ASSUMING
// WHAT IT WAS. A plain worker run's repeat ratio filed under orchestration is
// the misattribution that made gemini3.1's demotion `operator` rather than
// `measured` — so a band-3 claim needs to know the run conducted, and a band-2
// one needs to know it did not.
//
// It uses the doc's third option — "an item whose children were created during
// its own run conducted" — and gets the timing for free rather than by
// timestamping. An item with children is normally a CONTAINER, never claimed by
// an agent and therefore never reaching this function at all (no VerifyExit, no
// record). And `weave split` refuses a parent that is not `todo`. So the only
// way an item that RAN can hold children is `weave add` from inside the run:
// having children here already means it created them while working.
//
// The queue is found from the workspace path rather than threaded through three
// call sites: a workspace is always `<queueDir>/workspaces/issue-N`. A run with
// no workspace, or a queue that will not load, yields NO role — an unknown seat
// must stay unknown, because a wrong one is exactly the misattribution above.
func weaveLedgerRole(it *weaveItem) string {
	if it == nil || it.Workspace == "" {
		return ""
	}
	q, err := loadWeaveQueue(filepath.Dir(filepath.Dir(it.Workspace)))
	if err != nil || q == nil {
		return ""
	}
	for _, other := range q.Items {
		if other != nil && other.Parent == it.ID {
			return roleConductor
		}
	}
	if it.Parent != 0 {
		return roleWorker
	}
	// A solo run — no children, no parent. It did not conduct and it was not
	// dispatched, so neither seat describes it and neither is claimed.
	return ""
}

// The seats a weave run can be observed holding. Deliberately only two: these
// are what the queue can WITNESS, not the full role taxonomy.
const (
	roleConductor = "conductor"
	roleWorker    = "worker"
)
