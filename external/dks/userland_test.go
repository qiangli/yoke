package dks

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestUnimplementedVerbsFailClosed pins the closed door: run, ps, logs,
// attach and stop are registered, print the not-yet message, exit 2, and
// NEVER run anything — neither a local process nor a plane call.
func TestUnimplementedVerbsFailClosed(t *testing.T) {
	for _, verb := range unimplementedVerbs {
		t.Run(verb, func(t *testing.T) {
			te := newTestEnv(t)
			code, _, stderr := runUserland(t, te.env, verb, "some-arg", "--detach")
			if code != 2 {
				t.Errorf("%s exited %d, want 2", verb, code)
			}
			want := "bashy dks " + verb + ": not available yet (" + notAvailableSprint + ")"
			if !strings.Contains(stderr, want) {
				t.Errorf("%s stderr %q does not carry %q", verb, stderr, want)
			}
			if len(te.execCalls) != 0 {
				t.Errorf("%s executed something locally: %v", verb, te.execCalls)
			}
			if len(te.runner.calls) != 0 {
				t.Errorf("%s reached a plane: %v", verb, te.runner.calls)
			}
		})
	}
}

// The same verbs through the shared DispatchUserland entry (the lean build's
// path) map to exit code 2 with the message on real stderr.
func TestDispatchUserlandNotAvailableExitsTwo(t *testing.T) {
	for _, verb := range unimplementedVerbs {
		old := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w
		code := DispatchUserland([]string{verb, "x"})
		w.Close()
		os.Stderr = old
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		if code != 2 {
			t.Errorf("DispatchUserland(%s) = %d, want 2", verb, code)
		}
		if !strings.Contains(buf.String(), "bashy dks "+verb+": not available yet") {
			t.Errorf("DispatchUserland(%s) stderr %q misses the not-yet message", verb, buf.String())
		}
	}
}

// IsUserland claims exactly the client verbs: never federation, never the
// engine VM verbs.
func TestIsUserlandDoesNotClaimFederation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"use", "skel"}, true},
		{[]string{"use"}, true},
		{[]string{"local", "--", "make"}, true},
		{[]string{"run", "render"}, true},
		{[]string{"ps"}, true},
		{[]string{"logs", "01J"}, true},
		{[]string{"attach", "01J"}, true},
		{[]string{"stop", "01J"}, true},
		{[]string{"federation"}, false},
		{[]string{"federation", "status"}, false},
		{[]string{"federation", "join"}, false},
		{[]string{"federation", "karmadactl", "version"}, false},
		{[]string{"provision"}, false},
		{[]string{"status"}, false},
		{[]string{"bogus"}, false},
		{nil, false},
		{[]string{}, false},
	} {
		if got := IsUserland(tc.args); got != tc.want {
			t.Errorf("IsUserland(%q) = %v, want %v", tc.args, got, tc.want)
		}
		if IsFederation(tc.args) && IsUserland(tc.args) {
			t.Errorf("both doors claim %q", tc.args)
		}
	}
}

// NewUserlandCmds registers exactly the userland verbs, once each.
func TestNewUserlandCmdsRegistersEveryVerb(t *testing.T) {
	cmds := NewUserlandCmds()
	seen := map[string]bool{}
	for _, c := range cmds {
		seen[c.Name()] = true
	}
	for verb := range userlandVerbs {
		if !seen[verb] {
			t.Errorf("verb %q has no registered command", verb)
		}
	}
	for name := range seen {
		if userlandVerbs[name] == "" {
			t.Errorf("registered command %q is not a userland verb", name)
		}
	}
	for _, verb := range unimplementedVerbs {
		if userlandVerbs[verb] == "" {
			t.Errorf("unimplemented verb %q is missing from userlandVerbs", verb)
		}
	}
}

// DispatchUserland refuses non-userland args loudly instead of guessing.
func TestDispatchUserlandRefusesForeignVerbs(t *testing.T) {
	for _, args := range [][]string{{"provision"}, {"federation", "status"}, {"bogus"}, {}} {
		if code := DispatchUserland(args); code != 1 {
			t.Errorf("DispatchUserland(%q) = %d, want 1", args, code)
		}
	}
}

// Help on the userland verbs stays zero-ceremony: no YAML, no kubectl, no
// kubeconfig on the default path.
func TestUserlandHelpHasZeroCeremony(t *testing.T) {
	for _, c := range NewUserlandCmds() {
		var out bytes.Buffer
		help := &cobra.Command{Use: "dks", SilenceUsage: true, SilenceErrors: true}
		help.AddCommand(c)
		help.SetArgs([]string{c.Name(), "--help"})
		help.SetOut(&out)
		if err := help.Execute(); err != nil {
			t.Fatalf("%s --help: %v", c.Name(), err)
		}
		for _, banned := range []string{"YAML", "yaml", "kubeconfig", "kubectl"} {
			if strings.Contains(out.String(), banned) {
				t.Errorf("%s help mentions %q:\n%s", c.Name(), banned, out.String())
			}
		}
	}
}

// The sentinel mapping of the shared exit-code path.
func TestUserlandExitCodeMapping(t *testing.T) {
	if got := userlandExitCode(nil); got != 0 {
		t.Errorf("nil → %d, want 0", got)
	}
	if got := userlandExitCode(errNotAvailable); got != 2 {
		t.Errorf("errNotAvailable → %d, want 2", got)
	}
	if got := userlandExitCode(errors.New("boom")); got != 1 {
		t.Errorf("plain error → %d, want 1", got)
	}
	if got := userlandExitCode(&exitStatusError{code: 42}); got != 42 {
		t.Errorf("exit status 42 → %d, want 42", got)
	}
}
