// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package meet

import (
	"os"
	"strings"
)

// turnAuthority is how much a meet or chat turn is allowed to DO, in one place
// because every seat must be launched the same way.
//
// THIS CHANGED DELIBERATELY, AND IT GAVE SOMETHING UP. Meeting turns used to be
// read-only, on the argument that a meeting is a conversation: a seat produces
// text, the files it reviews are read into its prompt, and a reviewer that can
// edit what it reviews can "fix" the thing under debate and then argue from the
// fixed version. That argument is still true, and it is the cost of this
// setting — it is not being claimed as an improvement.
//
// The reason it is paid: these seats are no longer only discussing. A room's
// manager runs a sprint from that seat and a steward drives the machine from a
// chat, and both of those jobs ARE acting — filing a story, moving a branch,
// running a gate. A seat that can only speak has to ask a human to type every
// consequence of what it just decided, which makes the surface a suggestion box.
// Meet's own `Start work` already launched with exactly this authority
// (startRelayDMWork), so an operator could get it by clicking a different
// button; ordinary turns disagreeing with that was the inconsistency, not the
// authority itself.
//
// WHAT IT ACTUALLY GRANTS. ReadOnly false leaves the agent's write capability
// in place; AllowUnsafe keeps the CLI's approval-gate kill-switches (and, for a
// tool whose gate is a sandbox value rather than a flag, maps to its bypass
// mode) and skips the uncontained-host guard. That guard exists because an
// unattended agent with full access on a host nobody is watching is exactly
// what it says it is — so this is a risk acceptance, not a loophole, and the
// containment answer is still the right one where it is available: run the
// fleet inside a container (`bashy podman`) and nothing here has to be relaxed.
//
// HOW TO PUT IT BACK. $BASHY_MEET_READONLY=1 restores the old posture for every
// seat, host-wide. It is one variable rather than a per-verb flag because the
// property is about the HOST, not about which button was clicked: a machine
// where an unattended agent must not write is a machine where no seat may.
func turnAuthority() (readOnly, allowUnsafe bool) {
	if meetReadOnly() {
		// Read-only wins over AllowUnsafe everywhere downstream (see
		// agentlaunch.FinalizeArgs), but returning false here says so plainly
		// rather than relying on that precedence.
		return true, false
	}
	return false, true
}

// meetReadOnly reads the operator's host-wide restriction. Any of the usual
// affirmatives, because a setting that only accepts one spelling is a setting
// that silently does nothing.
func meetReadOnly() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ReadOnlyTurnsEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ReadOnlyTurnsEnv restores the read-only posture described on turnAuthority.
// Exported so a host can name it in its own documentation and error text
// without copying the string.
const ReadOnlyTurnsEnv = "BASHY_MEET_READONLY"
