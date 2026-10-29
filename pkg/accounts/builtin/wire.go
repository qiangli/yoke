package builtin

// The `--account` wiring (S524.7, slice D).
//
// pkg/fleet and pkg/secrets both take `--account`, and neither may import
// pkg/accounts: pkg/accounts depends on BOTH (on fleet through llmbudget, on
// secrets to materialize a secret: reference), so an import back would be a
// cycle. Each therefore declares ONE package-level resolver variable, and this
// package — which sits above all three and is already the single place hosts
// blank-import for adapter registration — fills them in.
//
// That makes this file the one place that knows how a reference becomes a
// usable identity for every non-LLM family, so DigitalOcean, AWS and Cloudbox
// cannot drift in how they resolve, what they refuse, or what they leak.
// (The LLM family needs no seam: pkg/chat may import pkg/accounts directly,
// and it resolves a BINDING rather than a credential.)

import (
	"context"

	"github.com/qiangli/yoke/pkg/accounts"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
)

func init() {
	fleet.AccountResolver = resolveFleetAccount
	secrets.AccountResolver = resolveSecretsAccount
}

// accountStore and accountMaterializer are the inventory and the credential
// resolver. They are variables only so a test can inject a fake vault; every
// other seam (BASHY_HOME, XDG_CONFIG_HOME, AWS_SHARED_CREDENTIALS_FILE, the
// credential environment variables) is already reachable from the process
// environment, so a test normally needs neither.
var (
	accountStore        = func() *accounts.Store { return accounts.Open(accounts.DefaultPath()) }
	accountMaterializer = accounts.DefaultMaterializer
)

// resolve is the shared half: select one connection for the operation, then
// materialize its credential for this call only. Both failures carry a code
// and no provider text.
func resolve(ctx context.Context, ref, operation string) (accounts.Account, accounts.Selection, accounts.Credential, error) {
	acct, sel, err := accounts.SelectFor(ctx, accountStore(), accounts.DefaultRegistry(), ref, accounts.Operation(operation))
	if err != nil {
		return acct, sel, accounts.Credential{}, err
	}
	cred, err := accountMaterializer().Materialize(ctx, sel, acct.Category)
	return acct, sel, cred, err
}

// resolveFleetAccount answers pkg/fleet's seam. The credential it returns is
// the one this invocation uses and nothing else: it is handed to one provider
// adapter or one HTTP client and discarded with the call.
func resolveFleetAccount(ctx context.Context, ref, operation string) (fleet.AccountSelection, error) {
	acct, sel, cred, err := resolve(ctx, ref, operation)
	if err != nil {
		return fleet.AccountSelection{}, err
	}
	out := fleet.AccountSelection{
		Ref:       ref,
		AccountID: acct.ID,
		Category:  string(acct.Category),
		Endpoint:  sel.Endpoint,
		Profile:   sel.Profile,
	}
	// AWS is the one family whose credential is a PAIR; everyone else has a
	// single token. Assigning only the field that applies keeps a token from
	// standing in for a key pair by accident.
	if acct.Category == accounts.AWS {
		out.AWSKeys = cred.AWS
	} else {
		out.Token = cred.Token
	}
	return out, nil
}

// resolveSecretsAccount answers pkg/secrets' seam: a vault endpoint plus a
// Bearer token for one invocation.
func resolveSecretsAccount(ctx context.Context, ref, operation string) (secrets.AccountSelection, error) {
	acct, sel, cred, err := resolve(ctx, ref, operation)
	if err != nil {
		return secrets.AccountSelection{}, err
	}
	return secrets.AccountSelection{
		Ref:       ref,
		AccountID: acct.ID,
		Category:  string(acct.Category),
		Endpoint:  sel.Endpoint,
		Token:     cred.Token,
	}, nil
}
