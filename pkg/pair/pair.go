package pair

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
)

// SchemaVersion for `bashy pair --json`.
const SchemaVersion = "bashy-pair-v2"

// Outcome is what the run PROVED. Note that none of these is "approved" — that is not a
// thing a pair run can conclude, because a pair cannot approve and a gate does not know
// what the pair was trying to do.
type Outcome string

const (
	// OutcomeBrokenBefore: the gate was already red before the pair touched anything.
	// The pair's contribution CANNOT be attributed — and pretending otherwise is exactly
	// the absence-of-evidence bug. Fix the baseline first.
	OutcomeBrokenBefore Outcome = "broken-before"

	// OutcomeUnmeasured: a required gate state was absent or abstained.
	// There is not enough evidence to call the work green or red.
	OutcomeUnmeasured Outcome = "unmeasured"

	// OutcomeProved: the gate was GREEN, the pair acted, and now it is RED.
	//
	// This is the money case, and it is a PROOF, not an opinion: the pair wrote something
	// executable, and executing it exposed a defect that the proposer's own test suite
	// could not see. No adjudication. No second critic. The gate said so.
	OutcomeProved Outcome = "proved"

	// OutcomeRepaired: the gate was RED, the pair acted, and now it is GREEN.
	OutcomeRepaired Outcome = "repaired"

	// OutcomeHeld: green before, green after. The pair attacked and found nothing the gate
	// can see. A real result — and a cheap one.
	OutcomeHeld Outcome = "held"

	// OutcomeUngated: no gate ran. The pair's output is an unverified advisory claim.
	OutcomeUngated Outcome = "ungated"
)

// Result is what a pair run produced. Note what is NOT in it: an approval.
type Result struct {
	SchemaVersion string `json:"schema_version"`

	Role     string   `json:"role"`
	Acts     bool     `json:"acts"`
	Evidence Evidence `json:"evidence"`

	Proposer string `json:"proposer"` // tool:model
	Pair     string `json:"pair"`     // tool:model

	Proposal string `json:"proposal,omitempty"`

	// Contribution is what the pair produced: a diff when it acted, prose when it did not.
	Contribution string `json:"contribution"`

	// Outcome is what the two gate runs PROVED. It is the headline.
	Outcome Outcome `json:"outcome"`

	// GateBefore is the baseline. Without it, a red gate after the pair acted is
	// unattributable — it might be the pair's proof, or it might be that the proposer's
	// work never worked. You cannot tell, and guessing is how this codebase gets bugs.
	GateBefore *GateRun `json:"gate_before,omitempty"`
	GateAfter  *GateRun `json:"gate_after,omitempty"`

	// Diverse records whether the two agents came from DIFFERENT model families. When
	// false the pair is worth much less: same family means correlated errors, and two
	// models with the same blind spot agree confidently about the thing neither can see.
	Diverse       bool   `json:"diverse"`
	DiversityNote string `json:"diversity_note,omitempty"`
}

// GateRun is one execution of the real gate — a command, not a model.
type GateRun struct {
	Command   string `json:"command"`
	ExitCode  int    `json:"exit_code"`
	Passed    bool   `json:"passed"`
	Abstained bool   `json:"abstained"`
	Output    string `json:"output,omitempty"`
}

// Headline is the one line a human or a conductor reads.
func (r *Result) Headline() string {
	switch r.Outcome {
	case OutcomeProved:
		return fmt.Sprintf("PROVED — %s (%s) wrote a failing test against green. The defect is real; "+
			"the gate says so, not the model.", r.Pair, r.Role)
	case OutcomeRepaired:
		return fmt.Sprintf("REPAIRED — the gate was red; %s (%s) made it green.", r.Pair, r.Role)
	case OutcomeHeld:
		return fmt.Sprintf("HELD — %s (%s) attacked it and the gate stayed green. Nothing executable found.", r.Pair, r.Role)
	case OutcomeBrokenBefore:
		return "BROKEN BEFORE — the gate was already red when the pair started. Nothing it did can be " +
			"attributed. Fix the baseline, then pair."
	case OutcomeUnmeasured:
		return "UNMEASURED — a required gate state was absent or abstained. There is not enough " +
			"evidence to call the work green or red."
	case OutcomeUngated:
		return fmt.Sprintf("UNGATED — %s (%s) produced a claim that nothing checked. Believe it at your own risk.", r.Pair, r.Role)
	}
	return string(r.Outcome)
}

// Agents holds the two sides. Each is a `tool:model` binding.
type Agents struct {
	Proposer string
	Pair     string
}

