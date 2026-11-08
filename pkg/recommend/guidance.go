package recommend

import (
	"encoding/json"
	"strings"
)

// Capability-guidance record (schema "bashy-recommend-v1").
//
// Guidance is ONE versioned machine-readable record, Guidance, with fields:
//
//	schema_version  always GuidanceSchema ("bashy-recommend-v1")
//	kind            reuse | register | learn | shape | correct
//	reason          why this recommendation applies
//	evidence        strings backing the reason (paths, outputs, ids)
//	command         argv []string of the step (omitted when none)
//	prerequisites   what must hold before the step is taken
//	verify          how to verify the outcome
//	authorization   none | approval | operator — who must sign off
//	executable      true = a runnable step; false = a proposal only
//
// Extension order (GuidanceOrder, earliest applicable wins): existing
// composition (reuse) > declarative configuration/adapter (register) >
// execution-verified learned skill (learn) > shaped implementation (shape).
// "correct" is not a rung: it fixes a wrong guess and ranks outside the order.
// Producers are wired in a later story; this file only defines the record.

// GuidanceSchema is the schema_version of a Guidance record.
const GuidanceSchema = "bashy-recommend-v1"

// Kind classifies a recommendation.
type Kind string

const (
	KindReuse    Kind = "reuse"
	KindRegister Kind = "register"
	KindLearn    Kind = "learn"
	KindShape    Kind = "shape"
	KindCorrect  Kind = "correct"
)

// Authorization names who must approve acting on a recommendation.
type Authorization string

const (
	AuthNone     Authorization = "none"
	AuthApproval Authorization = "approval"
	AuthOperator Authorization = "operator"
)

// GuidanceOrder is the ranked extension order, most preferred first.
var GuidanceOrder = []Kind{KindReuse, KindRegister, KindLearn, KindShape}

// Guidance is the versioned recommendation record.
type Guidance struct {
	Schema        string        `json:"schema_version"`
	Kind          Kind          `json:"kind"`
	Reason        string        `json:"reason"`
	Evidence      []string      `json:"evidence"`
	Command       []string      `json:"command,omitempty"`
	Prerequisites []string      `json:"prerequisites"`
	Verify        string        `json:"verify"`
	Authorization Authorization `json:"authorization"`
	Executable    bool          `json:"executable"`
}

// NewGuidance returns a record with the schema set and non-nil slices.
func NewGuidance(kind Kind, reason string) Guidance {
	return Guidance{
		Schema: GuidanceSchema, Kind: kind, Reason: reason,
		Evidence: []string{}, Prerequisites: []string{}, Authorization: AuthNone,
	}
}

// Rank is the position of k in GuidanceOrder (0 = most preferred), or -1.
func Rank(k Kind) int {
	for i, o := range GuidanceOrder {
		if o == k {
			return i
		}
	}
	return -1
}

// Earliest returns the applicable kind that comes first in GuidanceOrder.
// Kinds outside the order are ignored; ok is false if none qualifies.
func Earliest(applicable ...Kind) (Kind, bool) {
	for _, o := range GuidanceOrder {
		for _, a := range applicable {
			if a == o {
				return o, true
			}
		}
	}
	return "", false
}

// MarshalGuidance renders g as a single JSON line (schema defaulted if unset).
func MarshalGuidance(g Guidance) ([]byte, error) {
	if g.Schema == "" {
		g.Schema = GuidanceSchema
	}
	if g.Evidence == nil {
		g.Evidence = []string{}
	}
	if g.Prerequisites == nil {
		g.Prerequisites = []string{}
	}
	return json.Marshal(g)
}

// Line is the human one-line stderr rendering of g.
func (g Guidance) Line() string {
	var b strings.Builder
	b.WriteString("bashy recommends [" + string(g.Kind) + "]: " + g.Reason)
	if len(g.Command) > 0 {
		verb := "propose"
		if g.Executable {
			verb = "run"
		}
		b.WriteString(" — " + verb + ": " + strings.Join(g.Command, " "))
	}
	if g.Authorization != "" && g.Authorization != AuthNone {
		b.WriteString(" (needs " + string(g.Authorization) + ")")
	}
	if g.Verify != "" {
		b.WriteString(" (verify: " + g.Verify + ")")
	}
	return b.String() + "\n"
}
