package weave

import "testing"

// TestClassifyReadyOutput covers the auth-readiness classifier that turns a
// tool's headless-probe output into ready / needs-login / stale-contract — the
// Phase-0 pre-flight that catches an installed-but-unauthenticated tool (agy
// "not signed in", codex/claude "do you trust this directory?") before it
// silently stalls a real fleet run.
func TestClassifyReadyOutput(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		timedOut bool
		want     string
	}{
		{"agy not signed in", "Welcome to the Antigravity CLI. You are currently not signed in.\nSigning in...", true, ReadyNeedsAuth},
		{"codex trust prompt", "Do you trust the contents of this directory?\n1. Yes, continue", true, ReadyNeedsAuth},
		{"claude trust", "Do you trust the files in this folder?", true, ReadyNeedsAuth},
		{"generic please log in", "Error: please log in to continue", false, ReadyNeedsAuth},
		{"unauthorized", "HTTP 401 Unauthorized", false, ReadyNeedsAuth},
		{"stale flag", "error: unexpected argument '--workspace' found", false, ReadyStale},
		{"unknown option", "unknown option: --sandbox", false, ReadyStale},
		{"probe ok", "some preamble\nPROBE_OK\n", false, ReadyOK},
		{"responded no gate", "Here is my answer: the file has 3 lines.", false, ReadyOK},
		{"timed out but produced work", "I analyzed the repo and started editing cmds/foo/foo.go with a full implementation ...", true, ReadyOK},
		{"stalled empty timeout", "\n  \n", true, ReadyNeedsAuth},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, note := classifyReadyOutput(tc.raw, tc.timedOut)
			if got != tc.want {
				t.Fatalf("classifyReadyOutput(%q, %v) = %q (%s), want %q", tc.raw, tc.timedOut, got, note, tc.want)
			}
		})
	}
}

// TestAgyProfileHasAuthHint guards that the tool most affected (agy, which needs
// a one-time browser sign-in) carries an actionable AuthHint for the pre-flight.
func TestAgyProfileHasAuthHint(t *testing.T) {
	p, ok := seededContract("agy")
	if !ok {
		t.Fatal("no seeded profile for agy")
	}
	if p.AuthHint == "" {
		t.Fatal("agy profile should carry an AuthHint (it requires an interactive sign-in)")
	}
}
