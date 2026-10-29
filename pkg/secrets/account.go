package secrets

// `bashy secret --account` (S524.7, slice D).
//
// The vault is a cloudbox endpoint plus a Bearer token, which is exactly what
// an account's cloudbox connection is. `--account` substitutes BOTH for one
// invocation: the endpoint the account names and the token materialized from
// its credential reference, with no ladder behind it. Nothing is exported to
// the environment, written to ~/.config/bashy/secrets-token, or cached.
//
// Like pkg/fleet, this package cannot import pkg/accounts — pkg/accounts
// imports THIS package to materialize secret: references — so the identity
// arrives through one package-level variable a wiring package sets at init
// (pkg/accounts/builtin).

import (
	"context"
	"strings"
)

// AccountSelection is one account's cloudbox connection for one invocation.
type AccountSelection struct {
	// Ref is what the operator typed — safe to print.
	Ref string
	// AccountID is the stable local id of the selected account.
	AccountID string
	// Category is the account category string (want cloudbox or satellite).
	Category string
	// Endpoint is the cloudbox base URL.
	Endpoint string
	// Token is the materialized Bearer token for this call only.
	Token string
}

// AccountError is a structured refusal: a stable code, no credential text.
type AccountError struct {
	Code    string
	Message string
}

func (e *AccountError) Error() string { return "secrets: " + e.Code + ": " + e.Message }

// ErrorCode lets a caller read the code without importing this package's type.
func (e *AccountError) ErrorCode() string { return e.Code }

const (
	accountCodeUnresolved  = "account_unresolved"
	accountCodeUnsupported = "account_unsupported"
	accountCodeConflict    = "account_conflict"
	accountCodeCredential  = "credential_unavailable"
)

func accountFail(code, msg string) error { return &AccountError{Code: code, Message: msg} }

// AccountResolver resolves one account reference to a per-invocation cloudbox
// connection. Nil until a host wires the account service in; nil means
// `--account` refuses rather than quietly using the ambient token.
var AccountResolver func(ctx context.Context, ref string, operation string) (AccountSelection, error)

// accountOpSecret is the operation name this verb selects for
// (accounts.OpSecret).
const accountOpSecret = "secret"

// resolveAccount turns Config.Account into an endpoint and a token. It refuses
// before any request: a conflicting --url/--token, a category that is not a
// cloudbox connection, and a credential that cannot be materialized are all
// refusals, never a fall-through to the default vault.
func (c Config) resolveAccount(ctx context.Context) (url, token string, err error) {
	ref := strings.TrimSpace(c.Account)
	if ref == "" {
		return c.URL, c.Token, nil
	}
	if strings.TrimSpace(c.URL) != "" || strings.TrimSpace(c.Token) != "" {
		return "", "", accountFail(accountCodeConflict,
			"--account already names the vault endpoint and token; drop --url/--token")
	}
	if AccountResolver == nil {
		return "", "", accountFail(accountCodeUnresolved, "no account service is wired into this binary")
	}
	sel, serr := AccountResolver(ctx, ref, accountOpSecret)
	if serr != nil {
		return "", "", serr
	}
	if sel.Category != "cloudbox" && sel.Category != "satellite" {
		return "", "", accountFail(accountCodeUnsupported,
			"account category \""+sel.Category+"\" is not a cloudbox connection")
	}
	url = strings.TrimRight(strings.TrimSpace(sel.Endpoint), "/")
	if url == "" {
		return "", "", accountFail(accountCodeUnresolved, "account "+ref+" has no cloudbox endpoint")
	}
	if strings.TrimSpace(sel.Token) == "" {
		return "", "", accountFail(accountCodeCredential, "account "+ref+" has no usable cloudbox token")
	}
	return url, sel.Token, nil
}
