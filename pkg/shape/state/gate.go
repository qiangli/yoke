// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package state

// EvidenceRef is the verifier's answer an activation records alongside the
// request: whether the candidate may go live and which evidence record the
// decision rests on.
type EvidenceRef struct {
	// Verdict is the gate's decision (for example "pass").
	Verdict string
	// EvidenceID identifies the evidence record behind the verdict.
	EvidenceID string
}

// ActivationGate decides whether a candidate revision may become a
// principal's active revision. The trusted verifier satisfies it; callers
// pass a fake in tests. A non-nil error refuses the activation before
// anything is recorded or switched.
type ActivationGate interface {
	Check(candidate, base, principal string) (EvidenceRef, error)
}

// GateFunc adapts a plain function to ActivationGate.
type GateFunc func(candidate, base, principal string) (EvidenceRef, error)

// Check runs the adapted function.
func (f GateFunc) Check(candidate, base, principal string) (EvidenceRef, error) {
	return f(candidate, base, principal)
}
