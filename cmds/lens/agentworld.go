// Agent-world providers for lens: `lens -t VIEW sprint:446`,
// `lens -t listing run:<id>`, and the same pull form for story, agent,
// claim, mb, meet, bus, kb and host. Each scheme pulls through one
// read-only provider.Entity over AgentWorldRefs; story and claim are
// locator schemes, not ref kinds, so their providers report the missing
// owning-store wiring (see provider.UnwiredSchemes) instead of resolving.
//
// This file only registers schemes. It must not grow flags, views, or
// --since handling here: the manager owns --since integration, and lens.go
// owns the views.
package lenscmd

import (
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// AgentWorldRefs is the ref.Registry the agent-world providers resolve
// through. The embedding shell fills it — one resolver per kind, from the
// store that owns the kind (weave for sprint/run, fleet for agent/host, bus
// for mb/bus, meet for meet, kb for kb) — the same wiring step that fills
// lexicon.RefResolvers for `define`; this package only ever reads it. Only
// ref-kind schemes resolve: story and claim have no ref kind, so no resolver
// can be registered for them here without changing the closed ref
// vocabulary, and their providers report that seam instead.
//
// A kind with no resolver is not an empty store: the provider surfaces
// ref.ErrNoResolver, which stays distinct from ref.ErrNotFound.
var AgentWorldRefs = ref.NewRegistry()

func init() {
	for _, scheme := range provider.AgentWorldSchemes {
		RegisterProvider(scheme, agentWorldFactory(scheme))
	}
}

func agentWorldFactory(scheme string) ProviderFactory {
	return func(rc *tool.RunContext, loc ref.Locator) (provider.Provider, error) {
		return provider.NewEntity(AgentWorldRefs, scheme), nil
	}
}
