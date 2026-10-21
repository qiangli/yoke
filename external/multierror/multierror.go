// Package multierror aggregates a list of errors into a single error
// value that satisfies the error interface.
//
// This is a yoke clean-room implementation written for S488.3c (remove
// the MPL-2.0 hashicorp/go-multierror dependency from the engine
// build). It was written from the call sites and expectations of its
// permissive callers (podman pkg/errorhandling, pkg/bindings/images,
// pkg/machine/shim and containers/common retry/supplemented/libimage)
// without reading the replaced module's source. Only the public API
// those callers use is provided: the Error type with its Errors field,
// Append, ErrorOrNil, Len, and the Error and Unwrap methods, plus
// Group for the Linux buildah caller.
package multierror

import (
	"fmt"
	"strings"
)

// Error is an aggregate of zero or more errors. The zero value is not
// useful; grow one with Append, starting from a nil *Error.
type Error struct {
	// Errors holds the aggregated errors in append order. Callers may
	// read the slice (the containers/common retry package type-switches
	// on *Error and iterates it) but should extend it only via Append.
	Errors []error
}

// Append returns a new aggregate with errs appended to err, or err
// itself when err is already a non-empty *Error.
//
// A nil err (including a typed-nil *Error, the idiom used by the
// podman callers) starts a fresh aggregate. Nil elements in errs are
// skipped, and an element that is itself a *Error is flattened into
// its constituent errors. When the result would hold no errors,
// Append returns nil, so callers testing the result against nil —
// directly or through an error interface — see nil.
func Append(err error, errs ...error) *Error {
	var result *Error
	switch e := err.(type) {
	case *Error:
		result = e
	case nil:
	default:
		result = &Error{Errors: []error{err}}
	}
	if result == nil {
		result = &Error{}
	}
	for _, e := range errs {
		result.append(e)
	}
	if len(result.Errors) == 0 {
		return nil
	}
	return result
}

// append adds one error to the aggregate, skipping nil and flattening
// nested aggregates.
func (e *Error) append(err error) {
	switch inner := err.(type) {
	case nil:
		return
	case *Error:
		if inner == nil {
			return
		}
		for _, nested := range inner.Errors {
			e.append(nested)
		}
		return
	default:
		e.Errors = append(e.Errors, inner)
	}
}

// Error renders the aggregate as a header line naming the error count
// followed by one tab-indented bullet per error, ending with a blank
// line:
//
//	2 errors occurred:
//		* first failure
//		* second failure
//
// The podman errorhandling package documents this header-plus-list
// shape (and trims the trailing blank line itself).
func (e *Error) Error() string {
	if e == nil || len(e.Errors) == 0 {
		return "0 errors occurred"
	}
	points := make([]string, len(e.Errors))
	for i, err := range e.Errors {
		points[i] = fmt.Sprintf("\t* %s", err)
	}
	return fmt.Sprintf("%d errors occurred:\n%s\n\n",
		len(e.Errors), strings.Join(points, "\n"))
}

// Len reports how many errors the aggregate holds; 0 on a nil *Error.
func (e *Error) Len() int {
	if e == nil {
		return 0
	}
	return len(e.Errors)
}

// ErrorOrNil returns the aggregate as an error interface value, or nil
// when no errors were appended. It is safe to call on a nil *Error.
func (e *Error) ErrorOrNil() error {
	if e == nil || len(e.Errors) == 0 {
		return nil
	}
	return e
}

// Unwrap exposes the aggregated errors to errors.Is and errors.As,
// following the standard multi-error unwrapping convention.
func (e *Error) Unwrap() []error {
	if e == nil {
		return nil
	}
	return e.Errors
}
