package ctty

import "testing"

// The remote-execution matrix.
//
// These are the cases that motivated the whole attendedness test, and the reason
// the logic is a pure function: a developer on a Mac, or CI on a headless Linux
// box, would otherwise never exercise them — and the failure they prevent is
// silent. Getting the SSH rows wrong means a password dialog appears on a screen
// in another building while the operator's terminal shows nothing and blocks until
// it times out.
//
// Note the asymmetry, which is the point: SSH alone is NOT disqualifying. X11
// forwarding genuinely reaches the human, and refusing it would push every
// forwarded session onto the rendezvous for no reason.
func TestX11AttendedAcrossSessions(t *testing.T) {
	cases := []struct {
		name    string
		display string
		wayland string
		remote  bool
		want    bool
	}{
		{
			name:    "local X session is attended",
			display: ":0",
			want:    true,
		},
		{
			name:    "local X session with a screen suffix is attended",
			display: ":0.0",
			want:    true,
		},
		{
			name:    "local wayland session is attended",
			wayland: "wayland-0",
			want:    true,
		},
		{
			name:   "no display at all is not attended",
			remote: false,
			want:   false,
		},
		{
			// THE case. Over SSH the remote box's own console is visible in the
			// environment, but nobody is sitting at it.
			name:    "ssh without forwarding, remote console display, is NOT attended",
			display: ":0",
			remote:  true,
			want:    false,
		},
		{
			name:    "ssh with X11 forwarding IS attended — it reaches the client",
			display: "localhost:10.0",
			remote:  true,
			want:    true,
		},
		{
			name:    "ssh with X11UseLocalhost=no forwarding is attended",
			display: "buildhost:10.0",
			remote:  true,
			want:    true,
		},
		{
			// Wayland has no forwarding, so this can only be the remote compositor.
			name:    "ssh with only a wayland display is NOT attended",
			wayland: "wayland-0",
			remote:  true,
			want:    false,
		},
		{
			name:   "ssh with nothing set is not attended",
			remote: true,
			want:   false,
		},
		{
			name:    "a blank display string is not a display",
			display: "   ",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := x11Attended(tc.display, tc.wayland, tc.remote); got != tc.want {
				t.Errorf("x11Attended(display=%q, wayland=%q, remote=%v) = %v, want %v",
					tc.display, tc.wayland, tc.remote, got, tc.want)
			}
		})
	}
}

func TestIsForwardedDisplay(t *testing.T) {
	forwarded := []string{"localhost:10.0", "localhost:10", "127.0.0.1:10.0", "host.example:11.0"}
	console := []string{":0", ":0.0", ":1", "", "not-a-display"}

	for _, d := range forwarded {
		if !isForwardedDisplay(d) {
			t.Errorf("%q should be recognised as forwarded — rejecting it strands X11 users on the rendezvous", d)
		}
	}
	for _, d := range console {
		if isForwardedDisplay(d) {
			t.Errorf("%q must NOT be treated as forwarded — accepting it shows a dialog on an unattended screen", d)
		}
	}
}

// isRemoteSession must react to any of sshd's markers; a session that sets only
// one of them is still remote.
func TestIsRemoteSession(t *testing.T) {
	for _, k := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		t.Run(k, func(t *testing.T) {
			for _, other := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
				t.Setenv(other, "")
			}
			t.Setenv(k, "10.0.0.1 5555 10.0.0.2 22")
			if !isRemoteSession() {
				t.Errorf("%s set should mark the session remote", k)
			}
		})
	}
	t.Run("none set is local", func(t *testing.T) {
		for _, k := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
			t.Setenv(k, "")
		}
		if isRemoteSession() {
			t.Error("no ssh markers should mean a local session")
		}
	})
}
