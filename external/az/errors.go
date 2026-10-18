// Typed errors for az (Sprint 457 story bedc320542eb).
//
// This package introduces NO new error ABI: results and failures travel in
// the existing shared native envelope loom-v2 (coreutils/pkg/weavecli, which
// owns the concrete field names, the single `schema_version` discriminator,
// and the exit mapping). What lives here is 457-owned service-specific
// detail — typed distinctions, safe operation/target context, provider code,
// and retryability — carried inside that envelope's message/help path, with
// process exits following the shared mapping (native unsupported input exits
// 2; an executed foreign command's status passes through).
package az

import (
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Code names a az failure class. The typed code is authoritative over the
// coarse process exit.
type Code string

const (
	CodeInvalidInput       Code = "invalid-input"
	CodeUnsupported        Code = "unsupported-capability"
	CodeMissingCredentials Code = "missing-credentials"
	CodeExpiredCredentials Code = "expired-credentials"
	CodePermissionDenied   Code = "permission-denied"
	CodeNotFound           Code = "target-not-found"
	CodeRateLimit          Code = "rate-limit"
	CodeCursorMismatch     Code = "cursor-mismatch"
	CodeTimeout            Code = "timeout"
	CodeCancelled          Code = "cancelled"
	CodeOutputLimit        Code = "output-limit"
	CodeProviderFailure    Code = "provider-failure"
	CodePartialMutation    Code = "partial-unknown-mutation"
)

// Error is a typed az failure: safe operation/target context, the provider
// code when one exists, retryability, and retry-after when available.
// Credential material must never appear in any field.
type Error struct {
	Code         Code
	Message      string
	Operation    string
	Target       string
	ProviderCode string
	Retryable    bool
	RetryAfter   string
}

func (e *Error) Error() string { return e.Message }

// NewError builds a typed error. Retryable defaults from the code.
func NewError(code Code, msg string, detail *Error) *Error {
	e := &Error{Code: code, Message: msg}
	if detail != nil {
		e.Operation = detail.Operation
		e.Target = detail.Target
		e.ProviderCode = detail.ProviderCode
		e.Retryable = detail.Retryable
		e.RetryAfter = detail.RetryAfter
	} else {
		e.Retryable = code == CodeRateLimit || code == CodeTimeout || code == CodeProviderFailure
	}
	return e
}

// WithOp returns the error with operation/target context attached.
func (e *Error) WithOp(operation, target string) *Error {
	e.Operation = operation
	e.Target = target
	return e
}

// Exit maps the typed code onto the shared native exit mapping.
func (e *Error) Exit() int {
	switch e.Code {
	case CodeInvalidInput, CodeUnsupported:
		return weavecli.ExitInvalidArg // 2: native unsupported/invalid input
	case CodeCancelled:
		return 130
	case CodeTimeout:
		return 124
	default:
		return weavecli.ExitGenericFail // 1: denied, missing/expired auth, provider, limits, partial
	}
}

// EmitError writes the failure as a loom-v2 error envelope to w (stdout
// stays machine-readable; diagnostics ride the envelope) and returns the exit.
func (e *Error) EmitError(w io.Writer, command string) int {
	return weavecli.EmitError(w, weavecli.OutputJSON, command, e.Exit(), e)
}

// splitTarget parses a "service:capability" target into service, capability,
// and op class parts. Unknown shapes yield empty strings, never an error.
func splitTarget(target string) (svc, cap string, op OpClass) {
	svc, cap, _ = strings.Cut(target, ":")
	if cap == "" {
		return "", "", ""
	}
	return svc, cap, OpClass(cap)
}

// Unsupported builds the refusal for an unadmitted combination: it names
// the deferred/unclaimed home when one exists and never executes.
func Unsupported(operation, target string) *Error {
	if home := Deferral(target); home != "" {
		return NewError(CodeUnsupported, fmt.Sprintf("az: %s %s not implemented here — %s", operation, target, home), nil).WithOp(operation, target)
	}
	svc, _, op := splitTarget(target)
	if svc != "" {
		if s, ok := LookupService(svc); ok && !s.Supports(op) {
			return NewError(CodeUnsupported, fmt.Sprintf("az: %s does not support %s for service %q", operation, op, svc), nil).WithOp(operation, target)
		}
	}
	return NewError(CodeUnsupported, fmt.Sprintf("az: unsupported combination %s %s — refusing without execution", operation, target), nil).WithOp(operation, target)
}
