package secrets

import "strings"

// Sprint: #379; Story: #37; Story-ID: 5b537ed16256
//
// A model names its credential by a STANDARD ref — `api_key_ref: zai` — and
// that ref is the same in every copy of the catalog. The VAULT name is not: a
// host binds the ref under its own name in secrets.map (`ZAI_API_KEY=@host-zai`),
// and that binding is the only place the two are joined. A caller that asked
// the vault for the bare ref (`bashy secret get zai`) therefore found nothing on
// every host set up from the template, while the same key was one `bashy secret
// env` away. The resolution has to walk the binding:
//
//	ref  ->  conventional env names (ZAI_API_KEY, ZAI_TOKEN, …)
//	     ->  the host's secrets.map line for one of those names
//	     ->  the vault (or the offline render cache when the vault is down)
//
// GrantAgentKey is the first step on its own, over an environment that has
// already been rendered. ResolveAgentKey is the whole walk.

// ResolveAgentKey resolves the credential a model's api_key_ref names on THIS
// host, as a NAME=value entry. The parent environment wins when it already
// carries one of the ref's conventional names; otherwise the host's secrets.map
// binding for those names is rendered — through the vault, or the offline cache
// when the vault is unreachable — exactly as `bashy secret env` would, without
// touching the process environment or the shared cache.
//
// It grants ONE key: only the ref's own names are rendered, never the rest of
// the template. Returns false when the ref is bound nowhere on this host, which
// is a real answer: the model cannot authenticate here.
//
// An exported-but-empty variable (`ZAI_API_KEY=`, which a shell profile that
// ran before the vault was reachable leaves behind) is ABSENT, not present:
// an empty credential is never an answer, so it must not shadow the binding.
func ResolveAgentKey(parentEnv []string, ref string) (string, bool) {
	if kv, ok := GrantAgentKey(parentEnv, ref); ok {
		return kv, true
	}
	names := CredentialEnvNames(ref)
	if len(names) == 0 {
		return "", false
	}
	present := make([]string, 0, len(parentEnv))
	for _, kv := range parentEnv {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.TrimSpace(kv[i+1:]) != "" {
			present = append(present, kv)
		}
	}
	return GrantAgentKey(projectMissingBindings(present, names), ref)
}
