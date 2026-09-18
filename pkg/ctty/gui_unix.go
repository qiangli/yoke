//go:build !darwin && !windows

package ctty

import "os"

// guiAttended reports whether a GUI session exists that the invoking human can
// see.
//
// The subtlety is SSH, and it cuts BOTH ways — which is why this is not simply
// "reject when SSH_CONNECTION is set":
//
//   - SSH with X11 forwarding sets DISPLAY to a FORWARDED display
//     ("localhost:10.0"), which tunnels back to the client's screen. A dialog there
//     genuinely reaches the person who typed the command, so this must be ACCEPTED.
//
//   - SSH without forwarding leaves the remote machine's own DISPLAY (":0") visible
//     in the environment of a login shell started on the console, or inherited by a
//     daemon. A dialog there appears on a screen in another building. This must be
//     REJECTED.
//
// The two are told apart by the display's host part: a local console display has
// none (":0", ":0.0"), a forwarded one always does ("localhost:10.0"). That is a
// property of how X addresses displays, not a heuristic about hostnames.
//
// Wayland has no forwarding equivalent, so WAYLAND_DISPLAY under SSH can only mean
// the remote compositor and is rejected.
func guiAttended() bool {
	return x11Attended(os.Getenv("DISPLAY"), os.Getenv("WAYLAND_DISPLAY"), isRemoteSession())
}

// guiCandidates lists helpers in preference order.
//
// The desktop dialog tools come first because they need no configuration. pinentry
// is last but is the most capable when present — it is what gpg drives, and it
// knows how to grab the keyboard so the value cannot be captured by another window.
func guiCandidates() []helper {
	return []helper{
		{path: "zenity", kind: kindZenity},
		{path: "kdialog", kind: kindKDialog},
		{path: "pinentry-gnome3", kind: kindPinentry},
		{path: "pinentry-gtk-2", kind: kindPinentry},
		{path: "pinentry-qt", kind: kindPinentry},
		{path: "pinentry-x11", kind: kindPinentry},
		{path: "pinentry", kind: kindPinentry},
	}
}
