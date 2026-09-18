package craft

// ABSORPTION — digest, do not memorize.
//
// The failure this exists to prevent is the obvious implementation: read N
// external skills, write N catalog entries, declare victory. That does not grow
// a body of knowledge, it grows a pile — and past a few dozen entries a pile
// actively degrades an agent, because semantically-overlapping entries displace
// each other at selection time. Adding a hundred skills can leave a system
// measurably worse than adding none.
//
// So absorption resolves every candidate against what is already known, by
// CAPABILITY rather than by name:
//
//	novel        a guarantee nothing here makes yet          -> a new capability
//	alternative  the same guarantee, another implementation  -> an alt edge
//	duplicate    the same guarantee, byte-identical          -> evidence, no entry
//	quarantined  no contract, so no capability can be known  -> held, not merged
//	refused      license, or a candidate that will not parse -> nothing stored
//
// Only `novel` grows the catalog. That ratio — capabilities added over skills
// absorbed — is the DIGESTION RATIO, and it is the measurable form of the whole
// claim. At 1:1 nothing was digested.
//
// # The model sits behind a hook
//
// Turning prose into a typed contract needs a model, and a model in the
// critical path would make this untestable and unusable offline. So Normaliser
// is an injected function (the pattern pkg/fleet already uses for LiveProbe and
// ContextCloner): coreutils stays dep-free and hermetic, the host wires a real
// one, and tests inject a deterministic stub.
//
// Absent a normaliser, prose candidates are QUARANTINED rather than guessed at.
// A skill with no contract has not said what it guarantees, and inventing one
// would file it under a promise nobody verified.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	dhntskills "github.com/dhnt/dhnt/skills"

	"github.com/qiangli/yoke/pkg/skills"
)

// Disposition is what absorption decided about one candidate.
type Disposition string

const (
	DispNovel       Disposition = "novel"       // a guarantee not held yet — the catalog grows
	DispAlternative Disposition = "alternative" // same guarantee, different implementation
	DispDuplicate   Disposition = "duplicate"   // same guarantee, same implementation
	DispQuarantined Disposition = "quarantined" // no contract, so no capability is knowable
	DispRefused     Disposition = "refused"     // license, or unparsable
)

// Grew reports whether this disposition adds a catalog entry. Only novel does —
// which is what keeps the digestion ratio meaningful.
func (d Disposition) Grew() bool { return d == DispNovel }

// Source records where a candidate came from, and under what terms.
//
// License is recorded PER CANDIDATE, never inherited from a repository. Two of
// the four upstream sources surveyed declare no repo license at all, and one of
// those mixes redistributable and non-redistributable skills in a single tree —
// so a repo-level verdict is wrong in both directions.
type Source struct {
	Origin  string `json:"origin,omitempty"`  // repo URL or local path
	Ref     string `json:"ref,omitempty"`     // commit sha, when known
	Path    string `json:"path,omitempty"`    // path within the source
	License string `json:"license,omitempty"` // SPDX id, positively identified
}

// Candidate is one external skill offered for absorption.
type Candidate struct {
	Name        string
	Description string
	Body        string // the prose skill, verbatim
	Canonical   string // an existing skill.dhnt face, when the source ships one
	Source      Source
}

// Outcome is what happened to one candidate.
type Outcome struct {
	Name        string      `json:"name"`
	Disposition Disposition `json:"disposition"`
	Capability  string      `json:"capability,omitempty"`
	Identity    string      `json:"identity,omitempty"`
	// Reason is always populated for refused and quarantined, so an exclusion
	// is auditable rather than silent.
	Reason string `json:"reason,omitempty"`
	Source Source `json:"source"`
}

// Report is the result of one absorption run.
type Report struct {
	Outcomes []Outcome `json:"outcomes"`

	// Absorbed counts candidates that were considered (not refused).
	Absorbed int `json:"absorbed"`
	// Grew counts capabilities genuinely added.
	Grew int `json:"capabilities_added"`
}

// DigestionRatio is capabilities added over candidates absorbed.
//
// The headline metric, and the precondition for evaluating this system at all:
// a ratio near 1.0 means each external skill became its own entry, i.e. nothing
// was digested, the catalog is a pile, and any downstream "composition" score
// is really measuring lookup. Report it beside every result.
func (r Report) DigestionRatio() float64 {
	if r.Absorbed == 0 {
		return 0
	}
	return float64(r.Grew) / float64(r.Absorbed)
}

// Normaliser turns prose into a typed dhnt skill.
//
// Injected rather than imported: it needs a model, and a hard dependency on one
// would make absorption untestable and unusable offline. Returning
// ErrNotNormalisable (or any error) is a legitimate answer — the candidate is
// quarantined, not guessed at.
type Normaliser func(c Candidate) (dhntskills.Skill, error)

// ErrNotNormalisable reports prose that could not be turned into a typed skill.
var ErrNotNormalisable = errors.New("craft: candidate could not be normalised to a typed skill")

// permissive is the closed set of licenses whose content may be absorbed.
//
// Fail-closed: absence of a license is NOT permission, it is
// all-rights-reserved by default. A candidate whose license cannot be
// positively identified is refused, never assumed.
var permissive = map[string]bool{
	"MIT": true, "Apache-2.0": true, "BSD-2-Clause": true,
	"BSD-3-Clause": true, "ISC": true,
}

