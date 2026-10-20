package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Result/error convention: every Adapter.Do in this module reports a
// normalized failure in Result.Error AND returns that same *Error as the Go
// error, so callers may use either `if err != nil` (with errors.As to
// *Error) or inspect the Result. A nil error always means Result.Error is nil.

// Failure builds the (Result, error) pair for a normalized failure.
func Failure(op Op, code ErrorCode, format string, args ...any) (Result, error) {
	e := &Error{Code: code, Message: fmt.Sprintf(format, args...)}
	return Result{ProtocolVersion: ProtocolVersion, Op: op, Error: e}, e
}

// Err returns r.Error as an error, or nil.
func (r Result) Err() error {
	if r.Error == nil {
		return nil
	}
	return r.Error
}

// knownOps is every Op of protocol v1, in declaration order.
var knownOps = []Op{
	OpValidate, OpCapacity, OpPlan, OpCreate, OpInspect, OpList,
	OpStart, OpStop, OpDestroy, OpReboot, OpResize,
}

// Ops returns every operation of protocol v1.
func Ops() []Op { return slices.Clone(knownOps) }

// Known reports whether op is an operation of protocol v1.
func (op Op) Known() bool { return slices.Contains(knownOps, op) }

// Mutating reports whether op changes provider state and therefore needs an
// idempotency key.
func (op Op) Mutating() bool {
	switch op {
	case OpCreate, OpStart, OpStop, OpDestroy, OpReboot, OpResize:
		return true
	}
	return false
}

// Supports reports whether the manifest declares op as a capability.
func (m Manifest) Supports(op Op) bool {
	return slices.Contains(m.Capabilities, Capability(op))
}

// ValidateManifest checks a manifest's identity, protocol version, form and
// capability set.
func ValidateManifest(m Manifest) error {
	bad := func(format string, args ...any) error {
		return &Error{Code: ErrInvalidRequest, Message: "manifest " + m.Name + ": " + fmt.Sprintf(format, args...)}
	}
	if m.Name == "" {
		return bad("name is required")
	}
	if m.Kind != KindInfra {
		return bad("kind %q, want %q", m.Kind, KindInfra)
	}
	if m.ProtocolVersion != ProtocolVersion {
		return &Error{Code: ErrProtocolMismatch, Message: fmt.Sprintf("manifest %s: protocol %q, this host speaks %q", m.Name, m.ProtocolVersion, ProtocolVersion)}
	}
	switch m.Form {
	case FormExecutable:
		if len(m.Argv) == 0 || m.Argv[0] == "" {
			return bad("executable form needs argv")
		}
	case FormDeclarative:
		if len(m.Declarative) == 0 {
			return bad("declarative form needs a declarative document")
		}
	case FormBuiltin:
	default:
		return bad("unknown form %q", m.Form)
	}
	for _, c := range m.Capabilities {
		if !Op(c).Known() && c != CapVolumes && c != CapSpot {
			return bad("unknown capability %q", c)
		}
	}
	return nil
}

// CheckRequest validates req against manifest m without calling any
// provider: protocol version, a declared capability for req.Op (an
// undeclared op is unsupported_capability and is never substituted), an
// idempotency key on mutating ops, a resource ID where one is needed, and
// the static half of the destroy authorization (see CheckDestroyAuthorization).
func CheckRequest(m Manifest, req Request) *Error {
	if req.ProtocolVersion != "" && req.ProtocolVersion != ProtocolVersion {
		return &Error{Code: ErrProtocolMismatch, Message: fmt.Sprintf("request protocol %q, this host speaks %q", req.ProtocolVersion, ProtocolVersion)}
	}
	if !req.Op.Known() || !m.Supports(req.Op) {
		return &Error{Code: ErrUnsupportedCapability, Message: fmt.Sprintf("provider %s does not declare operation %q", m.Name, req.Op)}
	}
	if req.Op.Mutating() && req.IdempotencyKey == "" {
		return &Error{Code: ErrInvalidRequest, Message: fmt.Sprintf("%s needs an idempotency key", req.Op)}
	}
	switch req.Op {
	case OpInspect, OpStart, OpStop, OpDestroy, OpReboot, OpResize:
		if req.ResourceID == "" {
			return &Error{Code: ErrInvalidRequest, Message: fmt.Sprintf("%s needs a resource id", req.Op)}
		}
	}
	switch req.Op {
	case OpCreate, OpPlan:
		if req.Spec == nil {
			return &Error{Code: ErrInvalidRequest, Message: fmt.Sprintf("%s needs a spec", req.Op)}
		}
		if a := req.Authorization; a != nil && a.Kind != AuthTemporary {
			return &Error{Code: ErrInvalidRequest, Message: fmt.Sprintf("%s accepts only a temporary authorization, got %q", req.Op, a.Kind)}
		}
		if a := req.Authorization; a != nil && a.ExpiresAt.IsZero() && a.WorkRef == "" {
			return &Error{Code: ErrInvalidRequest, Message: "temporary authorization needs expires_at or work_ref"}
		}
	case OpResize:
		if req.Spec == nil || req.Spec.Size == "" {
			return &Error{Code: ErrInvalidRequest, Message: "resize needs spec.size"}
		}
	case OpDestroy:
		return CheckDestroyAuthorization(req)
	}
	return nil
}

