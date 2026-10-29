package skills

// The SECOND identity — what a skill guarantees, as opposed to what it is.
//
// Identity(s) (dhnt attest.go) hashes the WHOLE canonical form, so it dedups
// BYTES: two skills that reach the same end-state by different steps are
// correctly different identities. That is right for versioning and wrong for
// cataloguing, because those two skills then sit in the catalog as peers — and
// peers are what a model has to choose between. Semantically-overlapping peers
// displacing each other at selection time is the measured failure mode ("skill
// shadowing"), and it dominates the damage as a catalog grows.
//
// CapabilityKey hashes only the CONTRACT and the EFFECT CAP: the postconditions
// a run must satisfy, and the upper bound on the blast radius it may cause. Two
// skills with the same key are the same capability with different
// implementations — exactly, by construction, not by embedding similarity. They
// become alternatives under one capability rather than rivals in one list.
//
// This is not a new invariant, it is the existing one taken at its word. dhnt's
// SPEC pins "the contract, not the steps, is the spine": steps are an
// implementation any executor tier may replace, and a run is judged only by the
// contract. If that is true, then the contract IS the capability's identity.
//
// Deliberately excluded from the projection:
//
//	Name       an alias, not a guarantee (see fleet's nickname discipline)
//	Caps       a REQUIREMENT ("needaso"), not a promise
//	Steps      the implementation — the whole point is to vary this
//	OnFail     recovery intent; two caps differing only in retry policy are one
//
// EffectCap IS included: a read-only build check and one that may write are not
// interchangeable, however identical their postconditions.

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	dhntskills "github.com/dhnt/dhnt/skills"
	"github.com/qiangli/yoke/pkg/unit"
)

// ErrNoContract reports that a skill declares no contract, and therefore has no
// capability key. It is unit.ErrNoContract — one refusal for every face.
//
// This is a REFUSAL, not a fallback. Hashing an empty projection would give
// every contract-less skill one shared key, silently merging skills that
// guarantee nothing in common — the precise opposite of the intent. A skill
// without postconditions has not said what it is for, so it cannot be matched
// against, elected, or deduplicated by guarantee; it stays a rung-0 citizen that
// clusters only approximately and never merges automatically.
//
// That is also the incentive the formalization ladder needs: writing a contract
// is what buys exact dedup and election.
var ErrNoContract = unit.ErrNoContract

// CapabilityKey returns the content address of what a skill GUARANTEES: the
// one unit capability key (unit.CapabilityKey) over the dhnt face's contract
// projection (DhntContract), with name, capabilities, steps and failure policy
// projected away.
//
// The key is orientation-neutral: a Bash# body whose @ensure/@effects state the
// same clauses keys identically, so a dhnt face and a Bash# face of one
// promise are one capability.
//
// Returns ErrNoContract when the skill declares no postconditions.
func CapabilityKey(s dhntskills.Skill) (string, error) {
	return unit.CapabilityKey(DhntContract(s))
}

// DhntContract projects a dhnt face onto the one unit contract: each contract
// clause becomes an Ensure (rendered by CheckText) and each effect-cap atom an
// Effects entry under its stable name (read, write, net, …) — the same
// vocabulary @effects and dag's Effects: use. Order is not semantic; the key
// sorts and deduplicates.
func DhntContract(s dhntskills.Skill) unit.Contract {
	var c unit.Contract
	for _, chk := range s.Contract {
		c.Ensure = append(c.Ensure, CheckText(chk))
	}
	for _, e := range s.EffectCap {
		c.Effects = append(c.Effects, e.String())
	}
	return c
}

// CheckText renders one dhnt contract clause as the text a Bash# @ensure
// states for it: the predicate id, then its named bindings sorted by name
// (args are NAMED, so their written order is not semantic) as name=value.
func CheckText(c dhntskills.Check) string {
	args := slices.Clone(c.Args)
	sort.SliceStable(args, func(i, j int) bool {
		if args[i].Name != args[j].Name {
			return args[i].Name < args[j].Name
		}
		return exprText(args[i].Value) < exprText(args[j].Value)
	})
	var b strings.Builder
	b.WriteString(c.Predicate)
	for _, a := range args {
		b.WriteString(" " + a.Name + "=" + exprText(a.Value))
	}
	return b.String()
}

// exprText renders a value expression: a ref by name, a numeral in decimal.
func exprText(e dhntskills.Expr) string {
	switch e.Kind {
	case dhntskills.ExprRef:
		return e.Ref
	case dhntskills.ExprNumber:
		return strconv.FormatUint(e.Number, 10)
	default:
		return "?"
	}
}
