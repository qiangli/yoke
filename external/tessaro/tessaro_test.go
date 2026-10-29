package tessaro

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestSubVerbMapping(t *testing.T) {
	cases := map[string]string{
		"login": "register", "signin": "register", "pair": "register",
		"logout": "unpair", "signout": "unpair", "unpair": "unpair",
		"status": "status", "whoami": "status",
	}
	for verb, want := range cases {
		if got := subVerbs[verb]; got != want {
			t.Errorf("subVerbs[%q] = %q, want %q", verb, got, want)
		}
	}
	// `open` is handled specially (no agent needed), not via subVerbs.
	if _, ok := subVerbs["open"]; ok {
		t.Error("open must not be in subVerbs (handled without the agent)")
	}
}

func TestCommandsBuild(t *testing.T) {
	if NewTessaroCmd().Name() != "tessaro" {
		t.Error("tessaro command name wrong")
	}
	if NewLoginCmd().Name() != "login" {
		t.Error("login command name wrong")
	}
}

// `--account` is UNSUPPORTED here (S524.7): these verbs pass their argv
// straight through to the outpost agent, and pairing establishes an account
// rather than selecting one. Forwarding the flag would hand outpost something
// it does not know while reading as though a selection had been honored — so
// it is refused by name, before the exec.
func TestAccountFlagIsRefusedBeforeTheExec(t *testing.T) {
	for _, args := range [][]string{
		{"--account", "owl"},
		{"--account=owl"},
		{"login", "--account", "owl"},
		{"status", "--account=owl"},
	} {
		if !accountUnsupported(args) {
			t.Errorf("accountUnsupported(%v) = false, want true", args)
		}
	}
	// Past a `--` the words belong to the remote side, not to us.
	for _, args := range [][]string{
		{"login"},
		{"status", "--json"},
		{"login", "--", "--account", "owl"},
	} {
		if accountUnsupported(args) {
			t.Errorf("accountUnsupported(%v) = true, want false", args)
		}
	}
	if !strings.Contains(accountUnsupportedMsg, "account_unsupported") {
		t.Errorf("the refusal must carry the stable code: %q", accountUnsupportedMsg)
	}
}

// Both front doors refuse, and neither reaches the agent.
func TestAccountFlagRefusedOnBothVerbs(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{"tessaro login", NewTessaroCmd(), []string{"login", "--account", "owl"}},
		{"login", NewLoginCmd(), []string{"--account", "owl"}},
	} {
		var out, errb bytes.Buffer
		tc.cmd.SetOut(&out)
		tc.cmd.SetErr(&errb)
		tc.cmd.SetArgs(tc.args)
		if err := tc.cmd.Execute(); !errors.Is(err, errHandled) {
			t.Fatalf("%s: err = %v, want the handled sentinel", tc.name, err)
		}
		if !strings.Contains(errb.String(), "account_unsupported") {
			t.Fatalf("%s: stderr = %q, want the account_unsupported refusal", tc.name, errb.String())
		}
	}
}
