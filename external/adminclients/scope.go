// Explicit tenant/domain, credential and scope selection (story
// 66494446f180). There is no ambient fallback: no CLI default tenant, no
// logged-in profile. m365 names its tenant, a credential file and its Graph
// scopes; gam names its domain and REUSES gws's credential selection
// (GWS_ACCOUNT + exactly one gws credential file) — this package keeps no
// credential store of its own. Mutations require a declared test
// tenant/domain.
package adminclients

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qiangli/yoke/external/gws"
)

// Env names. GAM credentials are gws's (gws.EnvAccount, gws.EnvCredentialsFile,
// gws.EnvServiceAccountFile).
const (
	EnvM365Tenant  = "M365_TENANT"
	EnvM365Auth    = "M365_AUTH_FILE"
	EnvM365Scopes  = "M365_SCOPES"
	EnvM365Sandbox = "M365_SANDBOX"
	EnvGAMDomain   = "GAM_DOMAIN"
	EnvGAMScopes   = "GAM_SCOPES"
	EnvGAMSandbox  = "GAM_SANDBOX"
)

var (
	guidRE   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	domainRE = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$`)
)

// Scope is a fully explicit target + credential + permission selection.
type Scope struct {
	Client string
	// Target is the tenant id/domain (m365) or Workspace domain (gam).
	Target         string
	CredentialFile string
	// Account is the gam delegated admin (the gws account); empty for m365.
	Account string
	// Scopes are the declared least-privilege permissions: Graph
	// permissions (m365) or Admin SDK OAuth scopes (gam).
	Scopes []string
	// Sandbox is the caller's declaration that Target is a test
	// tenant/domain; never inferred from the name.
	Sandbox bool
}

func splitScopes(v string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, s)
	}
	return out
}

func checkFile(client, label, file string) *gws.Error {
	fi, err := os.Stat(file)
	if err != nil {
		return gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: %s file %q unreadable", client, label, filepath.Base(file)), nil)
	}
	if fi.IsDir() {
		return gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: %s file %q is a directory", client, label, filepath.Base(file)), nil)
	}
	return nil
}

// ResolveScope builds a Scope from explicit environment. getenv serves the
// tenant/domain/scope/sandbox values; gam credentials come from
// gws.ResolveCredentials (process environment), the single credential
// selection shared with gws.
func ResolveScope(client string, getenv func(string) string) (Scope, *gws.Error) {
	switch client {
	case "m365":
		t := strings.TrimSpace(getenv(EnvM365Tenant))
		if t == "" {
			return Scope{}, gws.NewError(gws.CodeMissingCredentials, "m365: no tenant selected — set "+EnvM365Tenant+" to the tenant id or verified domain; there is no ambient-tenant fallback", nil)
		}
		if !guidRE.MatchString(t) && !domainRE.MatchString(t) {
			return Scope{}, gws.NewError(gws.CodeInvalidInput, "m365: "+EnvM365Tenant+" must be a tenant GUID or a DNS domain (not 'common'/'organizations')", nil)
		}
		f := strings.TrimSpace(getenv(EnvM365Auth))
		if f == "" {
			return Scope{}, gws.NewError(gws.CodeMissingCredentials, "m365: no credential selected — set "+EnvM365Auth+" to a credential file (secrets never ride argv or inline env)", nil)
		}
		if e := checkFile("m365", "credential", f); e != nil {
			return Scope{}, e
		}
		sc := splitScopes(getenv(EnvM365Scopes))
		if len(sc) == 0 {
			return Scope{}, gws.NewError(gws.CodeMissingCredentials, "m365: no Graph permissions declared — set "+EnvM365Scopes+" to the least-privilege list", nil)
		}
		return Scope{Client: client, Target: t, CredentialFile: f, Scopes: sc, Sandbox: getenv(EnvM365Sandbox) == "1"}, nil
	case "gam":
		d := strings.TrimSpace(getenv(EnvGAMDomain))
		if d == "" {
			return Scope{}, gws.NewError(gws.CodeMissingCredentials, "gam: no domain selected — set "+EnvGAMDomain+" explicitly; there is no ambient-domain fallback", nil)
		}
		if !domainRE.MatchString(d) {
			return Scope{}, gws.NewError(gws.CodeInvalidInput, "gam: "+EnvGAMDomain+" must be a DNS domain", nil)
		}
		cr, err := gws.ResolveCredentials()
		if err != nil {
			if ge, ok := err.(*gws.Error); ok {
				return Scope{}, ge
			}
			return Scope{}, gws.NewError(gws.CodeMissingCredentials, "gam: "+err.Error(), nil)
		}
		if cr.Kind != gws.AuthServiceAccount {
			return Scope{}, gws.NewError(gws.CodeUnsupported, "gam: only a gws service-account selection ("+gws.EnvServiceAccountFile+", domain-wide delegation) is mapped; OAuth user credentials are not (EVIDENCE.md gap)", nil)
		}
		sc := splitScopes(getenv(EnvGAMScopes))
		if len(sc) == 0 {
			return Scope{}, gws.NewError(gws.CodeMissingCredentials, "gam: no Admin SDK scopes declared — set "+EnvGAMScopes+" to the least-privilege list", nil)
		}
		return Scope{Client: client, Target: d, CredentialFile: cr.CredentialsFile, Account: cr.Account, Scopes: sc, Sandbox: getenv(EnvGAMSandbox) == "1"}, nil
	}
	return Scope{}, gws.NewError(gws.CodeInvalidInput, client+": not an admitted client", nil)
}

// RedactedScope reports the resolved scope without secret material.
func (s Scope) RedactedScope() string {
	return fmt.Sprintf("client=%s target=%s account=%s scopes=%s credentials=file:%s sandbox=%t", s.Client, s.Target, s.Account, strings.Join(s.Scopes, ","), filepath.Base(s.CredentialFile), s.Sandbox)
}

func (s Scope) has(scope string) bool {
	for _, x := range s.Scopes {
		if x == scope {
			return true
		}
	}
	return false
}

// writeScope reports whether a permission grants more than read.
func writeScope(client, s string) bool {
	if client == "m365" {
		return strings.Contains(s, "ReadWrite") || strings.Contains(s, "Write") || strings.HasSuffix(s, ".All") && !strings.Contains(s, "Read")
	}
	return !strings.HasSuffix(s, ".readonly")
}

// ChildEnv returns env for the child: tracking opt-outs shared with gws, and
// for gam the selected gws service-account key handed over by PATH (no copy,
// no GAM-side credential store) with a caller-supplied scratch GAMCFGDIR.
func ChildEnv(s Scope, cfgDir string, env []string) []string {
	out := gws.ChildEnv(env)
	if s.Client == "gam" {
		out = append(out, "GAMCFGDIR="+cfgDir, "OAUTHSERVICEFILE="+s.CredentialFile)
	}
	return out
}