// Permissive reports whether an SPDX id is in the allowed set.
func Permissive(spdx string) bool { return permissive[strings.TrimSpace(spdx)] }

// Study absorbs candidates against the capabilities already known.
//
// known is the set of capability keys already held, and identities maps a
// capability to the implementations under it; both are typically derived from
// the live catalog. norm may be nil, in which case prose candidates are
// quarantined rather than normalised.
func Study(candidates []Candidate, known map[string][]string, norm Normaliser) Report {
	if known == nil {
		known = map[string][]string{}
	}
	// Work on a copy: absorption must not mutate a caller's catalog view, and a
	// candidate absorbed earlier in the run has to be visible to later ones or
	// two identical inputs would both report novel.
	seen := make(map[string][]string, len(known))
	for k, v := range known {
		seen[k] = append([]string(nil), v...)
	}

	var rep Report
	for _, c := range candidates {
		o := studyOne(c, seen, norm)
		rep.Outcomes = append(rep.Outcomes, o)
		if o.Disposition == DispRefused {
			continue
		}
		rep.Absorbed++
		if o.Disposition.Grew() {
			rep.Grew++
		}
		if o.Capability != "" && o.Identity != "" {
			seen[o.Capability] = appendDistinct(seen[o.Capability], o.Identity)
		}
	}
	return rep
}

func studyOne(c Candidate, seen map[string][]string, norm Normaliser) Outcome {
	out := Outcome{Name: strings.TrimSpace(c.Name), Source: c.Source}
	if out.Name == "" {
		out.Disposition = DispRefused
		out.Reason = "candidate has no name"
		return out
	}

	// The license gate runs FIRST. Parsing, normalising, or storing content we
	// have no right to redistribute is work that should never begin, and doing
	// it "just to see" is how unlicensed content ends up in a store.
	if !Permissive(c.Source.License) {
		out.Disposition = DispRefused
		if strings.TrimSpace(c.Source.License) == "" {
			out.Reason = "no license identified; absence of a license is not permission"
		} else {
			out.Reason = fmt.Sprintf("license %q is not in the permissive set", c.Source.License)
		}
		return out
	}

	sk, err := typedSkill(c, norm)
	if err != nil {
		out.Disposition = DispQuarantined
		out.Reason = err.Error()
		return out
	}

	id, err := dhntskills.Identity(sk)
	if err != nil {
		out.Disposition = DispRefused
		out.Reason = "typed skill has no valid identity: " + err.Error()
		return out
	}
	out.Identity = id

	key, err := skills.CapabilityKey(sk)
	if err != nil {
		// A typed skill with no contract states no guarantee. It is held, not
		// merged: filing it under a shared "no contract" bucket would silently
		// unify skills that have nothing in common.
		out.Disposition = DispQuarantined
		out.Reason = "declares no contract, so no capability can be determined"
		return out
	}
	out.Capability = key

	impls, held := seen[key]
	switch {
	case !held:
		out.Disposition = DispNovel
	case containsString(impls, id):
		out.Disposition = DispDuplicate
		out.Reason = "an identical implementation of this capability is already held"
	default:
		out.Disposition = DispAlternative
		out.Reason = fmt.Sprintf("same guarantee as %d implementation(s) already held", len(impls))
	}
	return out
}

// typedSkill resolves a candidate to a typed skill: its own canonical face when
// it ships one, otherwise the normaliser.
//
// A shipped face is preferred over normalisation even when a normaliser is
// available — it is the author's own declaration, and re-deriving it through a
// model could only lose fidelity.
func typedSkill(c Candidate, norm Normaliser) (dhntskills.Skill, error) {
	if canon := strings.TrimSpace(c.Canonical); canon != "" {
		sk, err := dhntskills.ParseDhnt(canon)
		if err != nil {
			return dhntskills.Skill{}, fmt.Errorf("canonical face does not parse: %w", err)
		}
		return sk, nil
	}
	if norm == nil {
		return dhntskills.Skill{}, ErrNotNormalisable
	}
	sk, err := norm(c)
	if err != nil {
		return dhntskills.Skill{}, fmt.Errorf("%w: %v", ErrNotNormalisable, err)
	}
	return sk, nil
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// LoadDir reads candidates from a directory of skill folders, each holding a
// SKILL.md and optionally a skill.dhnt and a LICENSE.
//
// Local only, by design: tests here are hermetic (no network), and fetching is
// a separate concern that belongs above this layer. A caller that pulls from a
// remote source materialises it first, then studies the directory.
func LoadDir(root string, src Source) ([]Candidate, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		body, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			continue // not a skill folder
		}
		sk, err := skills.ParseFrontmatter(body)
		if err != nil {
			// Malformed frontmatter degrades to the directory name rather than
			// dropping the candidate silently; the pipeline will still refuse
			// or quarantine it downstream, with a reason.
			sk = skills.Skill{Name: e.Name()}
		}
		c := Candidate{
			Name:        firstNonEmpty(sk.Name, e.Name()),
			Description: sk.Description,
			Body:        string(body),
			Source:      src,
		}
		c.Source.Path = filepath.Join(src.Path, e.Name())
		if canon, err := os.ReadFile(filepath.Join(dir, "skill.dhnt")); err == nil {
			c.Canonical = string(canon)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
