//go:build darwin

package ctty

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// The two world observations guiAttended makes on macOS, as variables so tests
// can inject a session shape (a headless box, a logged-in desktop) without
// owning one. Production always reads the real world through them.
var (
	// launchctlManagerName names the launchd domain this process runs in.
	// Empty when launchctl cannot answer; every consumer fails closed on that.
	launchctlManagerName = func() string {
		out, err := exec.Command("launchctl", "managername").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	// consoleDeviceOwnerUID is the uid owning /dev/console. macOS chowns the
	// console device to the user who logs in at the machine and back to root at
	// the login window, so it is positive evidence of a console user session —
	// the one screen a dialog can actually reach. Root (0) means nobody is
	// logged in; -1 means it could not be read, which fails closed.
	consoleDeviceOwnerUID = func() int {
		fi, err := os.Stat(consoleDevice)
		if err != nil {
			return -1
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return -1
		}
		return int(st.Uid)
	}
)

// consoleDevice is the device whose ownership witnesses a console login.
const consoleDevice = "/dev/console"

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
// It is NOT the only gate, and relying on it alone was a bug twice over.
//
//   - A Mac with somebody logged in at the console can also be reached over SSH,
//     and a process started that way can still find itself in an Aqua domain —
//     at which point launchd's answer is technically true and operationally
//     wrong: the desktop it is naming belongs to whoever is sitting in front of
//     that machine, not to the person who typed the command from somewhere else.
//     So an SSH session disqualifies the GUI rung independently, before launchd
//     is even consulted.
//
//   - Aqua is also not proof the GUI is REACHABLE from here. A managed,
//     launchd-detached process can carry an Aqua managername from a desktop
//     session that no longer has anybody logged into it, or whose window server
//     it has no access to; on such a box osascript fails the whole dialog with
//     error -128 ("User canceled") within seconds and without any operator,
//     which the helper layer used to read as a cancel. The console device is
//     the second, independent witness: root owns /dev/console exactly when
//     nobody is logged in at the machine, and that shape must fail closed to
//     the rendezvous instead of gambling on a dialog nobody can see.
func guiAttended() bool {
	if isRemoteSession() {
		// Reached from elsewhere: whatever desktop exists here is not the caller's.
		return false
	}
	if !strings.EqualFold(launchctlManagerName(), "Aqua") {
		return false
	}
	// Positive evidence that somebody is logged in at the console. Root owns
	// the device at the login window; an unreadable one is not evidence at all.
	return consoleDeviceOwnerUID() > 0
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
