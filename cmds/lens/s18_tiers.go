package lenscmd

import (
	"fmt"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// S18 CLI slice: --verbose prints the resolved tier and transport for a
// pulled locator. Both names come from provider.TierOf/TransportOf, which are
// pure functions of the locator: only identifiers the caller already spelled
// (scheme, scope kind, scope value) ever reach stderr. Resolved secrets,
// tokens, environment values and file contents are never printed.

// reportTier prints the resolved tier, transport and explicit scope of loc.
func reportTier(rc *tool.RunContext, loc ref.Locator) {
	scope := "local"
	if loc.ScopeKind != "" {
		scope = loc.ScopeKind + ":" + loc.ScopeValue
	}
	fmt.Fprintf(rc.Err, "lens: tier %s transport %s scope %s\n", provider.TierOf(loc), provider.TransportOf(loc), scope)
}
