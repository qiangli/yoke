package lenscmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// S18 sandbox tier: `oci:containers@<peer>` lists containers on a remote
// Podman-backed outpost peer, read-only. The scope is only ever the peer
// spelled on the locator — nothing is inferred from the environment or the
// local checkout — and anything that cannot be resolved to exactly one
// online peer with exactly one saved outpost SSH target fails closed.
//
// Transport is outpost's existing SSH exec path: exactly one read-only
// command, `outpost ssh exec <target> -- podman ps --all --format json`.
// No write verb is ever issued and the recipe exposes no write-capable
// stream. The local host (and Dragon in particular, the dev-loop macOS
// host with no Podman) is never a sandbox: local containers stay on the
// unscoped oci: recipe in the local tier.

// sandboxRunner is nil in production (the real read-only runner); tests
// inject one.
var sandboxRunner func(context.Context, string, ...string) ([]byte, error)

// sandboxRefusesLocal reports whether id names this host: the short local
// hostname or Dragon, the dev-loop host without Podman. Comparison is
// case-insensitive; the grammar already forbids an empty scope value.
func sandboxRefusesLocal(id string) bool {
	if strings.EqualFold(id, "dragon") {
		return true
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return false
	}
	short, _, _ := strings.Cut(host, ".")
	return short != "" && strings.EqualFold(id, short)
}

// sandboxProvider serves oci:containers@sandbox:<peer> through the
// Podman-backed outpost target. Only the oci scheme is served: a sandbox
// is containers on Podman, never a local directory (dir:) or another
// tier's recipe. Dragon-local and local-host ids are refused before any
// outpost query runs.
func sandboxProvider(loc ref.Locator) (provider.Provider, error) {
	if loc.Scheme != "oci" {
		return nil, fmt.Errorf("provider %q does not support @sandbox scope", loc.Scheme)
	}
	if loc.ScopeValue == "" {
		return nil, fmt.Errorf("oci containers need an explicit @sandbox peer, got %q", loc.String())
	}
	if sandboxRefusesLocal(loc.ScopeValue) {
		return nil, fmt.Errorf("@sandbox:%s: sandbox is never Dragon or the local host: name a remote Podman-backed outpost peer", loc.ScopeValue)
	}
	target, host, err := resolveHostPeerEx(context.Background(), loc.ScopeValue)
	if err != nil {
		return nil, err
	}
	if sandboxRefusesLocal(host) {
		return nil, fmt.Errorf("@sandbox:%s: sandbox is never Dragon or the local host: peer %s resolves locally", loc.ScopeValue, host)
	}
	// The peer id stays the scope value so pulled refs round-trip; the
	// resolved SSH target travels only in the podman argv.
	return provider.NewOCISandboxRecipe(loc.ScopeValue, target, sandboxRunner)
}
