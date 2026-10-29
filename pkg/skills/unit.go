package skills

// One unit model. A skill is ONE executable unit whichever face states it:
//
//	SKILL.md with Bash# fences   the canonical document — prose + script in one
//	                             file, identity = the Bash# body's content address
//	skill.dhnt beside SKILL.md   the dhnt face — canonical on its own, or a face
//	                             DERIVED from the Bash# body
//
// A skill that carries both faces must say which is the source: the derived
// dhnt face is admitted only when SKILL.md's `metadata.derived-from` names the
// Bash# body's identity (skill.dhnt's alphabet has no room for the
// declaration) and the face promises the same capability. Anything else is
// AMBIGUOUS and refused — a host that silently picked one face would run, admit
// and key a different unit from the one its author is editing.
//
// The identity, the contract reader and the capability key are pkg/unit's,
// shared with dag targets, craft and receipts; this file only decides which
// face a skill's unit is read from.

import (
	"fmt"

	"github.com/qiangli/yoke/pkg/unit"
	"github.com/qiangli/yoke/pkg/unit/identity"
)

// DerivedFromKey is the SKILL.md metadata key a derived skill.dhnt face is
// declared under: its value is the identity of the Bash# body it derives from.
const DerivedFromKey = "derived-from"

// Unit faces.
const (
	FaceBashSharp = "bashsharp" // SKILL.md Bash# fences (the canonical document)
	FaceDhnt      = "dhnt"      // a skill.dhnt with no Bash# body
)

// UnitInfo is a skill's one unit identity, read from whichever face carries it.
type UnitInfo struct {
	Face       string        `json:"face,omitempty"`       // FaceBashSharp, FaceDhnt, or "" (prose-only)
	Identity   string        `json:"identity,omitempty"`   // content address of the unit's body
	Capability string        `json:"capability,omitempty"` // "" when uncontracted
	Contract   unit.Contract `json:"contract"`
	Derived    bool          `json:"derived,omitempty"` // a skill.dhnt face declared as derived from the Bash# body
	Err        string        `json:"error,omitempty"`   // ambiguous or invalid unit
}

// Valid reports a unit that can be admitted and run: one unambiguous,
// well-formed face.
func (u UnitInfo) Valid() bool { return u.Face != "" && u.Err == "" && u.Identity != "" }

// Verified reports a valid unit that states a contract. An uncontracted unit
// can run, but nothing about it is ever verified.
func (u UnitInfo) Verified() bool { return u.Valid() && u.Capability != "" }

// ResolveUnit reads a skill's unit from its SKILL.md document, its frontmatter
// metadata, and its skill.dhnt face (dhnt nil when absent). It is the one
// decision every consumer (catalog, admission, run, record, craft) shares.
func ResolveUnit(document []byte, meta map[string]string, dhnt *DhntInfo) UnitInfo {
	bs, hasBody := identity.BashSharp(document)
	switch {
	case !hasBody && dhnt == nil:
		return UnitInfo{} // prose-only: discovery, never machine-run
	case !hasBody:
		u := UnitInfo{Face: FaceDhnt, Identity: dhnt.Identity, Capability: dhnt.Capability, Contract: dhnt.UnitContract}
		if !dhnt.Valid() {
			u.Err = "skill.dhnt: " + dhnt.Err
		}
		return u
	}
	u := UnitInfo{Face: FaceBashSharp, Identity: bs.Identity, Capability: bs.Capability, Contract: bs.Contract}
	if bs.Err != "" {
		u.Err = "Bash# body: " + bs.Err
		return u
	}
	if dhnt == nil {
		return u
	}
	declared := meta[DerivedFromKey]
	switch {
	case declared == "":
		u.Err = fmt.Sprintf("ambiguous unit: SKILL.md carries a Bash# body (%s) and a skill.dhnt face sits beside it with no declared derivation — keep one face, or declare metadata.%s: %s", bs.Identity, DerivedFromKey, bs.Identity)
	case declared != bs.Identity:
		u.Err = fmt.Sprintf("ambiguous unit: skill.dhnt declares it derives from %s but the Bash# body is %s — regenerate the derived face or drop it", declared, bs.Identity)
	case !dhnt.Valid():
		u.Err = "derived skill.dhnt: " + dhnt.Err
	case dhnt.Capability != bs.Capability:
		u.Err = fmt.Sprintf("ambiguous unit: derived skill.dhnt promises capability %q but the Bash# body promises %q", dhnt.Capability, bs.Capability)
	default:
		u.Derived = true
	}
	return u
}
