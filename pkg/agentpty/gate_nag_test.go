package agentpty

import "testing"

// A vendor questionnaire is not an authorization decision, and it blocks an
// unattended run exactly as hard as one. An agy conductor 40 minutes into a
// campaign stopped dead on "How's the CLI experience so far? [0] Skip" — and the
// watchdog kill would have been reported as a model timeout. A wedged run blamed
// on the model, caused by a survey.
func TestClassifyGateSkipsAVendorNag(t *testing.T) {
	tail := "How's the CLI experience so far? Help us improve:\n [1] Good  [2] Fine  [3] Bad  [0] Skip"
	v := ClassifyGate(tail)
	if v.Kind != GateNag {
		t.Fatalf("ClassifyGate = %q, want %q — an unattended agent will sit on this forever", v.Kind, GateNag)
	}
	if GateNagClearPayload != "0" {
		t.Errorf("a nag is dismissed, never answered: %q", GateNagClearPayload)
	}
}

// A real trust prompt must NOT be mistaken for a nag — dismissing it with "0"
// would leave the agent blocked while we reported it handled.
func TestATrustPromptIsNotANag(t *testing.T) {
	v := ClassifyGate("Do you trust the contents of this folder?\n 1. Yes  2. No")
	if v.Kind != GateTrust {
		t.Fatalf("ClassifyGate = %q, want %q", v.Kind, GateTrust)
	}
}
