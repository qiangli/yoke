package accounts

// Per-invocation credential materialization — the shared half of `--account`
// (S524.7).
//
// Every family that takes `--account` (DigitalOcean, AWS, Cloudbox, LLM) needs
// the same two steps and must not spell either of them twice: resolve the
// account to ONE connection for the operation at hand, then turn that
// connection's credential REFERENCE into a value that lives for exactly this
// call. Both steps are here.
//
// The rules this file exists to enforce:
//
//   - The value is RETURNED, never installed. Nothing here calls os.Setenv,
//     writes a file, switches a provider CLI's active profile, or prints a
//     credential. A caller hands it to one client or one child process.
//   - There is NO FALLBACK to another identity. A reference that cannot be
//     resolved is a refusal, never "try the ambient credential instead" —
//     silently acting as a different account is the one outcome `--account`
//     exists to prevent.
//   - Failures carry a CODE and no provider text (SelectionError), so an
//     error message cannot become a credential leak.
//   - Reads of foreign credential stores are READ-ONLY. The cloudbox profile
//     directory belongs to outpost; we read one access token out of it and
//     never write, re-order or activate anything.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	awsauth "github.com/qiangli/yoke/external/aws"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/secrets"
)

// Stable error codes for an explicit selection that cannot be honored. They
// are the contract an agent parses; the message is for a person.
const (
	// CodeUnresolved: no such account, or it is not resolvable for this operation.
	CodeUnresolved = "account_unresolved"
	// CodeUnsupported: this account's category cannot drive this verb.
	CodeUnsupported = "account_unsupported"
	// CodeAmbiguous: the account has more than one usable connection/binding.
	CodeAmbiguous = "account_ambiguous"
	// CodeConflict: --account was given together with an explicit, conflicting target.
	CodeConflict = "account_conflict"
	// CodeCredentialMissing: the selected connection carries no credential reference.
	CodeCredentialMissing = "credential_missing"
	// CodeCredentialUnsupported: the reference kind cannot be used for this family.
	CodeCredentialUnsupported = "credential_unsupported"
	// CodeCredentialUnavailable: the reference is right but the value could not be fetched.
	CodeCredentialUnavailable = "credential_unavailable"
)

// The operation vocabulary `--account` front doors pass to SelectFor and to
// `account resolve --for`. One list so a host and an adapter cannot disagree
// about the name of the thing being selected for.
const (
	OpConnect  Operation = "connect"  // bashy connect (host login)
	OpInfra    Operation = "infra"    // bashy infra * (DigitalOcean, AWS)
	OpCloudbox Operation = "cloudbox" // cloudbox-facing endpoint + bearer
	OpSecret   Operation = "secret"   // bashy secret (vault endpoint + bearer)
	OpDelegate Operation = "delegate" // bashy delegate (LLM seat)
)

// SelectionError is a structured refusal: a stable code and a fixed message.
// It deliberately carries no provider error text, because provider text is
// the one place a credential could ride out of this package.
type SelectionError struct {
	Code    string
	Message string
}

func (e *SelectionError) Error() string { return "accounts: " + e.Code + ": " + e.Message }

// ErrorCode lets a package that CANNOT import this one read the code off the
// error. pkg/fleet and pkg/secrets are both imported by this package's
// dependency closure, so they reach the code through this method rather than
// through a type assertion.
func (e *SelectionError) ErrorCode() string { return e.Code }

func selectionError(code, msg string) *SelectionError {
	return &SelectionError{Code: code, Message: msg}
}

// ErrorCode reports the stable code of a selection refusal, or "" when err is
// not one.
func ErrorCode(err error) string {
	var se *SelectionError
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

// SelectFor resolves one account reference (alias or id) to a single
// connection for one operation. It is the ONE entry point every `--account`
// front door uses, so no family can invent its own notion of "which
// connection did the operator mean".
//
// The owning adapter decides when it declares Select; otherwise the store's
// own single-connection rule applies. Either way the result is safe metadata:
// a reference, never a value.
func SelectFor(ctx context.Context, s *Store, r *Registry, ref string, op Operation) (Account, Selection, error) {
	if s == nil {
		return Account{}, Selection{}, selectionError(CodeUnresolved, "no account store")
	}
	acct, err := s.Get(ref)
	if err != nil {
		return Account{}, Selection{}, selectionError(CodeUnresolved, "account not found")
	}
	if acct.Status == Suppressed {
		return acct, Selection{}, selectionError(CodeUnresolved, "account is removed")
	}
	if r != nil && acct.Provenance != "" {
		if a, aerr := r.Adapter(acct.Provenance, Select); aerr == nil {
			sel, serr := a.Select(ctx, acct, op)
			return acct, sel, selectionFailure(serr)
		}
	}
	sel, serr := s.Resolve(acct.ID, op)
	return acct, sel, selectionFailure(serr)
}

// selectionFailure maps the store/adapter sentinels onto the stable codes.
func selectionFailure(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrAmbiguous):
		return selectionError(CodeAmbiguous, "account has more than one connection; none was named")
	case errors.Is(err, ErrNotFound):
		return selectionError(CodeUnresolved, "account not found")
	case errors.Is(err, ErrUnsupported):
		return selectionError(CodeUnsupported, "account is not selectable for this operation")
	default:
		return selectionError(CodeUnresolved, "account could not be resolved")
	}
}

