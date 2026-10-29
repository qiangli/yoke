package fleet

// `--account` for the infrastructure and cloudbox verbs (S524.7, slice D).
//
// WHY A SEAM AND NOT AN IMPORT. The account inventory lives in
// pkg/accounts, and pkg/accounts already depends on THIS package
// (accounts -> llmbudget -> fleet). Importing it back would be an import
// cycle, so the identity arrives through ONE package-level variable that a
// wiring package sets at init — the same shape as weavecli.DetectTool and
// schedule.DefaultAdmission. pkg/accounts/builtin is that wiring package;
// a host that blank-imports it (bashy's internal/agentos does) gets
// `--account` everywhere, and a host that does not gets a clean refusal
// instead of a surprise.
//
// WHAT THE RULES ARE, for every verb below:
//
//   - The credential is materialized for THIS invocation only. It reaches
//     one adapter or one client and is never written to the process
//     environment, a file, or a provider CLI's active profile. No
//     `doctl auth switch`, no AWS_PROFILE export, no cloudbox profile
//     activation.
//   - There is NO FALLBACK. With `--account` the account's identity is the
//     only one that may be used; a credential that cannot be resolved is a
//     refusal, never a silent slide onto the ambient token.
//   - A conflicting explicit flag, and a category that cannot drive the
//     verb, fail BEFORE the first network call or side effect.
//   - Without `--account` nothing changes: every path below is the
//     historical one, byte for byte.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
)

// Account categories this package can act as. They are the string values of
// pkg/accounts.Category; the vocabulary is duplicated rather than imported
// for the cycle reason above, and accountCategoriesAgree in the tests pins
// the two lists together from the wiring package, which sees both.
const (
	AccountDigitalOcean = "digitalocean"
	AccountAWS          = "aws"
	AccountCloudbox     = "cloudbox"
	AccountSatellite    = "satellite"
)

// AccountSelection is one account's identity for one invocation: where to go,
// who to be, and the credential already materialized by the host's account
// service. It is built per call and discarded with the call.
type AccountSelection struct {
	// Ref is what the operator typed (alias or id) — safe to print.
	Ref string
	// AccountID is the stable local id of the selected account.
	AccountID string
	// Category is one of the Account* constants above.
	Category string
	// Endpoint is the connection endpoint (a cloudbox base URL).
	Endpoint string
	// Profile is the connection's profile name (an AWS profile, a cloudbox
	// profile). It is metadata for messages; the credential is below.
	Profile string
	// Token is the single-value credential: a DigitalOcean API token, a
	// cloudbox Bearer token.
	Token string
	// AWSKeys is the explicit key pair for an AWS account's profile.
	AWSKeys awsprovider.Credentials
}

// AccountError is a structured refusal: a stable code and a fixed message,
// never provider text and never credential material.
type AccountError struct {
	Code    string
	Message string
}

func (e *AccountError) Error() string { return "fleet: " + e.Code + ": " + e.Message }

// ErrorCode is how a caller reads the code without a type assertion against
// this package. pkg/accounts.SelectionError implements the same method, so
// one accessor covers refusals raised on either side of the seam.
func (e *AccountError) ErrorCode() string { return e.Code }

func accountFail(code, msg string) error { return &AccountError{Code: code, Message: msg} }

// The stable codes. They mirror pkg/accounts' constants; the strings are the
// contract, not the identifiers.
const (
	accountCodeUnresolved  = "account_unresolved"
	accountCodeUnsupported = "account_unsupported"
	accountCodeConflict    = "account_conflict"
	accountCodeCredential  = "credential_unavailable"
)

// accountCoded is implemented by every structured refusal on either side of
// the seam (fleet.AccountError, accounts.SelectionError).
type accountCoded interface{ ErrorCode() string }

// AccountErrorCode reports the stable code carried by err, or "" when err is
// not a structured account refusal.
func AccountErrorCode(err error) string {
	var c accountCoded
	if errors.As(err, &c) {
		return c.ErrorCode()
	}
	return ""
}

// AccountResolver resolves one account reference to a per-invocation identity.
// It is nil until a host wires the account service in (pkg/accounts/builtin);
// nil means `--account` refuses rather than silently using ambient credentials.
//
// It MUST NOT mutate global state: no environment export, no profile switch.
var AccountResolver func(ctx context.Context, ref string, operation string) (AccountSelection, error)

// accountFlag is the shared `--account` flag. Every verb that takes one binds
// this, so the flag's help, its conflict rules and its category gate are
// written once.
type accountFlag struct{ ref string }

// accountFlagHelp is the one help string, so `bashy infra --help`, the atlas
// flag projection and bashy's MCP schema (all reflected off this cobra flag)
// cannot drift from each other.
const accountFlagHelp = "act as this account alias or id for THIS invocation only (see 'bashy account'); never changes the active provider profile"

