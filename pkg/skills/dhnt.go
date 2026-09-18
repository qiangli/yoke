package skills

// This file is the package's single touchpoint with the dhnt skill-CNL
// runtime (github.com/dhnt/dhnt, Apache-2.0) — the machine face of a
// dual-bundle skill. Everything else in pkg/skills stays dep-free of it.
//
// Validity is transpilability: skill.dhnt is valid iff it parses to the
// typed AST (whose canonical re-linearisation is what Identity hashes).

import (
	"strings"

	dhntskills "github.com/dhnt/dhnt/skills"
)

// DhntInfo is the parsed summary of a skill's canonical face.
type DhntInfo struct {
	Identity string // "h" + sha256(canonical form) — the content address
	// Capability is "k" + sha256(contract+cap projection) — what the skill
	// GUARANTEES, ignoring how. Empty when the skill declares no contract
	// (see ErrNoContract): a skill that has not said what it is for cannot be
	// matched by guarantee, and must not be merged with one that has.
	Capability string
	Contract   []string // contract predicate ids (dhnt-canonical)
	EffectCap  []string // declared effect-cap atoms
	Steps      int
	// HasJudgeStep reports a step whose latitude is judge (P4) anywhere in the
	// body, branches included. It is what turns a skill from a deterministic
	// runbook into an agentic one, so a consumer projecting "how much freedom
	// does running this take" needs the bit without re-parsing the face.
	HasJudgeStep bool
	Err          string // non-empty when skill.dhnt is invalid (parse/identity)
}

// Valid reports whether the canonical face parsed and hashed cleanly.
func (d *DhntInfo) Valid() bool { return d != nil && d.Err == "" && d.Identity != "" }

// parseDhntInfo validates a skill.dhnt body. Invalid content degrades to
// an Err — a broken canonical face never hides the prose skill.
func parseDhntInfo(canon []byte) *DhntInfo {
	src := strings.TrimSpace(string(canon))
	if src == "" {
		return &DhntInfo{Err: "empty skill.dhnt"}
	}
	sk, err := dhntskills.ParseDhnt(src)
	if err != nil {
		return &DhntInfo{Err: err.Error()}
	}
	id, err := dhntskills.Identity(sk)
	if err != nil {
		return &DhntInfo{Err: err.Error()}
	}
	info := &DhntInfo{Identity: id, Steps: len(sk.Steps), HasJudgeStep: anyJudgeStep(sk.Steps)}
	// A missing capability key is a legitimate state (no contract), not a
	// parse failure — it must never invalidate an otherwise-good face.
	if key, err := CapabilityKey(sk); err == nil {
		info.Capability = key
	}
	for _, c := range sk.Contract {
		info.Contract = append(info.Contract, c.Predicate)
	}
	for _, e := range sk.EffectCap {
		info.EffectCap = append(info.EffectCap, e.String())
	}
	return info
}

// anyJudgeStep walks a step list, branches included, for a judge-latitude
// leaf. Only leaves carry latitude — a Branch node's own Latitude is unused.
func anyJudgeStep(steps []dhntskills.Step) bool {
	for _, st := range steps {
		if st.Branch != nil {
			if anyJudgeStep(st.Branch.Then) || anyJudgeStep(st.Branch.Else) {
				return true
			}
			continue
		}
		if st.Latitude == dhntskills.LatJudge {
			return true
		}
	}
	return false
}