// Credential is one account credential resolved for a SINGLE call. It is
// returned up the stack and handed to one client; it is never stored, printed
// or exported to this process's environment.
type Credential struct {
	// Ref is the reference it came from. Safe to print — it names a lookup,
	// not a value.
	Ref string
	// Kind is the reference scheme: secret, env, profile or ssh-key.
	Kind string
	// Token is the single-value credential: a DigitalOcean API token, a
	// cloudbox Bearer token.
	Token string
	// AWS is the key pair a profile: reference resolved for an AWS account.
	// AWS is the one family whose credential is a PAIR, so it has its own
	// field rather than being squeezed into Token.
	AWS awsprovider.Credentials
}

// Materializer resolves credential references. Every reach outside the
// process is a field, so a test substitutes a fake instead of a real vault,
// environment or profile directory.
type Materializer struct {
	// SecretGet fetches one named secret from the cloudbox vault.
	SecretGet func(name string) (string, error)
	// Getenv reads one environment variable.
	Getenv func(name string) string
	// CloudboxToken reads one cloudbox/outpost profile's Bearer token.
	CloudboxToken func(profile string) (string, error)
	// AWSProfileKeys reads one AWS profile's explicit key pair.
	AWSProfileKeys func(profile string) (awsprovider.Credentials, error)
	// CloudboxDir overrides the outpost profile root (tests).
	CloudboxDir string
}

// doEnvAliases are the two spellings of the DigitalOcean token variable.
// The account adapter discovered $DIGITALOCEAN_TOKEN while pkg/fleet's
// provider path reads $DIGITALOCEAN_ACCESS_TOKEN, so an env: reference to
// either name accepts whichever one the host actually set. Listing both in
// one place is what keeps the two halves from disagreeing again.
var doEnvAliases = []string{"DIGITALOCEAN_ACCESS_TOKEN", "DIGITALOCEAN_TOKEN"}

// DigitalOceanEnvNames returns the accepted DigitalOcean token variables in
// precedence order.
func DigitalOceanEnvNames() []string { return append([]string(nil), doEnvAliases...) }

func isDOEnvName(name string) bool {
	for _, n := range doEnvAliases {
		if name == n {
			return true
		}
	}
	return false
}

func (m Materializer) getenv(name string) string {
	get := m.Getenv
	if get == nil {
		get = os.Getenv
	}
	return envCredential(get, name)
}

// envCredential reads one credential variable, accepting either DigitalOcean
// spelling for either DigitalOcean reference.
func envCredential(get func(string) string, name string) string {
	if v := strings.TrimSpace(get(name)); v != "" {
		return v
	}
	if !isDOEnvName(name) {
		return ""
	}
	for _, alias := range doEnvAliases {
		if alias == name {
			continue
		}
		if v := strings.TrimSpace(get(alias)); v != "" {
			return v
		}
	}
	return ""
}