func (a *accountFlag) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&a.ref, "account", "", accountFlagHelp)
}

// bindPersistent binds `--account` on a command TREE, so every subcommand
// under a cloudbox front door takes it once.
func (a *accountFlag) bindPersistent(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&a.ref, "account", "", accountFlagHelp)
}

func (a *accountFlag) given() bool { return strings.TrimSpace(a.ref) != "" }

// resolve resolves the flag for one operation, refusing any category not in
// allowed. It returns (nil, nil) when the flag was not given, which is the
// unchanged path.
func (a *accountFlag) resolve(ctx context.Context, operation string, allowed ...string) (*AccountSelection, error) {
	if !a.given() {
		return nil, nil
	}
	if AccountResolver == nil {
		return nil, accountFail(accountCodeUnresolved, "no account service is wired into this binary")
	}
	sel, err := AccountResolver(ctx, strings.TrimSpace(a.ref), operation)
	if err != nil {
		code := AccountErrorCode(err)
		if code == "" {
			code = accountCodeUnresolved
		}
		// The resolver's own message is already code-only; re-wrap it so
		// every refusal from this flag reads the same way.
		return nil, accountFail(code, accountMessage(err))
	}
	if !accountAllowed(sel.Category, allowed) {
		return nil, accountFail(accountCodeUnsupported,
			fmt.Sprintf("account category %q cannot be used here (want %s)", sel.Category, strings.Join(allowed, " or ")))
	}
	if err := accountEnvConflict(sel); err != nil {
		return nil, err
	}
	sel.Ref = strings.TrimSpace(a.ref)
	return &sel, nil
}

// accountMessage is the refusal text with the package-and-code prefix the
// resolver already added stripped off, so re-wrapping it does not stutter
// ("fleet: account_unresolved: accounts: account_unresolved: ...").
func accountMessage(err error) string {
	msg := err.Error()
	if code := AccountErrorCode(err); code != "" {
		if _, rest, found := strings.Cut(msg, code+": "); found {
			return rest
		}
	}
	return msg
}

func accountAllowed(category string, allowed []string) bool {
	for _, a := range allowed {
		if category == a {
			return true
		}
	}
	return false
}

// accountEnvConflict refuses an AWS selection made while the environment
// already names a different explicit identity. Letting both stand would make
// which one wins depend on which code path reads first, and the operator
// asked for exactly one.
func accountEnvConflict(sel AccountSelection) error {
	if sel.Category != AccountAWS {
		return nil
	}
	// "AWS_PROFILE" is external/aws.EnvProfile; it is spelled out rather
	// than imported because external/aws' own in-package tests import THIS
	// package, and an import back would be a test-time cycle.
	for _, name := range []string{awsprovider.EnvAccessKeyID, "AWS_PROFILE"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return accountFail(accountCodeConflict,
				"--account conflicts with $"+name+" in the environment; unset it or drop --account")
		}
	}
	return nil
}

// credentialRefConflict refuses `--account` together with
// `--credential-ref`: they are two different answers to "which credential",
// and guessing is how an operation runs as the wrong identity.
func (a *accountFlag) credentialRefConflict(credentialRef string) error {
	if a.given() && strings.TrimSpace(credentialRef) != "" {
		return accountFail(accountCodeConflict, "--account and --credential-ref both name a credential; pass one")
	}
	return nil
}

// doClient opens a DigitalOcean client for the direct-client verbs (`infra
// ip|firewall|harden|trash`). Without `--account` it is exactly the historical
// environment-only resolution.
func (a *accountFlag) doClient(ctx context.Context) (*digitalocean.Client, error) {
	sel, err := a.resolve(ctx, string(accountOpInfra), AccountDigitalOcean)
	if err != nil {
		return nil, err
	}
	return resolveDOClientFor(sel)
}

// accountOpInfra is the operation name the infra verbs select for. It matches
// accounts.OpInfra; see the category comment above for why it is not imported.
const accountOpInfra = "infra"

// resolveDOClientFor is resolveDOClient with an optional per-invocation
// identity. The account's token replaces the environment's — it does not
// extend a ladder, because a resolved account that cannot be used must refuse
// rather than fall through to whatever token the shell happens to carry.
func resolveDOClientFor(sel *AccountSelection) (*digitalocean.Client, error) {
	token := ""
	if sel != nil {
		if strings.TrimSpace(sel.Token) == "" {
			return nil, accountFail(accountCodeCredential, "account "+sel.Ref+" has no usable DigitalOcean token")
		}
		token = sel.Token
	} else {
		// No account: the historical environment-only resolution, unchanged.
		token = os.Getenv("DIGITALOCEAN_ACCESS_TOKEN")
		if token == "" {
			return nil, errors.New("digitalocean: DIGITALOCEAN_ACCESS_TOKEN environment variable is required")
		}
	}
	baseURL := os.Getenv("DIGITALOCEAN_API_URL")
	if baseURL == "" {
		baseURL = os.Getenv("DIGITALOCEAN_BASE_URL")
	}
	if baseURL == "" {
		baseURL = digitalocean.DefaultBaseURL
	}
	return digitalocean.NewClient(digitalocean.ClientConfig{
		BaseURL: baseURL,
		Token:   token,
	})
}

