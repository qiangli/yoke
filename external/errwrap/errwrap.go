// Package errwrap wraps errors with additional context and walks
// chains of wrapped errors.
//
// This is a yoke clean-room implementation written for S488.3c (remove
// the MPL-2.0 hashicorp/errwrap dependency from the engine build).
// Nothing in the yoke engine graph imports errwrap directly — it was
// only reached through go-multierror, which this repository now
// replaces with external/multierror. The module exists so the
// hashicorp/errwrap module path resolves to permissive code for
// consumers that pin it alongside the multierror replacement. The
// surface is the small wrapping/walking core, designed from the
// standard library error-wrapping conventions (fmt.Errorf with %w,
// errors.Is/As traversal); the replaced module's source was never
// read.
package errwrap

import (
	"errors"
	"fmt"
)

// wrapped is an error with an additional message and the error it
// wraps. It implements the standard Unwrap convention so errors.Is
// and errors.As traverse the chain.
type wrapped struct {
	msg   string
	cause error
}

func (w *wrapped) Error() string { return w.msg + ": " + w.cause.Error() }
func (w *wrapped) Unwrap() error { return w.cause }

// Wrap returns an error that adds context to cause, formatted as
// "<msg>: <cause>". A nil cause returns nil.
func Wrap(cause error, msg string) error {
	if cause == nil {
		return nil
	}
	return &wrapped{msg: msg, cause: cause}
}

// Wrapf is Wrap with fmt.Sprintf formatting for the message.
func Wrapf(cause error, format string, args ...any) error {
	if cause == nil {
		return nil
	}
	return &wrapped{msg: fmt.Sprintf(format, args...), cause: cause}
}

// Walk descends err through its Unwrap chain (both the single-error
// and []error forms), calling fn for err itself and every wrapped
// error beneath it. It stops descending a branch once fn returns
// false, and returns the first error fn itself returns, if any.
func Walk(err error, fn func(error) error) error {
	if err == nil || fn == nil {
		return nil
	}
	if ferr := fn(err); ferr != nil {
		return ferr
	}
	switch cause := err.(type) {
	case interface{ Unwrap() error }:
		return Walk(cause.Unwrap(), fn)
	case interface{ Unwrap() []error }:
		for _, e := range cause.Unwrap() {
			if ferr := Walk(e, fn); ferr != nil {
				return ferr
			}
		}
	}
	return nil
}

// Get returns the first error in err's chain that is equal to target
// (per errors.Is), or nil when there is no match.
func Get(err, target error) error {
	var found error
	Walk(err, func(e error) error {
		if found == nil && target != nil && errors.Is(e, target) {
			found = e
		}
		return nil
	})
	return found
}

// GetAll returns every error in err's chain that is equal to target
// (per errors.Is).
func GetAll(err, target error) []error {
	var found []error
	Walk(err, func(e error) error {
		if target != nil && errors.Is(e, target) {
			found = append(found, e)
		}
		return nil
	})
	return found
}
