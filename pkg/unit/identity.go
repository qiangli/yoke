// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package unit provides shared envelope readers and identity helpers for bashy
// execution units (skills, dag targets, and .bsh scripts).
package unit

import (
	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/unit/identity"
)

// ErrNoContract reports that a unit declares no contract.
var ErrNoContract = identity.ErrNoContract

// Header is the envelope metadata read from a script's leading comment block.
type Header = identity.Header

// DagTargetIdentity computes the content address ("h" + sha256) of a dag target
// over canonical bytes = the target's heading line + its metadata lines + its body.
func DagTargetIdentity(task *dag.Task) string {
	return identity.DagTargetIdentity(task)
}

// DagTargetCapability computes the contract projection hash ("k" + sha256) of a dag target
// over its Require/Ensure/Effects contract projection.
func DagTargetCapability(task *dag.Task) string {
	return identity.DagTargetCapability(task)
}

// DagTargetCapabilityKey returns DagTargetCapability or ErrNoContract if no contract is declared.
func DagTargetCapabilityKey(task *dag.Task) (string, error) {
	return identity.DagTargetCapabilityKey(task)
}

// ScriptIdentity computes the content address ("h" + sha256) of a .bsh script
// over whole file bytes with CRLF normalized to LF.
func ScriptIdentity(src []byte) string {
	return identity.ScriptIdentity(src)
}

// ScriptHeader reads the envelope (name, description, type) from a script's
// leading '#' comment block.
func ScriptHeader(src []byte) Header {
	return identity.ScriptHeader(src)
}
