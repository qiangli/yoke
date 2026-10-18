// Explicit account/project selection (Sprint 457 story c512b342864d).
//
// Every invocation names its target (org / account / workspace) and exactly
// one credential file explicitly. There is no ambient fallback: no CLI
// default org, no logged-in profile, no environment-wide API key. Whether the
// target is a sandbox/test target must be declared by the caller, and the
// Stripe key prefix is additionally checked against that declaration.
// Operations with real-world effect (payments, messages) are refused unless
// the scope is a declared sandbox.
package saasclients

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/external/gws"
)

type scopeSpec struct {
	Prefix  string // env prefix
	Target  string // env suffix naming the account/org/workspace
	CredEnv string // env suffix naming the credential file
	What    string // human noun for the target
}

var scopeSpecs = map[string]scopeSpec{
	"sf":     {"SF", "TARGET_ORG", "AUTH_URL_FILE", "org alias or username"},
	"stripe": {"STRIPE", "ACCOUNT", "API_KEY_FILE", "account id (acct_…)"},
	"twilio": {"TWILIO", "ACCOUNT_SID", "API_KEY_FILE", "account SID (AC…)"},
	"slack":  {"SLACK", "WORKSPACE", "TOKEN_FILE", "workspace team id (T…)"},
}

// Scope is a fully explicit target + credential selection.
type Scope struct {
	Client         string
	Target         string
	CredentialFile string
	// Sandbox is the caller's declaration (<PREFIX>_SANDBOX=1) that the target
	// is a sandbox/scratch/test target. It is never inferred from the name.
	Sandbox bool
}

// EnvName returns the full env var for a client's scope field ("TARGET",
// "CREDENTIAL", "SANDBOX").
func EnvName(client, field string) string {
	s := scopeSpecs[client]
	switch field {
	case "TARGET":
		return s.Prefix + "_" + s.Target
	case "CREDENTIAL":
		return s.Prefix + "_" + s.CredEnv
	}
	return s.Prefix + "_" + field
}

// ResolveScope builds a Scope from explicit environment only.
func ResolveScope(client string, getenv func(string) string) (Scope, *gws.Error) {
	s, ok := scopeSpecs[client]
	if !ok {
		return Scope{}, gws.NewError(gws.CodeInvalidInput, client+": not an admitted client", nil)
	}
	target := strings.TrimSpace(getenv(EnvName(client, "TARGET")))
	if target == "" {
		return Scope{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: no target selected — set %s to the %s explicitly; there is no ambient-account fallback", client, EnvName(client, "TARGET"), s.What), nil)
	}
	file := strings.TrimSpace(getenv(EnvName(client, "CREDENTIAL")))
	if file == "" {
		return Scope{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: no credential selected — set %s to a credential file (secrets never ride argv or inline env)", client, EnvName(client, "CREDENTIAL")), nil)
	}
	fi, err := os.Stat(file)
	if err != nil {
		return Scope{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: credential file %q unreadable: %v", client, filepath.Base(file), pathErr(err)), nil)
	}
	if fi.IsDir() {
		return Scope{}, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: credential file %q is a directory", client, filepath.Base(file)), nil)
	}
	sc := Scope{Client: client, Target: target, CredentialFile: file, Sandbox: getenv(EnvName(client, "SANDBOX")) == "1"}
	if client == "stripe" {
		if e := sc.checkStripeKey(); e != nil {
			return Scope{}, e
		}
		sc.Sandbox = true // a test-mode key is a sandbox by construction
	}
	return sc, nil
}

func pathErr(err error) string {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err.Error()
	}
	return err.Error()
}

// checkStripeKey cross-checks the declaration against the key prefix without
// retaining the key: a live key (sk_live_/rk_live_) is never accepted, and a
// sandbox declaration needs a test key.
func (s Scope) checkStripeKey() *gws.Error {
	b, err := os.ReadFile(s.CredentialFile)
	if err != nil {
		return gws.NewError(gws.CodeMissingCredentials, "stripe: credential file unreadable", nil)
	}
	k := strings.TrimSpace(string(b))
	switch {
	case strings.HasPrefix(k, "sk_live_"), strings.HasPrefix(k, "rk_live_"):
		return gws.NewError(gws.CodePermissionDenied, "stripe: live-mode key refused — only test-mode keys are admitted", nil)
	case strings.HasPrefix(k, "sk_test_"), strings.HasPrefix(k, "rk_test_"):
		return nil
	}
	return gws.NewError(gws.CodeInvalidInput, "stripe: credential file does not hold a test-mode secret/restricted key (sk_test_/rk_test_)", nil)
}

// RedactedScope reports the resolved scope without secret material.
func (s Scope) RedactedScope() string {
	return fmt.Sprintf("client=%s target=%s credentials=file:%s sandbox=%t", s.Client, s.Target, filepath.Base(s.CredentialFile), s.Sandbox)
}
