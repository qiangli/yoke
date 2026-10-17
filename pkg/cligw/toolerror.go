package cligw

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/llmgw/gateway"
)

// ToolError is a CLI worker failure carrying the provider classification of
// the tool's stderr. The message stays structural (agent plus exit status):
// stderr can echo prompts, replies or credentials, so it never reaches HTTP
// headers or logs. Serve unwraps the Failure and answers classified failures
// (401 auth, 402 billing) without spending the failover budget.
type ToolError struct {
	err error
	// Failure is the shared upstream classification of the bounded stderr
	// signal. It is the same verdict the door would have reached had the
	// provider answered it directly.
	Failure gateway.UpstreamFailure
}

func (e *ToolError) Error() string { return e.err.Error() }
func (e *ToolError) Unwrap() error { return e.err }

// wrapToolError classifies the tool's stderr with the shared upstream
// classifier and attaches the verdict to the structural worker error. Empty
// stderr carries no signal and leaves the error untouched.
func wrapToolError(err error, stderr string) error {
	failure, ok := classifyToolStderr(stderr)
	if !ok {
		return err
	}
	return &ToolError{err: err, Failure: failure}
}

// toolFailure extracts a carried tool classification from a worker error.
func toolFailure(err error) (gateway.UpstreamFailure, bool) {
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		return toolErr.Failure, true
	}
	return gateway.UpstreamFailure{}, false
}

// toolStatusPattern finds an HTTP status a CLI reports next to its provider
// body ("status": 401, status_code=429, HTTP 401, error 429). A bare number
// is never a status: "exit status 1" and 4-digit provider codes (1113) must
// not match, so only a keyword-adjacent 3-digit code counts.
var toolStatusPattern = regexp.MustCompile(`(?i)(?:status(?:_code|code)?|http(?:/1\.[01])?|error|code)\W{0,10}(\d{3})(?:\D|$)`)

// classifyToolStderr runs the tool's bounded stderr through the shared
// upstream classifier. An explicit provider status in the text decides with
// the status; otherwise the body signals (invalid key, exhausted balance)
// decide with status 0, exactly as for a direct upstream response.
func classifyToolStderr(stderr string) (gateway.UpstreamFailure, bool) {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return gateway.UpstreamFailure{}, false
	}
	signal := boundSignal(stderr)
	status := 0
	if m := toolStatusPattern.FindStringSubmatch(signal); m != nil {
		if code, err := strconv.Atoi(m[1]); err == nil && actionableToolStatus(code) {
			status = code
		}
	}
	return gateway.ClassifyUpstream(status, []byte(signal), nil), true
}

// actionableToolStatus reports whether code is a status the shared classifier
// answers with a stable class. Anything else keeps status 0 so the body
// signals alone decide.
func actionableToolStatus(code int) bool {
	switch code {
	case 401, 403, 402, 408, 429:
		return true
	}
	return code >= 500 && code <= 599
}
