package fleet

import (
	"os"
	"os/exec"

	"github.com/qiangli/yoke/pkg/secrets"
)

// Active reports whether a tool is usable on this host now, with no network:
// its binary resolves on PATH or its managed install sits in the local cache.
func (t Tool) Active() bool {
	if _, ok := t.ManagedCached(); ok {
		return true
	}
	_, err := exec.LookPath(t.Binary())
	return err == nil
}

// Active reports whether a model can authenticate on this host now, with no
// network. A local model needs no credential; an api model needs its
// api_key_ref resolvable from the environment under its vault names; a
// subscription seat is held by the CLI's own login, which an offline check
// cannot disprove. An undeclared kind constrains nothing; an unknown kind
// matches nothing.
//
// An exported-but-empty variable is absent, not present: GrantAgentKey
// already refuses empty values, so a shell profile that ran before the
// vault was reachable never counts as a credential.
func (m Model) Active() bool {
	switch m.Kind {
	case ModelKindLocal, ModelKindSubscription, "":
		return true
	case ModelKindAPI:
		if m.APIKeyRef == "" {
			return false
		}
		_, ok := secrets.GrantAgentKey(os.Environ(), m.APIKeyRef)
		return ok
	default:
		return false
	}
}

// AgentActive reports whether an agent is usable on this host now: both
// halves of its binding resolve and are active. A cascade serves through its
// base agent, so the base's halves decide; a base cycle resolves to nothing.
func (c *Catalog) AgentActive(a Agent) bool {
	seen := map[string]bool{}
	for a.Base != "" {
		if seen[a.Name] {
			return false
		}
		seen[a.Name] = true
		base, ok := c.Agent(a.Base)
		if !ok {
			return false
		}
		a = base
	}
	t, ok := c.Tool(a.Tool)
	if !ok || !t.Active() {
		return false
	}
	m, ok := c.Model(a.Model)
	if !ok || !m.Active() {
		return false
	}
	return true
}
