// Agent-world snapshots and deltas share the owning store ref.Resolver.
package lenscmd

import (
	"fmt"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// AgentWorldRefs is populated by the embedding shell with owning-store resolvers.
// Story and claim use the explicit hooks in lookupEntityResolver.
var AgentWorldRefs = ref.NewRegistry()

func init() {
	for _, scheme := range provider.AgentWorldSchemes {
		RegisterProvider(scheme, agentWorldFactory(scheme))
	}
}

func agentWorldFactory(scheme string) ProviderFactory {
	return func(rc *tool.RunContext, loc ref.Locator) (provider.Provider, error) {
		e := provider.NewEntity(AgentWorldRefs, scheme)
		e.Resolver = ref.ResolverFunc(func(id string) (ref.Node, error) {
			r, ok := lookupEntityResolver(scheme)
			if !ok {
				return ref.Node{}, fmt.Errorf("%w: %s", ref.ErrNoResolver, scheme)
			}
			return r.Resolve(id)
		})
		return e, nil
	}
}
