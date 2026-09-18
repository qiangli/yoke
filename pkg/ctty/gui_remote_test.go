//go:build !windows

package ctty

import "testing"

// An SSH session must never take the GUI rung, on any platform.
//
// This was a real bug and it is worth stating plainly, because the reasoning that
// produced it is seductive. On macOS `launchctl managername` reports the launchd
// domain, and over a plain SSH login it reports "Background" — so it LOOKS like a
// sufficient test on its own. It is not: a Mac with somebody logged in at the
// console can also be reached over SSH, and a process started that way can still
// resolve to an Aqua domain. launchd's answer is then technically true and
// operationally wrong — the desktop it names belongs to whoever is sitting in
// front of that machine, not to the person who typed the command from elsewhere.
//
// The consequence of getting this wrong is silent and bad: a password dialog
// appears on a screen in another building, `osascript` reports success, and the
// caller blocks until it times out while the operator's terminal shows nothing.
// So remoteness is checked FIRST and independently, and never inferred from a
// session-domain probe.
//
// On Linux the same gate exists but is narrower by design: X11 forwarding really
// does reach the client, so only an unforwarded display is rejected — see
// TestX11AttendedAcrossSessions.
func TestRemoteSessionNeverTakesTheGuiRung(t *testing.T) {
	for _, marker := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		t.Run(marker, func(t *testing.T) {
			for _, k := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
				t.Setenv(k, "")
			}
			t.Setenv(marker, "10.0.0.1 5555 10.0.0.2 22")

			// Give the unix path a display that WOULD be accepted locally, so the
			// test fails if the remote gate is skipped rather than passing because
			// there was nothing to accept.
			t.Setenv("DISPLAY", ":0")
			t.Setenv("WAYLAND_DISPLAY", "wayland-0")

			if guiAttended() {
				t.Error("a GUI dialog would be shown on the far machine's screen, " +
					"where nobody is sitting — the caller is at the other end of the SSH session")
			}
		})
	}
}
