package bus

// ONE RESOLVER. The board's fast checks (role, roster, cursor) and `whois`
// used to be independent name systems, and on a live host no two of them
// agreed: `ping steward` delivered while `whois steward` said "names
// nothing", and a meet participant the resolver knew was un-sendable here.
// docs/agent-comms-synergy.md's first ordered fix is that ping, mb, notify
// and whois must answer the same question the same way.
//
// The wiring runs in both directions, deliberately from THIS side of the
// seam so it cannot be forgotten:
//
//   - the role table (HostRoles, filled by pkg/steward and pkg/weave) is
//     bridged into pkg/principal at init, so whois resolves every seat the
//     addresser can post to;
//   - ResolveSendTarget falls through to principal.LookupSend when its own
//     fast checks miss, so a send reaches every name the resolver answers
//     for.
//
// Perf is the reason the fallback is a fallback: a full whois costs ~0.8s
// (liveness probes, mDNS timeouts). The fast checks stay first and cost
// microseconds; LookupSend is probe-free and memoizes the catalog load, so
// even the miss path stays far under the measured whois cost.
//
// This import makes pkg/bus depend on pkg/principal (and through it the
// fleet catalog — already in bus's dependency cone via agentctl). The
// reverse direction stays forbidden: principal reads the coordination
// stores with its own minimal parsers and must never import this package.

import (
	"strings"

	"github.com/qiangli/yoke/pkg/principal"
)

func init() {
	principal.RegisterRoleSource(func() []principal.HostRole {
		if HostRoles == nil {
			return nil
		}
		roles := HostRoles()
		out := make([]principal.HostRole, 0, len(roles))
		for _, r := range roles {
			out = append(out, principal.HostRole{Label: r.Label, Topic: r.Topic, Holder: r.Holder})
		}
		return out
	})
}

// principalTargets is the resolver consult, held in a var so a test can pin
// the answer without a catalog on disk.
var principalTargets = principal.LookupSend

// resolvePrincipalTarget asks the host resolver about a target the board's
// own fast checks did not recognize. Kinds that cannot read mail (tools,
// models, hosts) do not resolve; a name whose matches disagree about the
// canonical address is ambiguity, and per Yoke's rule an ambiguous identity
// fails with choices rather than being guessed at.
func resolvePrincipalTarget(target string) (addr, kind string, ok bool) {
	var name, k string
	for _, m := range principalTargets(target) {
		switch m.Kind {
		case principal.KindRole:
			// The fast path resolves every registered role before this runs;
			// answering here too keeps the two paths interchangeable.
			return m.Name, TargetRole, true
		case principal.KindAgent, principal.KindPerson:
			if name != "" && !strings.EqualFold(name, m.Name) {
				return "", "", false // two canonical addresses — ambiguous
			}
			if name == "" {
				name = m.Name
				k = TargetAgent
				if m.Kind == principal.KindPerson {
					k = TargetPerson
				}
			}
		}
	}
	return name, k, name != ""
}
