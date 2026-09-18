package weave

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
)

// Opt-in model review metadata and eligibility.
//
// A configured deterministic PROBE (verify / suite / clean-room gate) is always
// honored. An LLM adversarial review runs only when the caller supplies
// --review-agent. Persisted tiers remain for compatibility and auditability; they
// do not make an ordinary pull, salvage, or autopilot invoke a model.

const (
	// weaveJudgeNone — the deterministic probe is sufficient; no LLM verdict is
	// sought for this item.
	weaveJudgeNone = "none"
	// weaveJudgeRequired is retained as explicit compatibility metadata. Model
	// review is activated by --review-agent, not by a workflow default.
	weaveJudgeRequired = "required"

	// weaveJudgeBandFloor is the minimum band a judge must serve to issue a
	// binding verdict. A verdict is a judgment call; an L2 model is a coder, not a
	// lead, so it may never be the arbiter regardless of the issue's own band.
	weaveJudgeBandFloor = 3
)

// weaveValidJudgeTier reports whether s is an accepted verifiability tier. Empty
// is accepted (it reads as the deterministic-only default at use time).
func weaveValidJudgeTier(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", weaveJudgeNone, weaveJudgeRequired:
		return true
	default:
		return false
	}
}

// weaveJudgeMode normalizes an item's declared verifiability tier. An empty or
// unrecognized value reads as "none": deterministic verify/suite gates are the
// default authority, including for queue records created by older clients.
func weaveJudgeMode(it *weaveItem) string {
	if it != nil && strings.EqualFold(strings.TrimSpace(it.Judge), weaveJudgeRequired) {
		return weaveJudgeRequired
	}
	return weaveJudgeNone
}

// weaveBindingBand resolves the capability band of a tool:model binding via the
// fleet catalog. 0 when the model is unknown or unpegged.
func weaveBindingBand(model string) int {
	if strings.TrimSpace(model) == "" {
		return 0
	}
	if m, ok := fleetCatalog().Model(model); ok {
		return m.Band
	}
	return 0
}

// weaveModelFamily resolves a model's declared family, falling back to the model
// name when the catalog has no record (so two unknown models never collapse into
// one family by accident). Used by the coder/judge family-separation rule.
func weaveModelFamily(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if m, ok := fleetCatalog().Model(model); ok {
		if m.Family != "" {
			return m.Family
		}
		return m.Name
	}
	return model
}

// weaveAgentBand resolves the band a resolved judge SERVES. A cascade agent's
// served band (Agent.Band) wins — that is its contract; otherwise the band is
// inherited from the bound model.
func weaveAgentBand(l *weaveAgentLaunch) int {
	if l == nil {
		return 0
	}
	if a, ok := fleetCatalog().Agent(l.Nick); ok && a.Band > 0 {
		return a.Band
	}
	return weaveBindingBand(l.ModelName)
}

// weaveIssueBand is the issue's difficulty band. An explicit peg (it.Band) wins;
// otherwise the coding agent's own band is a lower bound on the difficulty it was
// assigned to.
func weaveIssueBand(it *weaveItem) int {
	if it == nil {
		return 0
	}
	if it.Band > 0 {
		return it.Band
	}
	_, _, coderModel := weaveCodingIdentity(it)
	return weaveBindingBand(coderModel)
}

