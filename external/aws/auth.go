// Explicit credential selection for aws (Sprint 457 story bedc320542eb).
//
// Every aws invocation requires an explicit profile (AWS_PROFILE): there
// is no ambient default-profile fallback. The profile's credential source
// is exactly one of a shared credentials file (AWS_SHARED_CREDENTIALS_FILE
// or ~/.aws/credentials, which must exist as a file) or an explicit env
// key pair (AWS_ACCESS_KEY_ID plus AWS_SECRET_ACCESS_KEY, presence only).
// The region (AWS_REGION / AWS_DEFAULT_REGION) is optional and always
// reported in the redacted scope. Missing or ambiguous credentials fail
// promptly with a typed error (errors.go).
//
// Credential material lives in `bashy secret` / explicit env files owned
// outside this package: this package never reads key values beyond
// presence, never stores secrets, and hands only profile/file selection to
// the aws child. Redaction of argv/diagnostics is in redact.go.
//
// A resolved credential proves authentication only. Provisioning
// permission is never inferred from login: ProvisioningPermitted always
// refuses offline and names the live Sprint 465 check.
package aws

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AuthKind names the credential mechanism.
type AuthKind string

const (
	// AuthSharedFile is a profile in the shared credentials/config files.
	AuthSharedFile AuthKind = "shared-file"
	// AuthEnvKeys is an explicit env key pair.
	AuthEnvKeys AuthKind = "env-keys"
)

// Env names for explicit credential selection.
const (
	EnvProfile           = "AWS_PROFILE"
	EnvSharedCredentials = "AWS_SHARED_CREDENTIALS_FILE"
	EnvAccessKeyID       = "AWS_ACCESS_KEY_ID"
	EnvSecretAccessKey   = "AWS_SECRET_ACCESS_KEY"
	EnvRegion            = "AWS_REGION"
	EnvDefaultRegion     = "AWS_DEFAULT_REGION"
)

// Credentials is a fully explicit credential selection: which profile, by
// which mechanism, in which region.
type Credentials struct {
	Profile         string
	Kind            AuthKind
	CredentialsFile string
	Region          string
}

// defaultSharedFile is the SDK's standard shared-credentials location.
func defaultSharedFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".aws", "credentials")
}

// ResolveCredentials builds Credentials from explicit environment only.
// AWS_PROFILE is required. Exactly one credential source must be
// present: a credentials file (explicit or default, must exist) or an env
// key pair (both key id and secret present).
func ResolveCredentials() (Credentials, error) {
	profile := strings.TrimSpace(os.Getenv(EnvProfile))
	if profile == "" {
		return Credentials{}, NewError(CodeMissingCredentials, "aws: no profile selected — set "+EnvProfile+" explicitly; there is no ambient default-profile fallback", nil)
	}
	file := strings.TrimSpace(os.Getenv(EnvSharedCredentials))
	if file == "" {
		file = defaultSharedFile()
	}
	fileOK := false
	if file != "" {
		if fi, err := os.Stat(file); err == nil && !fi.IsDir() {
			fileOK = true
		}
	}
	keyID := strings.TrimSpace(os.Getenv(EnvAccessKeyID))
	secret := os.Getenv(EnvSecretAccessKey)
	keysOK := keyID != "" && secret != ""
	switch {
	case fileOK && keysOK:
		return Credentials{}, NewError(CodeInvalidInput, "aws: ambiguous credentials for profile "+profile+" — use the shared file or the env key pair, not both (unset one)", nil)
	case fileOK:
		// ok
	case keysOK:
		// ok
	default:
		return Credentials{}, NewError(CodeMissingCredentials, fmt.Sprintf("aws: no credential material for profile %q — provide %s (or %s) or set %s+%s explicitly", profile, EnvSharedCredentials, defaultSharedFile(), EnvAccessKeyID, EnvSecretAccessKey), nil)
	}
	c := Credentials{Profile: profile}
	if fileOK && !keysOK {
		c.Kind = AuthSharedFile
		c.CredentialsFile = file
	} else {
		c.Kind = AuthEnvKeys
	}
	if r := strings.TrimSpace(os.Getenv(EnvRegion)); r != "" {
		c.Region = r
	} else {
		c.Region = strings.TrimSpace(os.Getenv(EnvDefaultRegion))
	}
	return c, nil
}

// RedactedScope reports the resolved scope with secret material removed:
// profile, auth kind, and the credential file's basename (or "env-keys"
// when the key pair came from the environment) plus the region or
// "default-chain". Safe for stdout/stderr diagnostics, errors, previews,
// logs, and MCP exposure.
func (c Credentials) RedactedScope() string {
	src := "env-keys"
	if c.CredentialsFile != "" {
		src = "file:" + filepath.Base(c.CredentialsFile)
	}
	region := c.Region
	if region == "" {
		region = "default-chain"
	}
	return fmt.Sprintf("profile=%s auth=%s credentials=%s region=%s", c.Profile, c.Kind, src, region)
}

// ProvisioningPermitted never infers write/provisioning permission from a
// successful login: authentication proves identity, not authorization. The
// live authorization check needs a real account and is owned by Sprint 465,
// so this refuses offline even when credentials resolve.
func ProvisioningPermitted(c Credentials) *Error {
	return NewError(CodePermissionDenied, fmt.Sprintf("aws: provisioning permission unknown offline for %s — login proves identity, not authorization; live check owned by Sprint 465", c.RedactedScope()), nil)
}
