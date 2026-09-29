package secrets

import (
	"strings"
)

// The credential firewall denies by default: ScrubAgentEnv strips every
// vault-projected secret before a third-party agent CLI is exec'd, so an agent
// cannot walk off with the operator's whole keyring.
//
// But denying everything and granting nothing is not a security model, it is an
// outage. A metered model needs exactly one credential in order to answer, and
// the registry has always said which one — `api_key_ref: moonshot` on the model
// entry. Nothing read it. So the firewall stripped MOONSHOT_API_KEY, nothing put
// it back, and every kimi agent failed to authenticate on every run while the
// fleet recorded it as "medium reliability".
//
// (deepseek looked fine only by accident: aider happens to cache that key in its
// own config, so it survived a firewall it was never actually let through.)
//
// GrantAgentKey is the other half. Deny everything; grant precisely what the
// model declared it needs, and nothing else.

// keySuffixes are the shapes a provider credential takes, most specific first.
var keySuffixes = []string{"_API_KEY", "_TOKEN", "_KEY", ""}

// CredentialEnvNames returns the environment-variable names that may carry
// the credential identified by ref, in precedence order. It never reads or
// returns a value.
func CredentialEnvNames(ref string) []string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	base := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_", "/", "_").Replace(ref))
	out := make([]string, 0, len(keySuffixes))
	for _, suffix := range keySuffixes {
		out = append(out, base+suffix)
	}
	return out
}

// PreserveEnvNames restores only named entries removed from childEnv. Names
// come from a resolved launch contract; values remain opaque and are copied
// verbatim from the launcher's environment. Missing names are not manufactured
// and an existing child entry is never overwritten or duplicated.
func PreserveEnvNames(childEnv, parentEnv, names []string) []string {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = struct{}{}
		}
	}
	present := make(map[string]struct{}, len(childEnv))
	for _, kv := range childEnv {
		if i := strings.IndexByte(kv, '='); i > 0 {
			present[kv[:i]] = struct{}{}
		}
	}
	for _, kv := range parentEnv {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name := kv[:i]
		if _, ok := wanted[name]; !ok {
			continue
		}
		if _, ok := present[name]; ok {
			continue
		}
		childEnv = append(childEnv, kv)
		present[name] = struct{}{}
	}
	return childEnv
}

// PreserveEnvAliases restores one declared credential under the environment
// variable name expected by a CLI. Each target names ordered parent candidates;
// the first non-empty source wins. Values remain opaque and no source name is
// copied into the child unless it was separately declared.
func PreserveEnvAliases(childEnv, parentEnv []string, aliases map[string][]string) []string {
	present := make(map[string]struct{}, len(childEnv))
	values := make(map[string]string, len(parentEnv))
	for _, kv := range childEnv {
		if name, _, ok := strings.Cut(kv, "="); ok && name != "" {
			present[name] = struct{}{}
		}
	}
	for _, kv := range parentEnv {
		name, value, ok := strings.Cut(kv, "=")
		if ok && name != "" && strings.TrimSpace(value) != "" {
			values[name] = value
		}
	}
	for target, sources := range aliases {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if _, ok := present[target]; ok {
			continue
		}
		for _, source := range sources {
			if value, ok := values[strings.TrimSpace(source)]; ok {
				childEnv = append(childEnv, target+"="+value)
				present[target] = struct{}{}
				break
			}
		}
	}
	return childEnv
}

// GrantAgentKey finds the one credential a model's api_key_ref names, in the
// parent's environment, and returns it as a NAME=value entry to add back to a
// scrubbed child environment.
//
// `moonshot` resolves MOONSHOT_API_KEY; `deepseek` resolves DEEPSEEK_API_KEY.
// Returns false when the ref names nothing this host has — which is a real
// answer, not a failure: the agent will fail to authenticate and say so, which
// is exactly what `agents verify --live` is for.
func GrantAgentKey(parentEnv []string, ref string) (string, bool) {
	have := map[string]string{}
	for _, kv := range parentEnv {
		if i := strings.IndexByte(kv, '='); i > 0 {
			if strings.TrimSpace(kv[i+1:]) == "" {
				continue
			}
			have[kv[:i]] = kv
		}
	}
	for _, name := range CredentialEnvNames(ref) {
		if kv, ok := have[name]; ok {
			return kv, true
		}
	}
	return "", false
}
