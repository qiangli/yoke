// Explicit credential selection for glab (Sprint 457 story bedc320542eb).
//
// Every glab invocation requires explicit credential material: exactly one
// of GITLAB_TOKEN (env value) or GITLAB_TOKEN_FILE (path to a file holding
// the token). The target host is explicit or the documented default
// (GITLAB_HOST, default https://gitlab.com) and is always reported in the
// redacted scope. There is no ambient-account fallback: missing or
// ambiguous credentials fail promptly with a typed error (errors.go), and
// the resolved scope is reported redacted (host + auth kind + token file
// basename; never token material).
//
// Credential material lives in `bashy secret` / explicit env files owned
// outside this package: this package never reads token values beyond
// presence, never stores secrets, and hands only paths to the glab child.
// Redaction of argv/diagnostics is in redact.go.
//
// A resolved credential proves authentication only. Provisioning permission
// is never inferred from login: ProvisioningPermitted always refuses
// offline and names the live Sprint 465 check.
package glab

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AuthKind names the credential mechanism.
type AuthKind string

const (
	// AuthToken is a personal / project / group access token.
	AuthToken AuthKind = "token"
)

// Env names for explicit credential selection.
const (
	EnvToken     = "GITLAB_TOKEN"
	EnvTokenFile = "GITLAB_TOKEN_FILE"
	EnvHost      = "GITLAB_HOST"
	EnvProfile   = "GLAB_PROFILE"
)

// Credentials is a fully explicit credential selection: where (Host) and
// with what (exactly one token source), and by which mechanism.
type Credentials struct {
	Host      string
	Kind      AuthKind
	TokenFile string
	// HasTokenEnv is set when the token came from GITLAB_TOKEN directly.
	HasTokenEnv bool
	// Profile is set only when selection came through a named profile that
	// resolved these choices unambiguously; it sets presentation defaults,
	// never authority.
	Profile string
}

// ResolveCredentials builds Credentials from explicit environment only.
// It fails when no token source is set, when both are set, or when the
// selected file does not exist. GITLAB_HOST defaults to HostDefault and is
// always echoed in the redacted scope.
func ResolveCredentials() (Credentials, error) {
	token := strings.TrimSpace(os.Getenv(EnvToken))
	tokenFile := strings.TrimSpace(os.Getenv(EnvTokenFile))
	if token == "" && tokenFile == "" {
		return Credentials{}, NewError(CodeMissingCredentials, "glab: no token selected — set exactly one of "+EnvToken+" or "+EnvTokenFile+"; there is no ambient-account fallback", nil)
	}
	if token != "" && tokenFile != "" {
		return Credentials{}, NewError(CodeInvalidInput, "glab: ambiguous credentials — set exactly one of "+EnvToken+" or "+EnvTokenFile+", not both", nil)
	}
	c := Credentials{Host: strings.TrimSpace(os.Getenv(EnvHost)), Kind: AuthToken, Profile: strings.TrimSpace(os.Getenv(EnvProfile))}
	if c.Host == "" {
		c.Host = HostDefault
	}
	if token != "" {
		c.HasTokenEnv = true
		return c, nil
	}
	fi, err := os.Stat(tokenFile)
	if err != nil {
		return Credentials{}, NewError(CodeMissingCredentials, fmt.Sprintf("glab: token file %q unreadable: %v", tokenFile, err), nil)
	}
	if fi.IsDir() {
		return Credentials{}, NewError(CodeInvalidInput, fmt.Sprintf("glab: token file %q is a directory, want a file", tokenFile), nil)
	}
	c.TokenFile = tokenFile
	return c, nil
}

// RedactedScope reports the resolved scope with secret material removed:
// host, auth kind, and the token file's basename (or "env" when the token
// came from the environment). Safe for stdout/stderr diagnostics, errors,
// previews, logs, and MCP exposure.
func (c Credentials) RedactedScope() string {
	src := "env"
	if c.TokenFile != "" {
		src = "file:" + filepath.Base(c.TokenFile)
	}
	scope := fmt.Sprintf("host=%s auth=%s token=%s", c.Host, c.Kind, src)
	if c.Profile != "" {
		scope += " profile=" + c.Profile
	}
	return scope
}

// ProvisioningPermitted never infers write/provisioning permission from a
// successful login: authentication proves identity, not authorization. The
// live authorization check needs a real account and is owned by Sprint 465,
// so this refuses offline even when credentials resolve.
func ProvisioningPermitted(c Credentials) *Error {
	return NewError(CodePermissionDenied, fmt.Sprintf("glab: provisioning permission unknown offline for %s — login proves identity, not authorization; live check owned by Sprint 465", c.RedactedScope()), nil)
}
