package cligw

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/gateway"
)

// NeedsLogin is one binding whose vendor subscription login is missing or
// expired (signed out, keychain unreachable, token expired). It is a human
// one-time setup state, not a transient upstream failure: the binding must
// not trip the breaker, must not be retried on another seat, and serves
// again after the operator signs in — no door restart.
type NeedsLogin struct {
	Agent string `json:"agent"`
	Tool  string `json:"tool"`
	// LoginCommand is the vendor's one-time sign-in command, derived from
	// the tool's auth_hint (or its declared login check).
	LoginCommand string `json:"login_command"`
	// Hint is the tool's declared auth_hint, verbatim, when present.
	Hint  string    `json:"hint,omitempty"`
	Since time.Time `json:"since"`
}

// NeedsLoginError carries a NeedsLogin verdict on a worker/backend error.
type NeedsLoginError struct {
	NeedsLogin NeedsLogin
	Err        error
}

func (e *NeedsLoginError) Error() string {
	verb := e.NeedsLogin.LoginCommand
	if verb == "" {
		verb = e.NeedsLogin.Tool
	}
	msg := fmt.Sprintf("cligw: agent %q (tool %s) needs-login: the vendor login is missing or expired; sign in once with `%s` and retry — no restart needed",
		e.NeedsLogin.Agent, e.NeedsLogin.Tool, verb)
	if hint := strings.TrimSpace(e.NeedsLogin.Hint); hint != "" {
		msg += " (" + hint + ")"
	}
	return msg
}

func (e *NeedsLoginError) Unwrap() error { return e.Err }

// NeedsLoginFailure extracts a carried needs-login verdict, if any.
func NeedsLoginFailure(err error) (NeedsLogin, bool) {
	var nl *NeedsLoginError
	if errors.As(err, &nl) {
		return nl.NeedsLogin, true
	}
	return NeedsLogin{}, false
}

// Failure renders the verdict as the gateway's stable classified failure:
// type needs_login, HTTP 401, never retryable. The hint names the human step.
func (n NeedsLogin) Failure() gateway.UpstreamFailure {
	return gateway.UpstreamFailure{
		Class:     gateway.UpstreamClass("needs_login"),
		Retryable: false,
		Status:    401,
		Hint: (&NeedsLoginError{NeedsLogin: n}).Error(),
	}
}

// needsLoginSignals are the exit/output patterns a signed-out vendor CLI
// leaves behind (measured spellings live in the fleet probe vocabulary:
// fleet's workerMissing/workerExpired). They only count for a tool that
// declares a subscription auth check — an api-key tool's 401 keeps the
// existing auth/billing fail-fast and is not a human sign-in step here.
var needsLoginSignals = []string{
	"not logged in", "logged out", "not signed in", "signed out",
	"please log in", "please login", "log in required", "login required",
	"sign-in required", "sign in required", "sign in to", "page to sign in",
	"waiting for approval", "not authenticated", "no subscription",
	`"loggedin": false`, `"loggedin":false`,
	"session expired", "token expired", "credentials expired",
	"login expired", "expired token", "invalid token",
	"reauthenticate", "re-authenticate", "reauth", "log in again",
	"login again", "unauthorized",
}

// classifyNeedsLogin recognises a vendor-login failure from a worker's
// output. The gate is the tool's declared auth check (auth.check_exec in
// the tool asset): only subscription tools can be needs-login. Within the
// gate, an explicit auth classification (401/403) or any measured
// signed-out pattern on stdout/stderr decides.
func classifyNeedsLogin(tool fleet.Tool, agent, output string, failure gateway.UpstreamFailure, classified bool) (NeedsLogin, bool) {
	if strings.TrimSpace(tool.CLI.Launch.Auth.CheckExec) == "" {
		return NeedsLogin{}, false
	}
	text := strings.ToLower(boundSignal(output))
	hit := classified && failure.Class == gateway.UpstreamAuth
	if !hit {
		for _, signal := range needsLoginSignals {
			if strings.Contains(text, signal) {
				hit = true
				break
			}
		}
	}
	if !hit {
		return NeedsLogin{}, false
	}
	return NeedsLogin{
		Agent:        agent,
		Tool:         tool.Name,
		LoginCommand: loginCommand(tool),
		Hint:         strings.TrimSpace(tool.CLI.Launch.AuthHint),
		Since:        time.Now(),
	}, true
}

// loginCommand derives the vendor's one-time sign-in command from the
// tool's declarations: the first back-quoted command in auth_hint that
// names the tool binary, else the declared login check (`muse login`),
// else the binary itself (interactive sign-in).
func loginCommand(tool fleet.Tool) string {
	bin := strings.TrimSpace(tool.CLI.Binary)
	if bin == "" {
		bin = tool.Name
	}
	hint := tool.CLI.Launch.AuthHint
	for {
		open := strings.IndexByte(hint, '`')
		if open < 0 {
			break
		}
		rest := hint[open+1:]
		end := strings.IndexByte(rest, '`')
		if end < 0 {
			break
		}
		candidate := strings.TrimSpace(rest[:end])
		hint = rest[end+1:]
		if candidate == bin || strings.HasPrefix(candidate, bin+" ") {
			return candidate
		}
	}
	if fields := strings.Fields(tool.CLI.Launch.Auth.CheckExec); len(fields) > 0 && fields[0] == "login" {
		return bin + " " + strings.Join(fields, " ")
	}
	return bin
}
