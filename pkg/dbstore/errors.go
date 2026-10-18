package dbstore

import (
	"context"
	"errors"
	"fmt"
)

// codeError is a sentinel carrying a contract error code (§5.2). Adapters wrap
// it with %w; Code recovers the code for whichever envelope reports it.
type codeError string

func (e codeError) Error() string { return string(e) }

var (
	ErrNotFound      error = codeError("not_found")
	ErrExists        error = codeError("exists")
	ErrInvalidInput  error = codeError("invalid_input")
	ErrUnsupported   error = codeError("unsupported")
	ErrReadOnly      error = codeError("read_only")
	ErrBusy          error = codeError("busy")
	ErrConflict      error = codeError("conflict")
	ErrTimeout       error = codeError("timeout")
	ErrCancelled     error = codeError("cancelled")
	ErrLifetimeEnded error = codeError("lifetime_ended")
	ErrIntegrity     error = codeError("integrity")
	ErrBackend       error = codeError("backend_error")
)

var codes = []error{ErrNotFound, ErrExists, ErrInvalidInput, ErrUnsupported, ErrReadOnly,
	ErrBusy, ErrConflict, ErrTimeout, ErrCancelled, ErrLifetimeEnded, ErrIntegrity, ErrBackend}

// Code returns the contract error code for err ("" for nil). Context errors
// map to timeout/cancelled; anything unclassified is backend_error.
func Code(err error) string {
	if err == nil {
		return ""
	}
	for _, c := range codes {
		if errors.Is(err, c) {
			return c.Error()
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout.Error()
	}
	if errors.Is(err, context.Canceled) {
		return ErrCancelled.Error()
	}
	return ErrBackend.Error()
}

// FromContext classifies a context error, keeping the cause.
func FromContext(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %v", ErrCancelled, err)
	}
	return err
}

// Backend wraps an unclassified engine error, preserving its message.
func Backend(engine string, err error) error {
	if err == nil {
		return nil
	}
	if Code(err) != ErrBackend.Error() {
		return err
	}
	return fmt.Errorf("%w: %s: %w", ErrBackend, engine, err)
}