// weaveJudgeEligibility enforces the three constraints a judge must satisfy to
// issue a BINDING verdict for an item. Any failure is a REFUSAL — the absence of
// an eligible judge is never treated as a pass:
//
//   - (a) BAND FLOOR: the judge serves at >= max(L3, issue band). A verdict is a
//     lead's call; below L3 it is not trustworthy, and it must at least match the
//     difficulty the issue itself demanded.
//   - (b) FAMILY SEPARATION: the judge's model family differs from the coder's, so
//     a family cannot grade its own work (the separation rule extended from
//     weaveSelectReviewAgent's binding check down to the family).
func weaveJudgeEligibility(l *weaveAgentLaunch, coderModel string, issueBand int) error {
	if l == nil {
		return fmt.Errorf("no judge resolved")
	}
	floor := weaveJudgeBandFloor
	if issueBand > floor {
		floor = issueBand
	}
	if band := weaveAgentBand(l); band < floor {
		var eligible []string
		if cat := fleetCatalog(); cat != nil {
			if list, err := cat.Agents(); err == nil {
				for _, a := range list {
					if l2, err2 := weaveResolveAgent(a.Name); err2 == nil && l2 != nil {
						if b2 := weaveAgentBand(l2); b2 >= floor {
							coderFam := weaveModelFamily(coderModel)
							judgeFam := weaveModelFamily(l2.ModelName)
							if coderFam == "" || judgeFam == "" || !strings.EqualFold(coderFam, judgeFam) {
								eligible = append(eligible, a.Name)
							}
						}
					}
				}
			}
		}
		eligibleInfo := "none available"
		if len(eligible) > 0 {
			eligibleInfo = strings.Join(eligible, ", ")
		}
		return fmt.Errorf("judge %s serves band %s, below the required floor %s (max of L%d and the issue's band L%d); eligible reviewer(s) serving >= %s: %s",
			l.Binding(), fleet.BandLabel(band), fleet.BandLabel(floor), weaveJudgeBandFloor, issueBand, fleet.BandLabel(floor), eligibleInfo)
	}
	coderFam := weaveModelFamily(coderModel)
	judgeFam := weaveModelFamily(l.ModelName)
	if coderFam != "" && judgeFam != "" && strings.EqualFold(coderFam, judgeFam) {
		return fmt.Errorf("judge %s shares the coder's model family %q; a verdict must come from a different family", l.Binding(), coderFam)
	}
	return nil
}

// weaveResolveJudge resolves the requested reviewer to an ELIGIBLE judge for the
// item, or returns an error naming the refusal. It composes the existing
// duty-separation selection (weaveSelectReviewAgent) with the band-floor and
// family-separation eligibility checks. It is the fail-closed gate the merge path
// consults before ever invoking the pair runner.
//
// It is a package var so tests can drive the merge/autopilot loops without a
// populated fleet catalog, exactly as weavePairReviewRunner is replaceable.
var weaveVetJudge = weaveResolveJudge

func weaveResolveJudge(requested string, it *weaveItem) (reviewer, coder string, err error) {
	reviewer, coder, err = weaveSelectReviewAgent(requested, it)
	if err != nil {
		return "", coder, err
	}
	if reviewer == "" {
		return "", coder, fmt.Errorf("no review agent resolved from %q", requested)
	}
	l, rerr := weaveResolveAgent(reviewer)
	if rerr != nil {
		return "", coder, fmt.Errorf("resolve judge %q: %w", reviewer, rerr)
	}
	if l == nil {
		return "", coder, fmt.Errorf("judge %q is not a resolvable fleet agent", reviewer)
	}
	_, _, coderModel := weaveCodingIdentity(it)
	if eerr := weaveJudgeEligibility(l, coderModel, weaveIssueBand(it)); eerr != nil {
		return "", coder, eerr
	}
	return reviewer, coder, nil
}

// weaveRequireEligibleJudge is the pre-pass shared by pull and autopilot when a
// reviewer was explicitly requested: every submitted item in scope must have an
// eligible reviewer. It is a no-op on the normal reviewer-free path.
func weaveRequireEligibleJudge(items []*weaveItem, reviewAgent string, issueID int64, issueSpecified bool) error {
	if strings.TrimSpace(reviewAgent) == "" {
		return nil
	}
	for _, it := range items {
		if it == nil {
			continue
		}
		if issueSpecified && it.ID != issueID {
			continue
		}
		if it.State != "submitted" {
			continue
		}
		reviewer, _, jerr := weaveVetJudge(reviewAgent, it)
		if jerr != nil || reviewer == "" {
			if jerr == nil {
				jerr = fmt.Errorf("no eligible judge resolved for %q", reviewAgent)
			}
			return fmt.Errorf("run #%d requires a passing adversarial verdict but no eligible judge is available: %w", it.ID, jerr)
		}
	}
	return nil
}
