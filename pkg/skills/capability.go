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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"

	dhntskills "github.com/dhnt/dhnt/skills"
)

// ErrNoContract reports that a skill declares no contract, and therefore has no
// capability key.
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
var ErrNoContract = errors.New("skills: skill declares no contract, so it has no capability key")

// capabilityProjectionName is the fixed skill name used for the contract-only
// projection.
//
// LineariseDhnt REQUIRES a canonical, non-empty name, so the projection cannot
// simply zero it. Using one constant for every projection is what erases the
// name from the key: two skills under different names but one contract linearise
// identically here. The word is canonical dhnt (every consonant followed by a
// vowel) so the encoder accepts it.
const capabilityProjectionName = "kapabiliti"

// CapabilityKey returns the content address of what a skill GUARANTEES: a hash
// over its contract and effect cap alone, with name, capabilities, steps and
// failure policy projected away.
//
// The "k" prefix distinguishes it at a glance from Identity's "h". It reuses
// LineariseDhnt rather than introducing a serialiser, so the key is byte-
// compatible with every other dhnt fingerprint and cannot drift from the
// canonical form.
//
// Returns ErrNoContract when the skill declares no postconditions.
func CapabilityKey(s dhntskills.Skill) (string, error) {
	if len(s.Contract) == 0 {
		return "", ErrNoContract
	}
	canon, err := dhntskills.LineariseDhnt(capabilityProjection(s))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canon))
	return "k" + hex.EncodeToString(sum[:]), nil
}

// capabilityProjection strips a skill to the part that states its guarantee,
// then normalises the ordering of what remains.
//
// Normalisation matters because the key must not depend on authoring accidents.
// A contract is a SET of postconditions and an effect cap is a SET of atoms —
// neither carries meaning in its written order — so both are sorted and
// deduplicated. Without that, re-ordering two `enisure` clauses would mint a new
// capability and split a matched pair apart.
func capabilityProjection(s dhntskills.Skill) dhntskills.Skill {
	return dhntskills.Skill{
		Name:      capabilityProjectionName,
		EffectCap: normaliseEffects(s.EffectCap),
		Contract:  normaliseContract(s.Contract),
	}
}

// normaliseEffects sorts the cap into lattice order and drops duplicates.
func normaliseEffects(in []dhntskills.Effect) []dhntskills.Effect {
	if len(in) == 0 {
		return nil
	}
	out := make([]dhntskills.Effect, len(in))
	copy(out, in)
	slices.Sort(out)
	return slices.Compact(out)
}

// normaliseContract sorts contract clauses by their canonical text and drops
// exact duplicates. Each clause's own arguments are sorted too: args are NAMED
// bindings, so their written order is not semantic even though linearisation
// emits them positionally.
func normaliseContract(in []dhntskills.Check) []dhntskills.Check {
	if len(in) == 0 {
		return nil
	}
	out := make([]dhntskills.Check, 0, len(in))
	for _, c := range in {
		out = append(out, dhntskills.Check{Predicate: c.Predicate, Args: normaliseArgs(c.Args)})
	}
	sort.SliceStable(out, func(i, j int) bool { return checkKey(out[i]) < checkKey(out[j]) })
	return slices.CompactFunc(out, func(a, b dhntskills.Check) bool { return checkKey(a) == checkKey(b) })
}

// normaliseArgs sorts named bindings by name, tie-breaking on value so the
// result is total even if a name repeats.
func normaliseArgs(in []dhntskills.Arg) []dhntskills.Arg {
	if len(in) == 0 {
		return nil
	}
	out := make([]dhntskills.Arg, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return exprKey(out[i].Value) < exprKey(out[j].Value)
	})
	return out
}

// checkKey renders one contract clause as a comparable string. It is an
// ordering aid only — never hashed, never stored — so its shape can change
// freely without moving any capability key.
func checkKey(c dhntskills.Check) string {
	var b strings.Builder
	b.WriteString(c.Predicate)
	for _, a := range c.Args {
		b.WriteByte('\x00')
		b.WriteString(a.Name)
		b.WriteByte('=')
		b.WriteString(exprKey(a.Value))
	}
	return b.String()
}

// exprKey renders a value expression comparably, keeping the variants in
// distinct namespaces so a ref never collides with a numeral.
func exprKey(e dhntskills.Expr) string {
	switch e.Kind {
	case dhntskills.ExprRef:
		return "r:" + e.Ref
	case dhntskills.ExprNumber:
		return "n:" + strconv.FormatUint(e.Number, 10)
	default:
		return "?:"
	}
}
