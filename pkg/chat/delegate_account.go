// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package chat

// `bashy delegate --account` (S524.7, slice D — the LLM family).
//
// An LLM account is a SEAT, not a model: the thing an operator actually wants
// to pick is "run this on my owl subscription", and what makes that runnable
// is the agent binding linked to that account. So `--account <alias|id>`
// resolves to that binding and then behaves exactly as `--agent <binding>` —
// one code path, one set of budget and authorization guards, nothing special
// downstream.
//
// The rules:
//
//   - EXACTLY ONE usable binding. An account linked to two bindings is
//     account_ambiguous, not a coin flip: picking one would silently spend
//     from a seat the operator did not name.
//   - Conflicts fail BEFORE anything launches. --agent, --self and --model are
//     each a different answer to "which agent", so `--account` alongside any of
//     them is account_conflict.
//   - No credential is materialized here. A delegate launch authenticates
//     through the tool's own verified login; the account supplies the BINDING,
//     and the account service is read-only in this path.

import (
	"context"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/accounts"
)

// accountStore is the inventory delegate reads. It is a function so a test
// points it at a temporary store without a global.
var accountStore = func() *accounts.Store { return accounts.Open(accounts.DefaultPath()) }

// delegateAccountConflict names the flag that already answered "which agent".
// It runs before resolution, so a conflict costs no lookup and no launch.
func delegateAccountConflict(account, agent, model string, self bool) error {
	if strings.TrimSpace(account) == "" {
		return nil
	}
	switch {
	case strings.TrimSpace(agent) != "":
		return accountDelegateFail(accounts.CodeConflict, "--account and --agent both name a target; pass one")
	case self:
		return accountDelegateFail(accounts.CodeConflict, "--account names another seat; --self is this shell's own tool")
	case strings.TrimSpace(model) != "":
		return accountDelegateFail(accounts.CodeConflict, "--model applies to --self; with --account the binding names the model")
	}
	return nil
}

// resolveDelegateAccount returns the agent binding `--account` selects.
func resolveDelegateAccount(ctx context.Context, ref string) (string, error) {
	acct, _, selErr := accounts.SelectFor(ctx, accountStore(), accounts.DefaultRegistry(), ref, accounts.OpDelegate)
	// A CONNECTION-level failure is not fatal here, and that is the one place
	// this family departs from the others: an LLM seat may hold a connection
	// per pool, and this path needs the account's agent links rather than a
	// credential, so "which connection" is not a question worth refusing over.
	// An account that could not be found, or that was removed, still refuses —
	// SelectFor leaves the account zero in exactly the first case.
	if acct.ID == "" || acct.Status == accounts.Suppressed {
		if selErr == nil {
			selErr = accountDelegateFail(accounts.CodeUnresolved, "account "+ref+" is not usable")
		}
		return "", selErr
	}
	if acct.Category != accounts.LLM {
		return "", accountDelegateFail(accounts.CodeUnsupported,
			fmt.Sprintf("account category %q cannot be delegated to (want llm)", acct.Category))
	}
	bindings := usableAgentBindings(acct)
	switch len(bindings) {
	case 1:
		return bindings[0], nil
	case 0:
		return "", accountDelegateFail(accounts.CodeUnresolved,
			"account "+ref+" has no linked agent binding; link one with 'bashy account set "+ref+"'")
	default:
		return "", accountDelegateFail(accounts.CodeAmbiguous,
			"account "+ref+" is linked to "+fmt.Sprint(len(bindings))+" agent bindings; name one with --agent")
	}
}

// usableAgentBindings is the account's linked bindings, trimmed and deduped in
// recorded order. The binding is resolved downstream exactly as `--agent`
// resolves one, so "usable" here means "names a binding", not "the catalog
// already knows it" — an unknown name must produce delegate's own clear error
// rather than a selection error that hides it.
func usableAgentBindings(acct accounts.Account) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(acct.Links.Agents))
	for _, a := range acct.Links.Agents {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

func accountDelegateFail(code, msg string) error {
	return &accounts.SelectionError{Code: code, Message: msg}
}
