package secrets

import (
	"os"
	"strings"
)

// AllowAgentSecretsEnv opts a host into passing the vault's secrets through to
// spawned third-party agent CLIs. It is OFF by default, and turning it on is an
// explicit, auditable acceptance of risk — never the default.
//
// Why the default is deny: a coding-agent CLI processes untrusted content (repo
// files, fetched web pages, tool output) AND has its own network egress to an
// external LLM API. Handing it the whole decrypted vault via the inherited
// environment completes the "lethal trifecta" — a single prompt injection can
// exfiltrate every credential the operator holds (cloud keys, DB passwords,
// other providers' tokens), not just the one key the agent needs to run.
//
// This is a coarse P0 stopgap. The credential-firewall end state holds the
// secrets in bashy and injects them at an egress proxy per allowlisted host, so
// the agent never sees a raw key at all; until that lands, deny-by-default is
// the safe posture.
const AllowAgentSecretsEnv = "BASHY_ALLOW_AGENT_SECRETS"

// allowAgentSecrets reports whether the operator has explicitly opted into
// letting spawned agents inherit vault secrets.
func allowAgentSecrets() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AllowAgentSecretsEnv))) {
	case "", "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// VaultEnvNames returns the set of environment-variable names the secrets vault
// projects into a shell — the LOCAL names declared in the binding template plus
// any names present in the on-disk render cache. NAMES ONLY, never values, and
// no network call: it is safe to invoke on every agent spawn.
//
// An absent or unreadable template/cache yields an empty set. The names are the
// authoritative "these variables carry vault secrets" signal, because they are
// exactly what `bashy secret env` writes with `export NAME=...`.
func VaultEnvNames() map[string]struct{} {
	names := map[string]struct{}{}
	// (a) The declared binding template: LOCAL_NAME=@ref | literal. Only the @ref
	// bindings resolve to a VAULT SECRET; a bare literal (e.g. EDITOR=vim) is not
	// sensitive, so it is left in the child's environment.
	if bindings, err := readTemplate(defaultTemplatePath()); err == nil {
		for _, b := range bindings {
			if b.isRef && b.local != "" {
				names[b.local] = struct{}{}
			}
		}
	}
	// (b) The realized render cache (`export NAME='value'` lines): the set that
	// was actually projected into this shell, which can outlive a since-edited
	// template. Names only — parseEnvFile's values are discarded.
	if cp := cacheFile(); cp != "" {
		if f, err := os.Open(cp); err == nil {
			if items, perr := parseEnvFile(f); perr == nil {
				for _, it := range items {
					if it.Name != "" {
						names[it.Name] = struct{}{}
					}
				}
			}
			_ = f.Close()
		}
	}
	return names
}

// ScrubAgentEnv returns env with the operator's credentials removed, so a spawned
// third-party agent does not inherit the keyring. It is the one call an agent launcher
// makes before handing an environment to an exec'd agent CLI.
//
// It removes a variable if EITHER:
//
//  1. the vault projects that name (an exact, operator-declared secret), OR
//  2. the name LOOKS like a credential (*_API_KEY, *_TOKEN, *_SECRET, …)
//
// Rule 2 is the one that makes this a firewall rather than a filter, and it was not
// here. Removal used to be by vault name ALONE — "a variable the vault does not project
// is never touched" — which means a credential absent from the map was treated as not a
// credential. DHNT_API_KEY was exactly that: live in the operator's environment, absent
// from the map, and therefore handed to every agent bashy spawned. It is the pooled-LLM
// gateway key, so an agent whose OWN key had been correctly scrubbed could still spend
// through it. The deny-by-default boundary had an allow-by-default hole in the middle.
//
// And it used to FAIL OPEN: `if len(names) == 0 { return env }` — no vault, no map, no
// scrub, the whole keyring handed over, silently. A security boundary that disables
// itself when it cannot read its own config is not a boundary. The shape rule now
// applies regardless, so an unreadable vault makes the firewall STRICTER, not absent.
//
// A model that legitimately needs a credential names it in `api_key_ref`, and the
// launcher grants back exactly that one (see chat.agentChildEnv). Deny by default, then
// grant one thing — which only means something if the denial is by shape.
//
// The operator can still opt out wholesale with AllowAgentSecretsEnv.
func ScrubAgentEnv(env []string) []string {
	if allowAgentSecrets() {
		return env
	}
	names := VaultEnvNames() // may be empty; the shape rule below still applies
	out := env[:0:0]
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if _, declared := names[name]; declared {
			continue
		}
		if looksLikeCredential(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// credentialSuffixes are the shapes a secret's NAME takes. Anything ending in one of
// these is treated as a credential whether or not the vault has heard of it.
//
// This is deliberately broad. The cost of a false positive is an agent that has to be
// granted a key it needs — loud, immediate, one line of yaml. The cost of a false
// negative is the operator's credential silently riding into a third-party CLI.
var credentialSuffixes = []string{
	"_API_KEY",
	"_APIKEY",
	"_KEY",
	"_TOKEN",
	"_SECRET",
	"_PASSWORD",
	"_PASSWD",
	"_CREDENTIALS",
	"_CREDENTIAL",
	"_PRIVATE_KEY",
	"_ACCESS_KEY",
	"_SESSION_TOKEN",
}

// credentialNames are exact names that carry a secret without a tell-tale suffix.
var credentialNames = map[string]struct{}{
	"AWS_SECRET_ACCESS_KEY": {},
	"AWS_ACCESS_KEY_ID":     {},
	"AWS_SESSION_TOKEN":     {},
	"GH_TOKEN":              {},
	"NPM_TOKEN":             {},
}

// looksLikeCredential reports whether a variable NAME is credential-shaped.
//
// Names, never values: a value-based heuristic would have to look at the operator's
// secrets to decide whether to hide them, and would leak through any variable whose
// value did not look secret enough.
func looksLikeCredential(name string) bool {
	if _, ok := credentialNames[name]; ok {
		return true
	}
	upper := strings.ToUpper(name)
	for _, suf := range credentialSuffixes {
		if strings.HasSuffix(upper, suf) {
			return true
		}
	}
	return false
}
