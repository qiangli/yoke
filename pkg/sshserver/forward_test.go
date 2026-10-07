package sshserver

import "testing"

// TestAllowTCPIPForwardBind exercises the bind-address allowlist used by
// `tcpip-forward` (ssh -R). Loopback only; empty string ("") matches
// openssh's default-to-127.0.0.1 behavior.
func TestAllowTCPIPForwardBind(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"localhost", true},
		{"LOCALHOST", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"  127.0.0.1  ", true},
		{"0.0.0.0", false},
		{"192.168.1.10", false},
		{"example.com", false},
	}
	for _, tc := range cases {
		if got := allowTCPIPForwardBind(tc.in); got != tc.want {
			t.Errorf("allowTCPIPForwardBind(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
