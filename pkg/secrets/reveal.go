package secrets

import (
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ErrRevealRefused is returned when a plaintext-reveal verb was called from a
// context that may never see secret values: a spawned agent, or anything
// without an interactive terminal on stdin. Its text never contains a value.
var ErrRevealRefused = errors.New("secret reveal refused")

// agentPrincipalPrefix is the URN prefix bashy's launcher stamps on every
// spawned agent session (agentlaunch.PrincipalEnv): a process carrying it is
// an agent acting for the owner, never the owner at a keyboard.
const agentPrincipalPrefix = "dhnt:agent/"

// agentMarkerEnv names the environment variables that mark a process as a
// bashy-launched agent session. Any one of them is conclusive.
var agentMarkerEnv = []string{
	"BASHY_PRINCIPAL", // dhnt:agent/<nick>
	"BASHY_AGENT_ID",  // the fleet nickname
	"BASHY_AGENT_BINDING",
}

// AgentCaller reports whether environ marks the process as a spawned agent
// session. Agents use secrets by HANDLE — a ref resolved at the exec boundary
// into a child's environment (secret exec, api_key_ref grants, dagrun
// Delivery) — and may never read a plaintext value directly.
func AgentCaller(environ []string) bool {
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case "BASHY_PRINCIPAL":
			if strings.HasPrefix(value, agentPrincipalPrefix) {
				return true
			}
		case "BASHY_AGENT_ID", "BASHY_AGENT_BINDING":
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

// interactiveStdin reports whether in is an interactive terminal. A buffer, a
// pipe or a detached stdin is by construction not a human at a keyboard.
func interactiveStdin(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// revealInteractive is the terminal probe the reveal verbs use. It is a
// package var — a SEAM FOR TESTS, deliberately not an environment variable:
// an agent subprocess can write $BASHY_* but cannot flip a Go var in this
// process, so the gate has no forgeable bypass.
var revealInteractive = interactiveStdin

// CheckReveal is the gate every plaintext-reveal verb (`secret show`,
// `secret env`) passes before touching the vault. It refuses two callers:
//
//   - an agent session (AgentCaller): reveal is owner-human-only, by design;
//     an agent that needs a credential to run something uses handle use
//     (`secret exec NAME -- CMD` into the child's environment, or
//     `secret exec --stdin NAME -- CMD` onto the child's stdin), where the
//     value is resolved at the exec boundary and never printed.
//   - anything without an interactive terminal on stdin: a piped or detached
//     caller is by construction not a human at a keyboard, and output to a
//     pipe is exactly the captured transcript surface the Redactor exists to
//     protect.
//
// interactive is supplied by the caller (the verb probes its own stdin), so
// the decision is testable without a pty and there is no environment
// override an agent could forge.
func CheckReveal(environ []string, interactive bool) error {
	if AgentCaller(environ) {
		return errors.New(ErrRevealRefused.Error() + ": agent callers cannot reveal secret values — use a handle (`secret exec NAME -- CMD` for the command's environment, `secret exec --stdin NAME -- CMD` to feed the command's stdin) or ask the operator at the terminal")
	}
	if !interactive {
		return errors.New(ErrRevealRefused.Error() + ": values print only at an interactive terminal — for commands use a handle (`secret exec NAME -- CMD` for its environment, `secret exec --stdin NAME -- CMD` for its stdin); the value stays out of argv and output")
	}
	return nil
}