// CheckDiversity reports whether the two agents come from DIFFERENT model families.
//
// This is the one place model diversity buys something real. Two agents from the same family
// share a blind spot, and a pair that shares your blind spot will agree with you confidently
// about the thing neither of you can see.
//
// It WARNS rather than refuses — a same-family pair is degraded, not useless, and an operator
// may have exactly one provider. But it says so, every time, in the result.
func CheckDiversity(cat *fleet.Catalog, a Agents) (diverse bool, note string) {
	if cat == nil {
		return true, "" // no catalog: we cannot see a problem, so we do not claim one
	}
	famOf := func(binding string) string {
		_, modelName, ok := strings.Cut(binding, ":")
		if !ok {
			return ""
		}
		m, found := cat.Model(modelName)
		if !found {
			return ""
		}
		if m.Family != "" {
			return m.Family
		}
		return m.Name
	}

	pf, cf := famOf(a.Proposer), famOf(a.Pair)
	switch {
	case pf == "" || cf == "":
		return true, "" // unknown family: do not claim a problem we cannot see
	case pf == cf:
		return false, fmt.Sprintf("proposer and pair are both from the %q family — they share a "+
			"blind spot, so agreement between them is not a second signal, it is the same signal "+
			"louder. Prefer a different model family for the pair.", pf)
	default:
		return true, ""
	}
}

// Plan is a validated pairing. Building one is where the contract is enforced; Run executes.
type Plan struct {
	Role   Role
	Agents Agents
	Task   string
	Gate   string // optional gate command; when supplied, it is authoritative.

	// Proposal, when set, is work that ALREADY EXISTS — a diff, a file, a branch. The
	// proposer does not run; the pair attacks what is already there.
	//
	// This is the common case, and the one `judge --diff` served: you have a change and you
	// want it broken before it merges. The full propose->pair->gate loop is for greenfield.
	Proposal string

	Diverse bool
	Note    string
}

// NewPlan validates a pairing before a single token is spent.
//
// Everything that can be wrong about a pairing is wrong HERE — not three minutes and two
// model invocations later.
func NewPlan(cat *fleet.Catalog, role Role, agents Agents, task, gate string) (*Plan, error) {
	if err := role.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(task) == "" {
		return nil, errors.New("pair: task is required")
	}
	if agents.Pair == "" {
		return nil, errors.New("pair: --pair is required (the agent that attacks the work)")
	}
	if agents.Proposer != "" && agents.Proposer == agents.Pair {
		return nil, fmt.Errorf("pair: proposer and pair are the SAME agent (%s) — a model cannot "+
			"refute itself. It will agree with the reasoning it just produced, because it IS the "+
			"reasoning it just produced", agents.Proposer)
	}
	diverse, note := CheckDiversity(cat, agents)
	return &Plan{Role: role, Agents: agents, Task: task, Gate: gate, Diverse: diverse, Note: note}, nil
}

// ErrNoWork is returned when there is neither a proposer to produce work nor existing work to
// attack. A pair with nothing to pair ON is a very expensive way to get an opinion.
var ErrNoWork = errors.New("pair: nothing to pair on — give me a --proposer to write the work, " +
	"or existing work to attack (an uncommitted diff, --diff, or --file)")

// PairPrompt builds the instruction handed to the pair.
//
// When the role is BLIND the proposal is withheld — not an omission, the feature.
func (p *Plan) PairPrompt(proposal string) string {
	var b strings.Builder
	b.WriteString(p.Role.Brief)
	b.WriteString("\n\n## The task\n\n")
	b.WriteString(p.Task)

	if p.Role.SeesProposal {
		b.WriteString("\n\n## The work\n\n")
		b.WriteString(proposal)
	}

	b.WriteString("\n\n## Your authority\n\n")
	if p.Role.Acts {
		b.WriteString("You have the KEYBOARD: edit files, write tests, run commands. Produce code, " +
			"not commentary — code can be executed, and a thing that can be executed does not need " +
			"anyone's permission to be believed.\n\n")
	} else {
		b.WriteString("You do NOT have the keyboard. Your output is prose, and prose is a claim " +
			"someone must decide whether to believe.\n\n")
	}
	switch p.Role.Authority {
	case AuthorityReject:
		b.WriteString("You may REJECT. You may NOT APPROVE.\n\n")
		if strings.TrimSpace(p.Gate) != "" {
			b.WriteString("Approval is not yours to give — the caller supplied a command gate, and " +
				"that gate determines the verified outcome. ")
		} else {
			b.WriteString("Approval is not yours to give — this run is ungated, so your result is " +
				"advisory and unverified. ")
		}
		b.WriteString("Never say \"looks good\" or \"LGTM\"; that is not a result you are " +
			"authorised to produce. If you attacked it honestly and it held, say exactly that instead.")
	case AuthorityAdvise:
		b.WriteString("You ADVISE. You may neither approve nor block. Your output informs a " +
			"decision; it does not make one.")
	}
	return b.String()
}

