package weave

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// runSprintStreams drives the SPRINT tree with stdout and stderr captured
// separately. The contract under test is specifically about stderr being
// non-empty, so the two streams must not be merged.
func runSprintStreams(t *testing.T, args ...string) (stdout, stderr string, code int, structured bool) {
	t.Helper()
	cmd := NewSprintCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errb.String(), ExitCode(err), IsStructuredExit(err)
}

// TestSprintUnknownFlagFailsLoud pins the fix for todo b0acdf2c.
//
// NewSprintCmd inherited weave's silence (flags.attach sets
// SilenceErrors/SilenceUsage) but installed NEITHER reporter, so every
// subverb exited 1 having written ZERO bytes to stdout AND stderr. A silent
// exit 1 is indistinguishable from "ran, found nothing" — it cost three
// recorded incidents on sprint #87, most recently a continuity record its
// conductor believed was written and was not, because `--as` is not a
// checkpoint flag and nothing said so.
//
// Every case below was measured FAILING (exit=1, out=0B, err=0B) before the
// fix. If this test ever goes quiet again, that is the regression.
func TestSprintUnknownFlagFailsLoud(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "") // human rows, not the agent JSON envelope

	cases := []struct {
		name     string
		args     []string
		wantFlag string
		wantHint string // nearest-valid-flag suggestion; "" if none required
	}{
		// The one that actually bit the conductor.
		{"checkpoint --as", []string{"checkpoint", "87", "--as", "x"}, "--as", ""},
		// Recorded earlier on the same card.
		{"comment --body", []string{"comment", "87", "--body", "x"}, "--body", ""},
		{"show typo", []string{"show", "87", "--nope"}, "--nope", ""},
		{"take typo", []string{"take", "87", "--bogus"}, "--bogus", ""},
		{"board typo", []string{"board", "--nope"}, "--nope", ""},
		{"status typo", []string{"status", "--nope"}, "--nope", ""},
		{"root typo", []string{"--bogus"}, "--bogus", ""},
		// Nested group: FlagErrorFunc climbs to the parent, so `session`
		// must be covered by the single root install.
		{"nested session group", []string{"session", "--nope"}, "--nope", ""},
		// Suggestion path still works on the sprint tree.
		{"checkpoint near-miss", []string{"checkpoint", "87", "--mesage", "x"}, "--mesage", "--message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code, structured := runSprintStreams(t, tc.args...)
			if strings.TrimSpace(stderr) == "" {
				t.Fatalf("SILENT FAILURE: stderr empty for `sprint %s` (stdout=%q, exit=%d)",
					strings.Join(tc.args, " "), stdout, code)
			}
			if !strings.Contains(stderr, tc.wantFlag) {
				t.Errorf("stderr should name the offending flag %q, got %q", tc.wantFlag, stderr)
			}
			if tc.wantHint != "" && !strings.Contains(stderr, tc.wantHint) {
				t.Errorf("stderr should suggest %q, got %q", tc.wantHint, stderr)
			}
			if code != weavecli.ExitInvalidArg {
				t.Errorf("exit = %d, want %d (usage error); stderr=%q",
					code, weavecli.ExitInvalidArg, stderr)
			}
			// Structured means the tree already reported it, so a host that
			// prints only when IsStructuredExit is false stays silent instead
			// of double-printing.
			if !structured {
				t.Errorf("want a structured exit so the host does not double-print; stderr=%q", stderr)
			}
		})
	}
}

// TestSprintUnknownSubcommandFailsLoud covers the positional half. cobra's
// legacyArgs only runs the unknown-command check on the ROOT, so a typo'd
// verb under a nested group would otherwise be discarded silently — the
// exact failure argerr.go was written for, now wired into sprint too.
func TestSprintUnknownSubcommandFailsLoud(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "")

	for _, args := range [][]string{
		{"bogusverb"},
		{"session", "bogusverb"},
	} {
		stdout, stderr, code, _ := runSprintStreams(t, args...)
		if strings.TrimSpace(stderr) == "" {
			t.Fatalf("SILENT FAILURE: stderr empty for `sprint %s` (stdout=%q, exit=%d)",
				strings.Join(args, " "), stdout, code)
		}
		if !strings.Contains(stderr, "bogusverb") {
			t.Errorf("stderr should name the bad subcommand, got %q", stderr)
		}
		if code != weavecli.ExitInvalidArg {
			t.Errorf("exit = %d, want %d; stderr=%q", code, weavecli.ExitInvalidArg, stderr)
		}
	}
}

// TestSprintValidFlagsStillWork guards the blast radius: installing the two
// reporters must not turn a VALID invocation into an error. `sprint --help`
// and a well-formed flag both have to keep working.
func TestSprintValidFlagsStillWork(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "")

	for _, args := range [][]string{
		{"--help"},
		{"show", "--help"},
		{"checkpoint", "--help"},
	} {
		_, stderr, code, _ := runSprintStreams(t, args...)
		if code != weavecli.ExitOK {
			t.Errorf("`sprint %s` exit = %d, want 0; stderr=%q",
				strings.Join(args, " "), code, stderr)
		}
	}
}

// TestSprintMissingRequiredFlagFailsLoud pins the fix for todo 75d1842bc4c9.
//
// TestSprintUnknownFlagFailsLoud above covers the flag-PARSE path, which cobra
// routes through FlagErrorFunc and flagerr.go reports. A guard that runs INSIDE
// RunE — "--reason is required", "-m required", "sprint must be an integer" —
// never touches that path: it is an ordinary error returned from RunE, and with
// SilenceErrors set on every sprint command nothing prints it at all.
//
// The cost is recorded on the story and is worse than an error: a conductor ran
// two `sprint goal rm` calls, saw nothing, believed both goals were retired, and
// printed a "corrected" sprint that still carried them. A silent exit 1 does not
// stop you — it produces a confident wrong belief.
//
// Every case below was measured FAILING (exit=1, out=0B, err=0B) before the fix.
// The sprint ids are deliberately absent (#999999) so a case can never mutate a
// real card: each guard returns before the store is opened.
func TestSprintMissingRequiredFlagFailsLoud(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "")

	cases := []struct {
		name string
		args []string
		want string // a substring naming what is missing
	}{
		// The two the conductor of sprint #135 actually hit.
		{"goal rm without --reason", []string{"goal", "rm", "999999", "some-goal"}, "--reason"},
		{"handoff without -m", []string{"handoff", "999999"}, "-m"},
		// Same shape, found by auditing the siblings rather than patching the
		// two above: a positional that fails to parse is also a bare RunE error.
		{"non-integer sprint id", []string{"handoff", "notanumber", "-m", "x"}, "integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code, structured := runSprintStreams(t, tc.args...)
			if strings.TrimSpace(stderr) == "" {
				t.Fatalf("SILENT FAILURE: stderr empty for `sprint %s` (stdout=%q, exit=%d)",
					strings.Join(tc.args, " "), stdout, code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr should say what is missing (%q), got %q", tc.want, stderr)
			}
			if !strings.Contains(stderr, "sprint") {
				t.Errorf("stderr should name the command, got %q", stderr)
			}
			if code == 0 {
				t.Errorf("exit = 0 for a refused invocation; stderr=%q", stderr)
			}
			// Structured means the tree reported it, so a host that prints only
			// when IsStructuredExit is false stays silent instead of double-printing.
			if !structured {
				t.Errorf("want a structured exit so the host does not double-print; stderr=%q", stderr)
			}
		})
	}
}