// Materialize turns one selection's credential reference into a value for this
// call. The category decides what a profile: reference MEANS — an AWS profile
// is a key pair in the shared credentials file, a cloudbox profile is a Bearer
// token in the outpost profile directory — so it is a parameter, not a guess.
func (m Materializer) Materialize(_ context.Context, sel Selection, category Category) (Credential, error) {
	ref := strings.TrimSpace(sel.CredentialRef)
	if ref == "" {
		return Credential{}, selectionError(CodeCredentialMissing, "account connection has no credential reference")
	}
	if !ValidCredentialRef(ref) {
		return Credential{}, selectionError(CodeCredentialUnsupported, "credential reference is not a valid reference")
	}
	switch {
	case strings.HasPrefix(ref, "secret:"):
		if m.SecretGet == nil {
			return Credential{}, selectionError(CodeCredentialUnavailable, "no secrets client")
		}
		v, err := m.SecretGet(strings.TrimPrefix(ref, "secret:"))
		if err != nil || strings.TrimSpace(v) == "" {
			return Credential{}, selectionError(CodeCredentialUnavailable, "credential could not be fetched")
		}
		return Credential{Ref: ref, Kind: "secret", Token: v}, nil

	case strings.HasPrefix(ref, "env:"):
		v := m.getenv(strings.TrimPrefix(ref, "env:"))
		if v == "" {
			return Credential{}, selectionError(CodeCredentialUnavailable, "credential variable is not set in this environment")
		}
		return Credential{Ref: ref, Kind: "env", Token: v}, nil

	case strings.HasPrefix(ref, "profile:"):
		name := strings.TrimPrefix(ref, "profile:")
		switch category {
		case AWS:
			resolve := m.AWSProfileKeys
			if resolve == nil {
				resolve = defaultAWSProfileKeys
			}
			keys, err := resolve(name)
			if err != nil || !keys.Valid() {
				return Credential{}, selectionError(CodeCredentialUnavailable, "profile credentials are unavailable")
			}
			return Credential{Ref: ref, Kind: "profile", AWS: keys}, nil
		case Cloudbox, Satellite:
			read := m.CloudboxToken
			if read == nil {
				read = func(p string) (string, error) { return cloudboxProfileToken(m.CloudboxDir, p) }
			}
			v, err := read(name)
			if err != nil || strings.TrimSpace(v) == "" {
				return Credential{}, selectionError(CodeCredentialUnavailable, "profile token is unavailable")
			}
			return Credential{Ref: ref, Kind: "profile", Token: strings.TrimSpace(v)}, nil
		default:
			return Credential{}, selectionError(CodeCredentialUnsupported, "a profile reference is not usable for this account category")
		}

	default:
		// ssh-key: is a real reference kind, but it names a key an SSH
		// client uses — there is no value to hand an API client.
		return Credential{}, selectionError(CodeCredentialUnsupported, "credential reference kind is not supported for this operation")
	}
}

// Materialize resolves one selection with the default, real resolvers.
func Materialize(ctx context.Context, sel Selection, category Category) (Credential, error) {
	return DefaultMaterializer().Materialize(ctx, sel, category)
}

// DefaultMaterializer reaches the real vault, environment and profile dirs.
func DefaultMaterializer() Materializer {
	return Materializer{SecretGet: defaultSecretGet}
}

func defaultSecretGet(name string) (string, error) {
	c, err := (secrets.Config{}).Resolve()
	if err != nil {
		return "", err
	}
	return c.Get(name)
}

// defaultAWSProfileKeys reads the explicitly named shared-file profile and
// nothing else: external/aws refuses role/SSO/credential_process profiles and
// never falls back to [default], which is exactly the no-fallback rule.
func defaultAWSProfileKeys(profile string) (awsprovider.Credentials, error) {
	return awsauth.ResolveProfileKeys(profile)
}

// cloudboxRoots lists the outpost/cloudbox profile roots, newest name first,
// mirroring the discovery adapter's own lookup so a profile that `account
// discover` found is a profile this can read.
func cloudboxRoots(override string) []string {
	if strings.TrimSpace(override) != "" {
		return []string{override}
	}
	base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		base = filepath.Join(h, ".config")
	}
	return []string{filepath.Join(base, "matrix"), filepath.Join(base, "outpost")}
}

// cloudboxProfileToken reads ONE profile's access token out of the outpost
// profile directory. Read-only by construction: it opens agent.json and takes
// a single field. It never activates a profile and never writes.
func cloudboxProfileToken(dirOverride, profile string) (string, error) {
	profile = strings.TrimSpace(profile)
	if profile == "" || strings.ContainsAny(profile, "/\\\x00") {
		return "", errors.New("accounts: invalid profile name")
	}
	for _, root := range cloudboxRoots(dirOverride) {
		// A named profile lives in its own directory; the ACTIVE profile is
		// the root itself, named by .active (or "default" when unnamed).
		candidates := []string{filepath.Join(root, "profiles", profile, "agent.json")}
		active := ""
		if b, err := os.ReadFile(filepath.Join(root, ".active")); err == nil {
			active = strings.TrimSpace(string(b))
		}
		if active == profile || (active == "" && profile == "default") {
			candidates = append(candidates, filepath.Join(root, "agent.json"))
		}
		for _, p := range candidates {
			if tok := readAccessToken(p); tok != "" {
				return tok, nil
			}
		}
	}
	return "", errors.New("accounts: no token for profile")
}

func readAccessToken(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.AccessToken)
}
