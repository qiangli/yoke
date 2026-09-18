package herald

import (
	"context"

	"github.com/qiangli/yoke/pkg/gate"
)

// GateExtensionURI identifies herald's evidence extension.
//
// A2A extensions are URI-identified, declared in the card's
// capabilities.extensions[], and negotiated per request via the A2A-Extensions
// header. Declaring one is how you add meaning to A2A without forking it.
const GateExtensionURI = "https://dhnt.io/ext/gate/v1"

// Why this extension exists.
//
// A2A's terminal state TASK_STATE_COMPLETED is SELF-REPORTED. The peer decides
// it succeeded and says so; the protocol gives a caller no way to disagree.
// That is the same defect the fleet measured directly in its three-harness A/B:
// ALL THREE HARNESSES EXITED 0 WHEN THEY FAILED. A success state reached by the
// absence of evidence is not a success state.
//
// So a herald task carries a GATE — a command that decides. The peer's
// COMPLETED is a claim; the gate is the verdict.
//
// The design rule that keeps this interoperable: DEGRADATION IS THE FEATURE.
// A peer that has never heard of the extension is not refused and is not
// trusted. herald simply runs the gate itself, locally, against whatever the
// peer returned. The guarantee therefore holds against ANY conformant A2A
// peer, which is what makes this worth proposing upstream rather than a
// private dialect.

// GateOutcome is the verdict on one delegated task.
//
// An ALIAS, not a wrapper. The implementation moved to pkg/gate so pkg/acp can
// reach it without importing herald — that import was the only thing stopping
// herald from importing acp, and `herald acp` must construct an acp.Agent.
// ACP's end_turn is the same self-reported claim as A2A's COMPLETED, so both
// protocol packages want this primitive and neither should own it.
//
// Alias rather than a distinct type is load-bearing: gate.Outcome and
// herald.GateOutcome are the SAME type, so every existing value already
// flowing between acp and herald keeps working with no conversion.
type GateOutcome = gate.Outcome

// RunLocalGate executes the gate in dir and returns the verdict.
//
// herald's spelling of gate.RunLocal, kept because herald is where the gate
// DISCIPLINE is documented and where A2A callers look for it. The mechanism
// lives one level down so ACP can share it.
func RunLocalGate(ctx context.Context, dir, gateCmd string, peerClaimed string) GateOutcome {
	return gate.RunLocal(ctx, dir, gateCmd, peerClaimed)
}
