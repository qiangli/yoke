// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"errors"
	"fmt"
)

// Error carries a stable exit code (one of the weavecli.Exit* constants)
// alongside a human-readable message, so the command layer can map any engine
// failure onto the right envelope code without string-matching.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// ExitCode reports the stable exit code for this error.
func (e *Error) ExitCode() int { return e.Code }

func errf(code int, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// ExitCodeOf extracts the stable exit code from err: a *dag.Error (or anything
// exposing ExitCode() int) yields its code, a nil error yields 0, and any other
// error yields 1 (generic failure). Callers (e.g. bashy's agentos Dispatch)
// pass the cobra Execute() error here to os.Exit with the agent-meaningful code.
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	type coder interface{ ExitCode() int }
	var c coder
	if errors.As(err, &c) {
		return c.ExitCode()
	}
	return 1
}
