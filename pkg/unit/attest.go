// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package unit is the one contract + receipt vocabulary bashy's surfaces
// share. A unit of work — a dag target, a skill run, a function call — is
// contracted the same way (Require: preconditions, Ensure: postconditions,
// Effects: the declared cap; the canonical definitions live in pkg/dag's
// parser and contract engine) and its completion is sealed the same way:
// one Attestation type, defined here, that every receipt surface embeds.
//
// dag.Attestation is an alias of Attestation, so a dag target receipt IS a
// unit receipt; skills.AttestRecord embeds one beside the dhnt attestation
// its executor returns (the external module's shape, wrapped — never
// forked). The JSON vocabulary is additive by rule: fields are only ever
// added, never renamed or removed, because receipts are append-only
// evidence and every reader must keep decoding them.
//
// The sibling package unit/frontmatter is the other half of this story: the
// one reader of the `---` blocks that carry a unit's prose face. fence.go is
// the one Bash# fence extractor (dag.md's fence grammar) and contract.go the
// one contract reader and capability key, so a SKILL.md Bash# body, a dag
// target and a dhnt face keeping the same promise share one key.
package unit

// CheckResult is the outcome of one contract check (a Require
// precondition or an Ensure postcondition).
type CheckResult struct {
	Expr   string `json:"expr"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Attestation is the sealed verdict for one unit of work: did its body exit
// cleanly AND do all its Ensure postconditions hold, with its declared
// Effects recorded. Valid is the agent-trustable bit — success judged by
// world-state, not just the exit code (which is what make stops at).
//
// The same type seals a dag target receipt (dag.Attestation is an alias of
// this) and rides inside a skill run receipt (skills.AttestRecord embeds
// it), so evidence pooled from either surface speaks one vocabulary.
type Attestation struct {
	Target  string        `json:"target"`
	Valid   bool          `json:"valid"`
	Checks  []CheckResult `json:"checks,omitempty"`
	Effects []string      `json:"effects,omitempty"`
	// Clause is "require" when a precondition sealed this verdict before the
	// body ran; empty for the postcondition verdict (the shape shipped
	// first).
	Clause string `json:"clause,omitempty"`
	// Identity is the content address of the exact unit that ran, so a
	// receipt names not just the outcome but the version the outcome is
	// evidence about. Empty in receipts written before it is stamped;
	// readers treat empty as "not stamped", never as an address.
	Identity string `json:"identity,omitempty"`
	// Capability is the guarantee the unit was keeping, so evidence pools
	// across interchangeable implementations of the same promise rather
	// than only per name. Empty in receipts written before it is stamped.
	Capability string `json:"capability,omitempty"`
	// Uncontracted marks the receipt of a unit that states no contract (no
	// Require, no Ensure): it ran, but there was nothing to verify, so Valid
	// is never set for it. Absent on every contracted receipt.
	Uncontracted bool `json:"uncontracted,omitempty"`
}
