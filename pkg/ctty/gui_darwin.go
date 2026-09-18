//go:build darwin

package ctty

import (
	"os/exec"
	"strings"
)

// guiAttended reports whether this process belongs to a GUI session the invoking
// human can actually see.
//
// `launchctl managername` names the launchd domain we are in: "Aqua" for a
// process in a logged-in graphical session, "Background" or "StandardIO" for one
// that is not — which is exactly what an SSH login gets.
//
// This is asked instead of sniffing SSH_CONNECTION because it answers the real
// question. The failure it prevents is specific and silent: over SSH to a Mac,
// osascript happily renders the dialog on the REMOTE machine's physical screen and
// returns success. Nobody is sitting there. Without this check the caller waits
// out its full timeout on a prompt that was displayed to an empty room, and the
// operator at the other end of the SSH session sees nothing at all.
//
// Verified: returns "Aqua" from a setsid'd, tty-less child of an agentic CLI on a
// logged-in desktop (which is the case that must keep working).
//
// It is NOT the only gate, and relying on it alone was a bug. A Mac with somebody
// logged in at the console can also be reached over SSH, and a process started
// that way can still find itself in an Aqua domain — at which point launchd's
// answer is technically true and operationally wrong: the desktop it is naming
// belongs to whoever is sitting in front of that machine, not to the person who
// typed the command from somewhere else. So an SSH session disqualifies the GUI
// rung independently, before launchd is even consulted.
func guiAttended() bool {
	if isRemoteSession() {
		// Reached from elsewhere: whatever desktop exists here is not the caller's.
		return false
	}
	out, err := exec.Command("launchctl", "managername").Output()
	if err != nil {
		// Fail closed. An unknown session is treated as unattended, so we fall
		// through to the rendezvous — always recoverable — rather than gambling on
		// a dialog nobody may see.
		return false
	}
	return strings.EqualFold(strings.TrimSpace(string(out)), "Aqua")
}

// guiCandidates lists helpers in preference order.
//
// osascript is first because it is part of macOS: no install, no dependency, and
// it cannot be missing. pinentry-mac is a fine helper but is only present if the
// operator installed gpg tooling, so it is a fallback rather than the default —
// and an operator who prefers it can say so with BASHY_ASKPASS.
func guiCandidates() []helper {
	return []helper{
		{path: "osascript", kind: kindOsascript},
		{path: "pinentry-mac", kind: kindPinentry},
		{path: "pinentry", kind: kindPinentry},
	}
}
