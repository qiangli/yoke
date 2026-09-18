//go:build windows

package ctty

import (
	"os"
	"strings"
)

// guiAttended reports whether we are in an interactive desktop session.
//
// Windows isolates services in session 0, which has no usable desktop: a dialog
// shown from there is drawn on a station nobody can see (and since Vista, is not
// drawn to the console user at all). SESSIONNAME distinguishes them — "Console"
// for the local desktop, "RDP-Tcp#N" for a remote-desktop session (which IS
// attended: the human is looking at it), "Services" for session 0.
//
// The unset case is treated as unattended. That is the conservative reading and it
// is the right one here: OpenSSH on Windows does not set SESSIONNAME, so an SSH
// login falls through to the rendezvous rather than popping a dialog on whatever
// desktop happens to be logged in at the console — the same remote-execution
// failure the darwin and unix paths guard against.
// An SSH session disqualifies the rung outright, for the same reason as on the
// other platforms: the caller is elsewhere, so a dialog drawn here reaches nobody.
// RDP is deliberately NOT excluded — it sets SESSIONNAME=RDP-Tcp#N and not the
// SSH markers, and a remote-desktop user genuinely is looking at that screen.
func guiAttended() bool {
	if isRemoteSession() {
		return false
	}
	s := strings.TrimSpace(os.Getenv("SESSIONNAME"))
	if s == "" {
		return false
	}
	return !strings.EqualFold(s, "Services")
}

// guiCandidates lists helpers in preference order. Windows PowerShell is present
// on every supported Windows; pwsh is the cross-platform successor and is
// preferred when installed.
func guiCandidates() []helper {
	return []helper{
		{path: "pwsh", kind: kindPowerShell},
		{path: "powershell", kind: kindPowerShell},
	}
}
