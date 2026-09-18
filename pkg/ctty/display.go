package ctty

import (
	"os"
	"strings"
)

// isRemoteSession reports whether we were reached over SSH.
//
// sshd sets SSH_CONNECTION and SSH_CLIENT for every session, and SSH_TTY when a
// terminal was allocated; any one of them is sufficient evidence.
func isRemoteSession() bool {
	for _, k := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

// isForwardedDisplay reports whether an X DISPLAY tunnels back to the client's
// screen rather than pointing at this machine's own console.
//
// This is the whole remote-execution question in one predicate, and it decides
// between two outcomes that are not symmetric: getting it wrong in one direction
// costs an unnecessary rendezvous, and in the other direction shows a password
// prompt on a screen in another building and then blocks until it times out.
//
// The rule is structural, not a hostname heuristic. An X display address is
// "[host]:number[.screen]". A local console display omits the host entirely and
// therefore begins with the colon (":0", ":0.0"). ssh -X always sets a host part
// ("localhost:10.0", or "<hostname>:10.0" under X11UseLocalhost=no), because the
// forwarded display is a TCP endpoint rather than the local socket.
//
// Lives here, untagged, so it is testable on every platform — the cases it guards
// are the ones a developer on a Mac would otherwise never exercise.
func isForwardedDisplay(d string) bool {
	d = strings.TrimSpace(d)
	i := strings.LastIndex(d, ":")
	// i < 0 → not a display address at all. i == 0 → ":0", the local console.
	return i > 0
}

// x11Attended decides whether an X/Wayland session is one the invoking human can
// see. Split out from guiAttended, and untagged, so the SSH matrix below is
// table-testable from any developer machine.
//
// The asymmetry is deliberate: locally, any display is believed; remotely, only a
// forwarded X display is. Wayland has no forwarding mechanism, so a WAYLAND_DISPLAY
// seen over SSH can only be the remote compositor and is rejected.
func x11Attended(display, wayland string, remote bool) bool {
	display = strings.TrimSpace(display)
	wayland = strings.TrimSpace(wayland)
	if remote {
		return display != "" && isForwardedDisplay(display)
	}
	return display != "" || wayland != ""
}
