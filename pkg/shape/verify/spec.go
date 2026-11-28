package verify

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/shape/evidence"
)

// A version's spec is the contract its own tree carries: the required
// checks under checks/, the contracted functions and the declared effects.
// Its fingerprint is Contract.Digest. Every verdict records the spec of the
// version it judged (Record.Spec) and the spec it was graded against
// (Binding.Contract), so a version and the spec it was verified against
// travel together through approval, undo and restore.
//
// A candidate changes the spec when it edits or removes a check the base
// requires. Adding a check is not a spec change: an added check runs in
// addition to the base's, never instead of one.

// VersionSpec is the Options.Spec value for a version already recorded on
// the principal's line (an undo or a restore). Such a version is graded
// against the spec it carries, not against the spec of the version it
// replaces: that spec was accepted when the version was activated.
const VersionSpec = "version"

// SpecFromCandidate is Record.SpecFrom when the checks were frozen from the
// candidate's own tree instead of the base's.
const SpecFromCandidate = "candidate"

// ErrSpecUnreviewed reports a candidate that changes the spec without an
// owner review of exactly that spec.
var ErrSpecUnreviewed = errors.New("spec change needs the owner's review")

// SpecChange lists the base checks the candidate edits or removes, sorted,
// each as "NAME (changed)" or "NAME (removed)". Empty means the candidate
// keeps the base spec.
func SpecChange(base, candidate Contract) []string {
	cand := map[string]string{}
	for _, c := range candidate.Checks {
		cand[c.Name] = c.Body
	}
	var out []string
	for _, b := range base.Checks {
		body, ok := cand[b.Name]
		switch {
		case !ok:
			out = append(out, b.Name+" (removed)")
		case body != b.Body:
			out = append(out, b.Name+" (changed)")
		}
	}
	sort.Strings(out)
	return out
}

// SpecDelta is what an owner reviews before a candidate may be graded
// against its own spec: the base and candidate spec digests and the checks
// the candidate changes.
type SpecDelta struct {
	Base      string   `json:"base"`
	Candidate string   `json:"candidate"`
	Changed   []string `json:"changed,omitempty"`
}

// ReadSpecDelta reads the spec of base and candidate from repoDir (never
// checked out or written) and returns their delta.
func ReadSpecDelta(repoDir, baseSHA, candidateSHA string, reader ContractReader) (SpecDelta, error) {
	if reader == nil {
		reader = UnitReader{}
	}
	clone, err := IsolatedClone(repoDir, baseSHA)
	if err != nil {
		return SpecDelta{}, err
	}
	defer os.RemoveAll(clone)
	base, err := reader.ReadContract(clone)
	if err != nil {
		return SpecDelta{}, err
	}
	if err := detach(clone, candidateSHA); err != nil {
		return SpecDelta{}, err
	}
	cand, err := reader.ReadContract(clone)
	if err != nil {
		return SpecDelta{}, err
	}
	return SpecDelta{Base: base.Digest(), Candidate: cand.Digest(), Changed: SpecChange(base, cand)}, nil
}

// gradingSpec decides which contract grades the candidate. It returns the
// contract to freeze checks from, the effect cap contract, and a refusal
// reason when the candidate's spec change is not authorized.
func gradingSpec(base, candidate Contract, spec string, changed []string) (checks, capFrom Contract, from, refusal string) {
	if len(changed) == 0 {
		return base, base, "", ""
	}
	switch strings.TrimSpace(spec) {
	case "":
		return base, base, "", fmt.Sprintf("%s: %s; the owner reviews the spec change before the candidate is graded against it",
			ErrSpecUnreviewed, strings.Join(changed, ", "))
	case VersionSpec:
		// A recorded version is graded against its own spec, cap included:
		// that is the spec it was verified and activated with.
		return candidate, candidate, SpecFromCandidate, ""
	case candidate.Digest():
		// The owner reviewed exactly this spec. Only the checks move; the
		// effect cap stays the base's, so a spec change is never an
		// escalation.
		return candidate, base, SpecFromCandidate, ""
	default:
		return base, base, "", fmt.Sprintf("%s: the reviewed spec %s is not the candidate's spec %s (%s)",
			ErrSpecUnreviewed, shortDigest(spec), shortDigest(candidate.Digest()), strings.Join(changed, ", "))
	}
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// RequireSpec checks that a sealed record graded a spec-changing candidate
// the only way a spec change may be graded for a merge: against the
// candidate's own spec, under the owner's review of exactly that spec. A
// candidate that keeps the base spec needs nothing more than the binding.
// A record graded by a recorded version's own spec (a restore) does not
// authorize a merge.
func RequireSpec(rec evidence.Record, delta SpecDelta) error {
	if len(delta.Changed) == 0 {
		return nil
	}
	switch {
	case rec.SpecFrom != SpecFromCandidate || rec.Spec != delta.Candidate || rec.Binding.Contract != delta.Candidate:
		return fmt.Errorf("%w: the candidate changes the spec (%s) but the evidence was not graded against it",
			evidence.ErrStale, strings.Join(delta.Changed, ", "))
	case rec.SpecReview == "":
		return fmt.Errorf("%w: %s", ErrSpecUnreviewed, strings.Join(delta.Changed, ", "))
	case rec.SpecReview != delta.Candidate:
		return fmt.Errorf("%w: the reviewed spec %s is not the candidate's spec %s",
			ErrSpecUnreviewed, shortDigest(rec.SpecReview), shortDigest(delta.Candidate))
	}
	return nil
}
