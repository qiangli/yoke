// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package src

import (
	shapegate "github.com/qiangli/yoke/pkg/shape/gate"
	"github.com/qiangli/yoke/pkg/shape/state"
)

// The installed CLI activates through the production gate: the same
// sealed-evidence check the web console passes an approval through. It is
// registered once here, in the package that owns the src command tree, so
// any binary linking the src CLI (bashy embeds yoke) gets it without
// calling back into the console.
func init() {
	RegisterActivationGate(shapegate.New("", ""))
	gateWiredAtInit = true
}

// gateWiredAtInit marks the production registration above; a later
// RegisterActivationGate (a test or an embedder) is an explicit override.
var gateWiredAtInit bool

// overrideGate is the explicitly wired gate, or nil when only the
// production default is wired: verbs that can mint their own evidence
// (adopt) then verify and seal it themselves.
func overrideGate() state.ActivationGate {
	if gateOverridden {
		return cliGate
	}
	return nil
}

// effectiveGate is the gate the CLI verbs consult: the wired gate, or the
// production gate when nothing is wired. Tests reset the wiring to nil on
// their way out; the fallback keeps the CLI evidence-bound regardless.
func effectiveGate() state.ActivationGate {
	if cliGate != nil {
		return cliGate
	}
	return shapegate.New("", "")
}
