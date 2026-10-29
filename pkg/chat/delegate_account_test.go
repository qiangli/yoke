// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package chat

// `bashy delegate --account` (S524.7, slice D — the LLM family).
//
// The flag resolves a SEAT to the agent binding linked to it and then behaves
// exactly as --agent. These tests pin the three things that makes safe: the
// right binding for the right account, a refusal when the account names more
// than one, and a refusal before anything launches when another flag already
// answered "which agent".

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/accounts"
	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

// llmSeats records two LLM accounts, each linked to its own agent binding, in
// an isolated host-local store.
func llmSeats(t *testing.T) (owl, otter string) {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	st := accounts.Open(accounts.DefaultPath())
	prev := accountStore
	accountStore = func() *accounts.Store { return st }
	t.Cleanup(func() { accountStore = prev })

	seats := []struct {
		account, binding string
	}{{"owl-seat", "owl-claude"}, {"otter-seat", "otter-codex"}}
	for _, s := range seats {
		err := st.Observe("llm", []accounts.Observation{{Account: accounts.Account{
			Category: accounts.LLM, Provider: "anthropic", ProviderAccountID: s.account,
			Links: accounts.Links{Agents: []string{s.binding}},
		}}})
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	list, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, a := range list {
		switch a.ProviderAccountID {
		case "owl-seat":
			owl = a.ID
		case "otter-seat":
			otter = a.ID
		}
	}
	if owl == "" || otter == "" {
		t.Fatalf("seats not recorded: %+v", list)
	}
	return owl, otter
}

// Each account maps to ITS OWN binding; the two never cross.
func TestDelegateAccountResolvesItsOwnBinding(t *testing.T) {
	owl, otter := llmSeats(t)
	for ref, want := range map[string]string{owl: "owl-claude", otter: "otter-codex"} {
		got, err := resolveDelegateAccount(context.Background(), ref)
		if err != nil {
			t.Fatalf("resolve %s: %v", ref, err)
		}
		if got != want {
			t.Fatalf("account %s resolved binding %q, want %q", ref, got, want)
		}
	}
}

// An account linked to two bindings is ambiguous: picking one would silently
// spend from a seat the operator did not name.
func TestDelegateAccountAmbiguousWithTwoBindings(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	st := accounts.Open(accounts.DefaultPath())
	prev := accountStore
	accountStore = func() *accounts.Store { return st }
	t.Cleanup(func() { accountStore = prev })

	err := st.Observe("llm", []accounts.Observation{{Account: accounts.Account{
		Category: accounts.LLM, Provider: "anthropic", ProviderAccountID: "shared-seat",
		Links: accounts.Links{Agents: []string{"owl-claude", "otter-codex"}},
	}}})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	list, _ := st.List()
	_, err = resolveDelegateAccount(context.Background(), list[0].ID)
	if accounts.ErrorCode(err) != accounts.CodeAmbiguous {
		t.Fatalf("code = %q (%v), want %s", accounts.ErrorCode(err), err, accounts.CodeAmbiguous)
	}

	// And one with no binding at all is unresolved, not a silent default.
	err = st.Observe("llm", []accounts.Observation{{Account: accounts.Account{
		Category: accounts.LLM, Provider: "openai", ProviderAccountID: "bare-seat",
	}}})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	list, _ = st.List()
	for _, a := range list {
		if a.ProviderAccountID != "bare-seat" {
			continue
		}
		if _, err := resolveDelegateAccount(context.Background(), a.ID); accounts.ErrorCode(err) != accounts.CodeUnresolved {
			t.Fatalf("bare seat: code = %q (%v)", accounts.ErrorCode(err), err)
		}
	}
}

// Only an LLM account can be delegated to.
func TestDelegateAccountRejectsForeignCategory(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	st := accounts.Open(accounts.DefaultPath())
	prev := accountStore
	accountStore = func() *accounts.Store { return st }
	t.Cleanup(func() { accountStore = prev })

	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", accountstest.CanarySecret)
	err := st.Observe("digitalocean", []accounts.Observation{{Account: accounts.Account{
		Category: accounts.DigitalOcean, Provider: "digitalocean", ProviderAccountID: "team-owl",
		Connections: []accounts.Connection{{ID: "owl", CredentialRef: "env:DIGITALOCEAN_ACCESS_TOKEN"}},
		Links:       accounts.Links{Agents: []string{"owl-claude"}},
	}}})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	list, _ := st.List()
	_, err = resolveDelegateAccount(context.Background(), list[0].ID)
	if accounts.ErrorCode(err) != accounts.CodeUnsupported {
		t.Fatalf("code = %q (%v), want %s", accounts.ErrorCode(err), err, accounts.CodeUnsupported)
	}
	accountstest.AssertNoSecret(t, err.Error())
}

// --agent, --self and --model each already answer "which agent", so --account
// alongside any of them refuses before a lookup, let alone a launch.
func TestDelegateAccountConflicts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent string
		model string
		self  bool
	}{
		{"agent", "codex", "", false},
		{"self", "", "", true},
		{"model", "", "opus", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := delegateAccountConflict("owl-seat", tc.agent, tc.model, tc.self)
			if accounts.ErrorCode(err) != accounts.CodeConflict {
				t.Fatalf("code = %q (%v), want %s", accounts.ErrorCode(err), err, accounts.CodeConflict)
			}
		})
	}
	if err := delegateAccountConflict("", "codex", "opus", true); err != nil {
		t.Fatalf("without --account there is no conflict: %v", err)
	}
}

// The flag is registered on the verb, which is what puts it in bashy's help and
// its MCP tool schema (both reflected off the cobra tree). A conflict refuses
// through the real command, before any agent is launched.
func TestDelegateAccountFlagAndConflictThroughTheCommand(t *testing.T) {
	cmd := NewDelegateCmd()
	if cmd.Flags().Lookup("account") == nil {
		t.Fatal("delegate has no --account flag")
	}
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"--account", "owl-seat", "--agent", "codex", "do the thing"})
	err := cmd.Execute()
	if accounts.ErrorCode(err) != accounts.CodeConflict {
		t.Fatalf("code = %q (%v), want %s", accounts.ErrorCode(err), err, accounts.CodeConflict)
	}
	if strings.Contains(out.String(), "owl") {
		t.Fatalf("the refusal produced output: %q", out.String())
	}
}

// Without --account the account service is never consulted: the unchanged path
// must not acquire a new dependency on the inventory.
func TestDelegateWithoutAccountNeverReadsTheInventory(t *testing.T) {
	consulted := 0
	prev := accountStore
	accountStore = func() *accounts.Store {
		consulted++
		return accounts.Open(accounts.DefaultPath())
	}
	t.Cleanup(func() { accountStore = prev })

	cmd := NewDelegateCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	// --dry-run resolves the invocation and prints it without launching.
	cmd.SetArgs([]string{"--agent", "codex", "--dry-run", "say hello"})
	_ = cmd.Execute()
	if consulted != 0 {
		t.Fatal("a delegate without --account read the account inventory")
	}
}