// Runner executes one agent turn in the workspace. When the role acts, the agent has write
// access to that workspace and its output is the DIFF it left behind.
type Runner func(ctx context.Context, agent, prompt string, acts bool) (string, error)

// GateRunner executes the real gate.
type GateRunner func(ctx context.Context, command string) (*GateRun, error)

// Run executes the plan:  gate (baseline) -> propose -> pair -> gate (again).
//
// The baseline is not ceremony. Without it, a red gate after an acting pair is
// UNATTRIBUTABLE — it might be the pair's proof, or the proposer's work might simply never
// have worked. Concluding "the pair found a bug" from a red gate you never saw green is a
// success state (well, a finding) reached by the ABSENCE of evidence, which is the exact
// defect class this codebase spent a day cataloguing. Do not commit it in the tool built to
// catch it.
func (p *Plan) Run(ctx context.Context, run Runner, gate GateRunner) (*Result, error) {
	var err error
	res := &Result{
		SchemaVersion: SchemaVersion,
		Role:          p.Role.Name,
		Acts:          p.Role.Acts,
		Evidence:      p.Role.Evidence,
		Proposer:      p.Agents.Proposer,
		Pair:          p.Agents.Pair,
		Diverse:       p.Diverse,
		DiversityNote: p.Note,
		Outcome:       OutcomeUngated,
	}

	gated := strings.TrimSpace(p.Gate) != "" && gate != nil

	// 1. Baseline. Only meaningful when the pair will ACT — a commentator changes nothing,
	//    so there is nothing to attribute and nothing to compare against.
	if gated && p.Role.Acts {
		before, err := gate(ctx, p.Gate)
		if err != nil {
			return nil, fmt.Errorf("gate (baseline): %w", err)
		}
		res.GateBefore = before
	}

	// 2. Propose — unless the work already exists, in which case there is nothing to propose.
	proposal := p.Proposal
	switch {
	case p.Agents.Proposer != "":
		out, err := run(ctx, p.Agents.Proposer, p.Task, true)
		if err != nil {
			return nil, fmt.Errorf("proposer %s: %w", p.Agents.Proposer, err)
		}
		proposal = out
	case strings.TrimSpace(proposal) == "" && p.Role.SeesProposal:
		// A sighted role with nothing to look at would "review" thin air and, being
		// agreeable, find nothing. Silence is not an all-clear.
		return nil, ErrNoWork
	}
	res.Proposal = proposal

	// 3. Pair.
	contribution, err := run(ctx, p.Agents.Pair, p.PairPrompt(proposal), p.Role.Acts)
	if err != nil {
		return nil, fmt.Errorf("pair %s: %w", p.Agents.Pair, err)
	}
	res.Contribution = contribution

	// 4. The gate. It runs REGARDLESS of what the pair said.
	//
	//    Not "if the pair approved" — the pair CANNOT approve. And not "unless the pair
	//    objected" either: an objection is a claim to check, not a proven defect, and
	//    skipping the gate on a model's say-so hands it back the authority this package
	//    takes away.
	if !gated {
		return res, nil
	}
	after, err := gate(ctx, p.Gate)
	if err != nil {
		return res, fmt.Errorf("gate: %w", err)
	}
	res.GateAfter = after
	res.Outcome = classify(res.GateBefore, after, p.Role.Acts)
	return res, nil
}

// classify turns two gate runs into what they PROVED.
//
// This is the whole payoff of an acting pair: the pair's finding adjudicates itself. A
// critic's claim needs a judge; a failing test needs an exit code.
func classify(before, after *GateRun, acts bool) Outcome {
	if after == nil {
		return OutcomeUngated
	}
	if after.Abstained || before != nil && before.Abstained {
		return OutcomeUnmeasured
	}

	// A commentator changed nothing. A red after gate is therefore a fact about the
	// PROPOSER's work, but it is not evidence that the unmeasured baseline was red.
	if !acts {
		if after.Passed {
			return OutcomeHeld
		}
		return OutcomeUnmeasured
	}

	// An acting pair needs a measured baseline to attribute a red after gate. Without one,
	// the work is red but there is no evidence for either a green or red before state.
	if before == nil {
		if after.Passed {
			return OutcomeHeld
		}
		return OutcomeUnmeasured
	}

	switch {
	case !before.Passed && after.Passed:
		return OutcomeRepaired
	case !before.Passed:
		// Red before AND red after. We cannot say the pair proved anything — the gate was
		// already failing, and a second failure on top of a failure is not a signal.
		return OutcomeBrokenBefore
	case !after.Passed:
		// Green, then the pair acted, then red. THE PAIR PROVED IT.
		return OutcomeProved
	default:
		return OutcomeHeld
	}
}
