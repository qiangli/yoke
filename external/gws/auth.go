// Explicit credential selection for gws (Sprint 457 story f25d7aa4437c).
//
// Every gws invocation requires an explicit account and explicit credential
// material: OAuth client credentials or a service-account key, selected via
// environment (GWS_ACCOUNT plus exactly one of GWS_CREDENTIALS_FILE /
// GWS_SERVICE_ACCOUNT_KEY_FILE) or an unambiguous named profile. There is
// no ambient-account fallback and no production default: missing or
// ambiguous credentials fail promptly with a typed error (errors.go), and
// the resolved scope is reported redacted (account + auth kind + credential
// file basename; never key material).
//
// This package never reads key material itself — it validates that the
// selected file exists and is file-shaped, then hands the *path* to the
// gws child. Redaction of argv/diagnostics is in redact.go.
package gws

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AuthKind names the credential mechanism.
type AuthKind string

const (
	AuthOAuth          AuthKind = "oauth"
	AuthServiceAccount AuthKind = "service-account"
)

// Env names for explicit credential selection.
const (
	EnvAccount            = "GWS_ACCOUNT"
	EnvAuthKind           = "GWS_AUTH_KIND"
	EnvCredentialsFile    = "GWS_CREDENTIALS_FILE"
	EnvServiceAccountFile = "GWS_SERVICE_ACCOUNT_KEY_FILE"
	EnvProfile            = "GWS_PROFILE"
)

// Credentials is a fully explicit credential selection: who (Account) and
// with what (exactly one credential file), and by which mechanism.
type Credentials struct {
	Account         string
	Kind            AuthKind
	CredentialsFile string
	// Profile is set only when selection came through a named profile that
	// resolved these choices unambiguously.
	Profile string
}

// ResolveCredentials builds Credentials from explicit environment only.
// It fails when the account is missing, when neither/nor both credential
// files are set, when the auth kind is unknown, or when the selected file
// does not exist. A named profile (GWS_PROFILE) is accepted only together
// with the same explicit account/file choices — it may set presentation
// defaults, never authority.
func ResolveCredentials() (Credentials, error) {
	account := strings.TrimSpace(os.Getenv(EnvAccount))
	if account == "" {
		return Credentials{}, NewError(CodeMissingCredentials, "gws: no account selected — set "+EnvAccount+" explicitly; there is no ambient-account fallback", nil)
	}
	oauthFile := strings.TrimSpace(os.Getenv(EnvCredentialsFile))
	saFile := strings.TrimSpace(os.Getenv(EnvServiceAccountFile))
	if oauthFile == "" && saFile == "" {
		return Credentials{}, NewError(CodeMissingCredentials, "gws: no credential material selected — set exactly one of "+EnvCredentialsFile+" (OAuth) or "+EnvServiceAccountFile+" (service account)", nil)
	}
	if oauthFile != "" && saFile != "" {
		return Credentials{}, NewError(CodeInvalidInput, "gws: ambiguous credentials — set exactly one of "+EnvCredentialsFile+" or "+EnvServiceAccountFile+", not both", nil)
	}
	kind := AuthKind(strings.TrimSpace(os.Getenv(EnvAuthKind)))
	file := oauthFile
	if saFile != "" {
		file = saFile
		if kind == "" {
			kind = AuthServiceAccount
		} else if kind != AuthServiceAccount {
			return Credentials{}, NewError(CodeInvalidInput, "gws: "+EnvAuthKind+"="+string(kind)+" conflicts with "+EnvServiceAccountFile+" (want service-account)", nil)
		}
	} else if kind == "" {
		kind = AuthOAuth
	} else if kind != AuthOAuth {
		return Credentials{}, NewError(CodeInvalidInput, "gws: "+EnvAuthKind+"="+string(kind)+" conflicts with "+EnvCredentialsFile+" (want oauth)", nil)
	}
	fi, err := os.Stat(file)
	if err != nil {
		return Credentials{}, NewError(CodeMissingCredentials, fmt.Sprintf("gws: credential file %q unreadable: %v", file, err), nil)
	}
	if fi.IsDir() {
		return Credentials{}, NewError(CodeInvalidInput, fmt.Sprintf("gws: credential file %q is a directory, want a file", file), nil)
	}
	return Credentials{
		Account:         account,
		Kind:            kind,
		CredentialsFile: file,
		Profile:         strings.TrimSpace(os.Getenv(EnvProfile)),
	}, nil
}

// RedactedScope reports the resolved scope with secret material removed:
// account, auth kind, and the credential file's basename only. Safe for
// stdout/stderr diagnostics, errors, previews, logs, and MCP exposure.
func (c Credentials) RedactedScope() string {
	scope := fmt.Sprintf("account=%s auth=%s credentials=file:%s", c.Account, c.Kind, filepath.Base(c.CredentialsFile))
	if c.Profile != "" {
		scope += " profile=" + c.Profile
	}
	return scope
}