// accountOpCloudbox is the operation name the cloudbox-facing verbs select
// for (accounts.OpCloudbox).
const accountOpCloudbox = "cloudbox"

// cloudOverrides turns an optional `--account` into the (url, token) pair every
// cloudbox client constructor in this package already accepts.
//
// Doing it as OVERRIDES rather than as a new constructor parameter is
// deliberate: an account IS an explicit endpoint plus an explicit bearer for
// one call, which is exactly what those parameters mean, so the clients, their
// exported signatures and their consumers (bashy, outpost) are untouched.
// Without the flag the arguments pass through unchanged, so the ladder runs
// exactly once and exactly as before.
func cloudOverrides(ctx context.Context, flag *accountFlag, urlOverride, tokenOverride string) (string, string, error) {
	if flag == nil || !flag.given() {
		return urlOverride, tokenOverride, nil
	}
	sel, err := flag.resolve(ctx, accountOpCloudbox, AccountCloudbox, AccountSatellite)
	if err != nil {
		return "", "", err
	}
	return ResolveCloudAccount(sel, urlOverride, tokenOverride)
}

// ResolveCloudAccount is ResolveCloud under an explicit account selection.
//
// With no selection it IS ResolveCloud, ladder and all. With one, the ladder
// is gone on purpose: the account names the endpoint and the bearer, and
// nothing else may answer. An explicit --url/--token/--master alongside
// `--account` is a conflict, not a precedence question.
func ResolveCloudAccount(sel *AccountSelection, urlOverride, tokenOverride string) (base, token string, err error) {
	if sel == nil {
		base, token = ResolveCloud(urlOverride, tokenOverride)
		return base, token, nil
	}
	if strings.TrimSpace(urlOverride) != "" || strings.TrimSpace(tokenOverride) != "" {
		return "", "", accountFail(accountCodeConflict,
			"--account already names the cloudbox endpoint and token; drop --url/--token/--master")
	}
	if sel.Category != AccountCloudbox && sel.Category != AccountSatellite {
		return "", "", accountFail(accountCodeUnsupported,
			fmt.Sprintf("account category %q is not a cloudbox connection", sel.Category))
	}
	base = strings.TrimRight(strings.TrimSpace(sel.Endpoint), "/")
	if base == "" {
		return "", "", accountFail(accountCodeUnresolved, "account "+sel.Ref+" has no cloudbox endpoint")
	}
	if strings.TrimSpace(sel.Token) == "" {
		return "", "", accountFail(accountCodeCredential, "account "+sel.Ref+" has no usable cloudbox token")
	}
	return base, sel.Token, nil
}

// The cloudbox client constructors under one account. Each is NewXClient with
// cloudOverrides in front of it, so a verb gains `--account` by calling the
// For variant and nothing about the client itself changes.

func newCredentialClientFor(ctx context.Context, acct *accountFlag, urlOverride, tokenOverride string) (CredentialClient, error) {
	u, t, err := cloudOverrides(ctx, acct, urlOverride, tokenOverride)
	if err != nil {
		return CredentialClient{}, err
	}
	return NewCredentialClient(u, t)
}

func newProvisioningClientFor(ctx context.Context, acct *accountFlag, urlOverride, tokenOverride string) (ProvisioningClient, error) {
	u, t, err := cloudOverrides(ctx, acct, urlOverride, tokenOverride)
	if err != nil {
		return ProvisioningClient{}, err
	}
	return NewProvisioningClient(u, t)
}

func newTokenClientFor(ctx context.Context, acct *accountFlag, urlOverride, tokenOverride string) (TokenClient, error) {
	u, t, err := cloudOverrides(ctx, acct, urlOverride, tokenOverride)
	if err != nil {
		return TokenClient{}, err
	}
	return NewTokenClient(u, t)
}

func newPairedHostsClientFor(ctx context.Context, acct *accountFlag, urlOverride, tokenOverride string) (PairedHostsClient, error) {
	u, t, err := cloudOverrides(ctx, acct, urlOverride, tokenOverride)
	if err != nil {
		return PairedHostsClient{}, err
	}
	return NewPairedHostsClient(u, t)
}
