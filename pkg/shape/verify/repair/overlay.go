package repair

import (
	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/unit"
)

// EngineVersion versions the overlay verifier's own semantics. It is part
// of every overlay evidence binding, so a record minted by an older engine
// goes stale rather than being honoured by a verifier that would now judge
// differently.
const EngineVersion = "bashsharp-verify/1"

// OverlayBinding recomputes the evidence binding a promoted overlay must
// satisfy: the overlay body's identity as candidate, the authored body's
// identity as base, the frozen contract's capability key, this engine, and
// the coordinate as principal. The overlay body must keep the original
// contract — the capability comparison refuses a repair that weakened it,
// whatever its decorator lines look like.
func OverlayBinding(originalBody, overlayBody, principal string) (evidence.Binding, error) {
	orig, err := unit.ReadContract(originalBody)
	if err != nil || !orig.Contracted() {
		return evidence.Binding{}, errOverlayContract("the authored unit states no verifiable contract")
	}
	key, err := unit.CapabilityKey(orig)
	if err != nil {
		return evidence.Binding{}, errOverlayContract("the authored unit has no capability key")
	}
	cand, err := unit.ReadContract(overlayBody)
	if err != nil {
		return evidence.Binding{}, errOverlayContract("the overlay does not parse: " + err.Error())
	}
	ckey, cerr := unit.CapabilityKey(cand)
	if cerr != nil || ckey != key {
		return evidence.Binding{}, errOverlayContract("the overlay keeps a different contract than the authored unit")
	}
	return evidence.Binding{
		Candidate: unit.BodyIdentity([]byte(overlayBody)),
		Base:      unit.BodyIdentity([]byte(originalBody)),
		Verifier:  EngineVersion,
		Contract:  key,
		Principal: principal,
	}, nil
}

// overlayError is a binding refusal: the overlay cannot be evidence-bound.
type overlayError struct{ msg string }

func (e overlayError) Error() string { return e.msg }

func errOverlayContract(msg string) error { return overlayError{msg} }

// SealOverlay records the verified pass for overlayBody, bound to its
// identity. Only a candidate VerifyRepair passed leads here; the seal makes
// that pass durable across runs. It is the repair arm's minting half of the
// evidence pin: the verifier (tree verifier and repair arm) is the only
// writer of these records.
func SealOverlay(home, originalBody, overlayBody, principal string) error {
	binding, err := OverlayBinding(originalBody, overlayBody, principal)
	if err != nil {
		return err
	}
	_, err = evidence.Write(home, evidence.Record{Binding: binding, Verdict: evidence.VerdictPass})
	return err
}

// RequireOverlay returns nil only when a passing evidence record binds
// overlayBody to originalBody at principal under this engine. A stale,
// missing or failed record is an error: the caller runs the authored body,
// never the overlay.
func RequireOverlay(home, originalBody, overlayBody, principal string) error {
	binding, err := OverlayBinding(originalBody, overlayBody, principal)
	if err != nil {
		return err
	}
	_, err = evidence.Require(home, binding)
	return err
}
