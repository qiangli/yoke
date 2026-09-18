package weave

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// TestUnknownSubcommandFailsLoud pins the fix for the third silent
// structural failure (#118 residual): a typo'd or unknown SUBCOMMAND
// must name itself on stderr and exit 2. #131 covered flags, #141
// covered bare argv; `weave bogusxyz42` still exited 1 with ZERO output,
// and `weave baton bogus` — worse — exited 0 having silently ignored the
// typo (cobra only unknown-checks the ROOT's children).
//
// Per the #141 lesson these drive the real argv path via runWeaveStreams
// (flagerr_test.go) and assert on the captured stderr BYTES.
func TestUnknownSubcommandFailsLoud(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "") // human rows, not the agent JSON envelope

	cases := []struct {
		name     string
		args     []string
		wantName string // the offending operand, verbatim
		wantPath string // the command path the error is attributed to
		wantHint string // did-you-mean suggestion, "" if none required
	}{
		{"root unknown", []string{"bogusxyz42"}, `"bogusxyz42"`, `"weave"`, ""},
		{"root typo suggests", []string{"statu"}, `"statu"`, `"weave"`, `"status"`},
		{"nested group unknown", []string{"baton", "bogus"}, `"bogus"`, `"weave baton"`, ""},
		{"nested group typo suggests", []string{"baton", "wrote"}, `"wrote"`, `"weave baton"`, `"write"`},
		{"fleet group unknown", []string{"fleet", "nosuchxyz"}, `"nosuchxyz"`, `"weave fleet"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code, structured := runWeaveStreams(t, tc.args...)
			if code != weavecli.ExitInvalidArg {
				t.Errorf("exit = %d, want %d (usage error); stderr=%q", code, weavecli.ExitInvalidArg, stderr)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatalf("stderr must not be empty for %v (stdout=%q)", tc.args, stdout)
			}
			if !strings.Contains(stderr, "unknown command") || !strings.Contains(stderr, tc.wantName) {
				t.Errorf("stderr should report the unknown command %s, got %q", tc.wantName, stderr)
			}
			if !strings.Contains(stderr, tc.wantPath) {
				t.Errorf("stderr should attribute the error to %s, got %q", tc.wantPath, stderr)
			}
			if tc.wantHint != "" && !strings.Contains(stderr, tc.wantHint) {
				t.Errorf("stderr should suggest %s, got %q", tc.wantHint, stderr)
			}
			// weave reported it itself, so a host driving Execute() must
			// stay silent rather than double-print.
			if !structured {
				t.Errorf("args error should be a structured exit (already reported), stderr=%q", stderr)
			}
		})
	}
}

// TestMissingOperandFailsLoud: the sibling silent case — a VALID
// subcommand whose Args validator rejects the operands (`weave status`
// with no issue) also never reached RunE and printed nothing.
func TestMissingOperandFailsLoud(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "")
	stdout, stderr, code, structured := runWeaveStreams(t, "status")
	if code != weavecli.ExitInvalidArg {
		t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, weavecli.ExitInvalidArg, stderr, stdout)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Fatalf("stderr must not be empty for `weave status` with no operand")
	}
	if !strings.Contains(stderr, "requires at least 1 arg") {
		t.Errorf("stderr should carry cobra's arity message, got %q", stderr)
	}
	if !strings.Contains(stderr, "weave status") {
		t.Errorf("stderr should name the command, got %q", stderr)
	}
	if !structured {
		t.Errorf("args error should be a structured exit, stderr=%q", stderr)
	}
}

// TestMissingOperandJSONEnvelope: a subverb that defines --json answers
// an args failure in the envelope shape the caller asked for, same as
// the flag-error contract.
func TestMissingOperandJSONEnvelope(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "")
	_, stderr, code, _ := runWeaveStreams(t, "status", "--json")
	if code != weavecli.ExitInvalidArg {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, weavecli.ExitInvalidArg, stderr)
	}
	var env weavecli.Envelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &env); err != nil {
		t.Fatalf("stderr should be a JSON envelope, got %q (%v)", stderr, err)
	}
	if env.Status != "error" || env.Error == nil {
		t.Fatalf("envelope should carry an error, got %+v", env)
	}
	if env.Error.Code != "invalid_arg" {
		t.Errorf("error code = %q, want invalid_arg", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "requires at least 1 arg") {
		t.Errorf("message should carry the arity failure, got %q", env.Error.Message)
	}
}

// TestBareRootStillReportsMissingCommand guards the #141 fix against
// this one: wrapping the root's Args validator must not intercept EMPTY
// argv, which belongs to the root RunE's "missing command" envelope.
func TestBareRootStillReportsMissingCommand(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "")
	_, stderr, code, structured := runWeaveStreams(t)
	if code != weavecli.ExitInvalidArg {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, weavecli.ExitInvalidArg, stderr)
	}
	if !strings.Contains(stderr, "missing command") {
		t.Fatalf("bare `weave` should still emit the missing-command diagnostic, got %q", stderr)
	}
	if !structured {
		t.Errorf("bare-root error should be a structured exit, stderr=%q", stderr)
	}
}

// TestHelpSubcommandStillResolves: the wrapper rejects ANY positional on
// a group, so pin that cobra's auto-added `help` command (registered at
// Execute time, after the wrap) still resolves as a subcommand rather
// than being reported as unknown.
func TestHelpSubcommandStillResolves(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "")
	stdout, stderr, code, _ := runWeaveStreams(t, "help", "baton")
	if code != 0 {
		t.Fatalf("`weave help baton` should exit 0, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "baton") {
		t.Errorf("help output should describe baton, got %q", stdout)
	}
}
