// Explicit credential selection for az (Sprint 457 story bedc320542eb).
//
// Every az invocation requires an explicit subscription
// (AZURE_SUBSCRIPTION_ID, id or name): there is no ambient-subscription
// fallback — `az login` alone selects nothing. The credential source is
// exactly one of a service-principal secret file
// (AZURE_CLIENT_SECRET_FILE, which must exist as a file) or an explicit
// declaration of an existing CLI login session (AZURE_USE_CLI_LOGIN=1).
// Missing or ambiguous credentials fail promptly with a typed error
// (errors.go), and the resolved scope is reported redacted (subscription +
// auth kind + secret file basename; never secret material).
//
// Credential material lives in `bashy secret` / explicit env files owned
// outside this package: this package never reads secret values beyond
// presence, never stores secrets, and hands only subscription selection to
// the az child. Redaction of argv/diagnostics is in redact.go.
//
// A resolved credential proves authentication only. Provisioning
// permission is never inferred from login: ProvisioningPermitted always
// refuses offline and names the live Sprint 465 check.
package az

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AuthKind names the credential mechanism.
type AuthKind string

const (
	// AuthServicePrincipal is a service-principal secret file.
	AuthServicePrincipal AuthKind = "service-principal"
	// AuthCLILogin is an explicitly declared existing `az login` session.
	AuthCLILogin AuthKind = "cli-login"
)

// Env names for explicit credential selection.
const (
	EnvSubscription = "AZURE_SUBSCRIPTION_ID"
	EnvSecretFile   = "AZURE_CLIENT_SECRET_FILE"
	EnvUseCLILogin  = "AZURE_USE_CLI_LOGIN"
	EnvTenant       = "AZURE_TENANT_ID"
	EnvProfile      = "AZ_PROFILE"
)

// Credentials is a fully explicit credential selection: which
// subscription, by which mechanism.
type Credentials struct {
	Subscription string
	Kind         AuthKind
	SecretFile   string
	Tenant       string
	// Profile sets presentation defaults only, never authority.
	Profile string
}

// ResolveCredentials builds Credentials from explicit environment only.
// AZURE_SUBSCRIPTION_ID is required. Exactly one credential source must be
// present: a secret file (must exist) or AZURE_USE_CLI_LOGIN=1 declaring
// an existing login session.
func ResolveCredentials() (Credentials, error) {
	sub := strings.TrimSpace(os.Getenv(EnvSubscription))
	if sub == "" {
		return Credentials{}, NewError(CodeMissingCredentials, "az: no subscription selected — set "+EnvSubscription+" explicitly (id or name); `az login` alone selects nothing", nil)
	}
	if strings.ContainsAny(sub, " \t\n") {
		return Credentials{}, NewError(CodeInvalidInput, "az: subscription must be a single id or name, without whitespace", nil)
	}
	secretFile := strings.TrimSpace(os.Getenv(EnvSecretFile))
	useLogin := strings.TrimSpace(os.Getenv(EnvUseCLILogin)) == "1"
	switch {
	case secretFile != "" && useLogin:
		return Credentials{}, NewError(CodeInvalidInput, "az: ambiguous credentials — use "+EnvSecretFile+" or "+EnvUseCLILogin+"=1, not both", nil)
	case secretFile != "":
		fi, err := os.Stat(secretFile)
		if err != nil {
			return Credentials{}, NewError(CodeMissingCredentials, fmt.Sprintf("az: secret file %q unreadable: %v", secretFile, err), nil)
		}
		if fi.IsDir() {
			return Credentials{}, NewError(CodeInvalidInput, fmt.Sprintf("az: secret file %q is a directory, want a file", secretFile), nil)
		}
	case useLogin:
		// ok: the operator explicitly declares the existing session
	default:
		return Credentials{}, NewError(CodeMissingCredentials, "az: no credential material selected — set "+EnvSecretFile+" or "+EnvUseCLILogin+"=1 explicitly", nil)
	}
	c := Credentials{
		Subscription: sub,
		SecretFile:   secretFile,
		Tenant:       strings.TrimSpace(os.Getenv(EnvTenant)),
		Profile:      strings.TrimSpace(os.Getenv(EnvProfile)),
	}
	if secretFile != "" {
		c.Kind = AuthServicePrincipal
	} else {
		c.Kind = AuthCLILogin
	}
	return c, nil
}

// RedactedScope reports the resolved scope with secret material removed:
// subscription, auth kind, and the secret file's basename (or "cli-login"
// session when declared). Safe for stdout/stderr diagnostics, errors,
// previews, logs, and MCP exposure.
func (c Credentials) RedactedScope() string {
	src := "cli-login-session"
	if c.SecretFile != "" {
		src = "file:" + filepath.Base(c.SecretFile)
	}
	scope := fmt.Sprintf("subscription=%s auth=%s credentials=%s", c.Subscription, c.Kind, src)
	if c.Tenant != "" {
		scope += " tenant=" + c.Tenant
	}
	if c.Profile != "" {
		scope += " profile=" + c.Profile
	}
	return scope
}

// ProvisioningPermitted never infers write/provisioning permission from a
// successful login: authentication proves identity, not authorization. The
// live authorization check needs a real subscription and is owned by
// Sprint 465, so this refuses offline even when credentials resolve.
func ProvisioningPermitted(c Credentials) *Error {
	return NewError(CodePermissionDenied, fmt.Sprintf("az: provisioning permission unknown offline for %s — login proves identity, not authorization; live check owned by Sprint 465", c.RedactedScope()), nil)
}
