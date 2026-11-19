package secrets

import "strings"

// BoundValue resolves one environment NAME through this host's secrets.map
// binding — the vault, or the offline render cache when the vault is
// unreachable — exactly as `bashy secret env` would render that one line,
// without touching the process environment or the shared cache. It returns
// false when NAME is bound nowhere on this host.
//
// It is for bashy's own in-process credential ladders (a cloudbox session
// token): the value never reaches argv or output, so the non-interactive
// reveal guard on `secret env` does not apply.
func BoundValue(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	for _, kv := range projectMissingBindings(nil, []string{name}) {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name && strings.TrimSpace(v) != "" {
			return v, true
		}
	}
	return "", false
}
