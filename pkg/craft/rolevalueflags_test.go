// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package craft

import "testing"

// TestValueFlagsDoNotBecomeHosts guards a failure that is silent by
// construction and therefore very hard to notice.
//
// Extract skips an unknown flag rather than guessing at it — the right policy.
// But a flag that CONSUMES A VALUE and is not declared leaves its value behind
// as a bare word, and for a HostPositional command the first bare word becomes
// THE HOST. So `ssh -o BatchMode=yes host true` bound every fact of that
// invocation to a host named "batchmode=yes": a confident, well-formed,
// completely fabricated entity, recorded with no error anywhere.
//
// This was found by running the command, not by reading the table. The table
// looked complete.
func TestValueFlagsDoNotBecomeHosts(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"ssh -o", []string{"ssh", "-o", "BatchMode=yes", "remote.host", "true"}, "remote.host"},
		{"ssh -o twice", []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "remote.host"}, "remote.host"},
		{"ssh -F", []string{"ssh", "-F", "/tmp/cfg", "remote.host"}, "remote.host"},
		{"ssh -o with port", []string{"ssh", "-o", "StrictHostKeyChecking=no", "-p", "2222", "user@remote.host"}, "remote.host"},
		{"ssh -W", []string{"ssh", "-W", "other:22", "remote.host"}, "remote.host"},
		{"scp -o", []string{"scp", "-o", "BatchMode=yes", "f", "remote.host:/tmp"}, "remote.host"},
		{"scp -l limit is not a login", []string{"scp", "-l", "1000", "f", "remote.host:/tmp"}, "remote.host"},
		// sftp declares HostHasPath, so it needs the `host:path` form. A bare
		// word is deliberately NOT read as a host — that discriminator is what
		// stops a local filename being recorded as a machine.
		{"sftp -o", []string{"sftp", "-o", "BatchMode=yes", "remote.host:/tmp"}, "remote.host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, _ := Extract(tc.argv)
			if x.Entity.Name != tc.want {
				t.Errorf("Extract(%q) bound to host %q, want %q",
					tc.argv, x.Entity.Name, tc.want)
			}
		})
	}
}

// TestScpDashLIsNotALogin — scp's -l is a bandwidth limit while ssh's -l is the
// login. Reading one as the other would offer a wrong account on a later
// command, which is exactly the class of confident wrong answer the role table
// exists to prevent.
func TestScpDashLIsNotALogin(t *testing.T) {
	x, _ := Extract([]string{"scp", "-l", "1000", "f", "remote.host:/tmp"})
	if v, ok := x.Roles[RoleUser]; ok {
		t.Errorf("scp -l must not be read as a login, got user=%q", v)
	}
}