// CheckDestroyAuthorization is the static half of A6, decidable before any
// provider call: a destroy must carry either a user confirmation bound to
// exactly this resource ID and this idempotency key, or a temporary
// authorization. A confirmation is single use by construction — it names
// one idempotency key, and a replay under that key is the same operation.
// A temporary authorization still has to match the resource's recorded
// tags; see AuthorizeDestroy.
func CheckDestroyAuthorization(req Request) *Error {
	a := req.Authorization
	refuse := func(why string) *Error {
		return &Error{Code: ErrConfirmationRequired, Message: fmt.Sprintf("destroy of %s refused: %s; the resource is untouched and keeps its ongoing cost", req.ResourceID, why)}
	}
	if a == nil {
		return refuse("no authorization")
	}
	switch a.Kind {
	case AuthConfirmation:
		if a.ResourceID == "" || a.ResourceID != req.ResourceID {
			return refuse(fmt.Sprintf("confirmation is bound to resource %q", a.ResourceID))
		}
		if a.IdempotencyKey == "" || a.IdempotencyKey != req.IdempotencyKey {
			return refuse("confirmation is bound to a different idempotency key")
		}
		return nil
	case AuthTemporary:
		return nil
	}
	return refuse(fmt.Sprintf("unknown authorization kind %q", a.Kind))
}

// AuthorizeDestroy is the full A6 check against the resource as the
// provider reports it: the static check, and for a temporary authorization
// the resource must have been created as temporary (TagTemporary) and, when
// the authorization names a work ref, carry that work tag.
func AuthorizeDestroy(req Request, res Resource) *Error {
	if e := CheckDestroyAuthorization(req); e != nil {
		return e
	}
	a := req.Authorization
	if a.Kind != AuthTemporary {
		return nil
	}
	if !slices.Contains(res.Tags, TagTemporary) {
		return &Error{Code: ErrConfirmationRequired, Message: fmt.Sprintf("destroy of %s refused: temporary authorization on a resource not created as temporary; it keeps its ongoing cost", res.ID)}
	}
	if a.WorkRef != "" && !slices.Contains(res.Tags, WorkTag(a.WorkRef)) {
		return &Error{Code: ErrConfirmationRequired, Message: fmt.Sprintf("destroy of %s refused: temporary authorization names work %q the resource was not created for", res.ID, a.WorkRef)}
	}
	return nil
}

// RefusedDestroyCost is the cost report attached to a refused destroy: the
// resource was not touched, so whatever it cost before it still costs.
func RefusedDestroyCost() *CostReport {
	return &CostReport{Ongoing: true, Note: "not destroyed; the resource and anything attached to it keep billing"}
}

// Guard wraps an adapter so every request is checked by CheckRequest
// before the adapter runs. A refused request never reaches the adapter.
// Guard also refuses a result whose op differs from the request's, so an
// adapter cannot substitute one operation for another.
func Guard(a Adapter) Adapter { return guard{a} }

type guard struct{ inner Adapter }

func (g guard) Manifest() Manifest { return g.inner.Manifest() }

func (g guard) Do(ctx context.Context, req Request) (Result, error) {
	if e := CheckRequest(g.inner.Manifest(), req); e != nil {
		r := Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Error: e}
		if e.Code == ErrConfirmationRequired {
			r.Cost = RefusedDestroyCost()
		}
		return r, e
	}
	if req.ProtocolVersion == "" {
		req.ProtocolVersion = ProtocolVersion
	}
	r, err := g.inner.Do(ctx, req)
	if r.Op != req.Op && r.Error == nil {
		return Failure(req.Op, ErrProtocolMismatch, "adapter answered %q to a %q request", r.Op, req.Op)
	}
	if r.ProtocolVersion == "" {
		r.ProtocolVersion = ProtocolVersion
	}
	if err == nil && r.Error != nil {
		err = r.Error
	}
	if err != nil && r.Error == nil {
		var e *Error
		if !errors.As(err, &e) {
			e = &Error{Code: ErrProvider, Message: err.Error()}
		}
		r = Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Error: e}
		err = e
	}
	return r, err
}
