package chat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Selector chooses which agent a session launches: a SPECIFIC one named by the
// caller, or ANY operable one matching a capability band and/or a tool.
//
// It is the same choice `bashy agent list` and `meet --min-band` already make —
// name one, or let a band pick one — so "who is routable" means the same thing
// everywhere.
type Selector struct {
	Agent string // nick, canonical name, family alias, or a tool:model binding
	Tool  string // any operable agent using this tool (e.g. "codex")
	Band  int    // any operable agent pegged at or above this band (1-4)
}

// pickCandidate is one operable agent a band/tool selector admits.
type pickCandidate struct {
	Name    string
	Binding string
	Nick    string
	Tool    string
	Band    int
}

// PickAgent resolves a Selector to a single agent name ready for Interact/Invoke.
//
// A specific --agent wins outright (canonicalized through the catalog when it
// names one; a bare tool like "claude" is left as-is — it is a valid launch that
// names no agent). Otherwise the strongest operable agent matching --band/--tool
// is chosen deterministically (highest band, then name). An unreachable match is
// reported, never silently dropped — a session credited to an agent the host
// cannot launch is the absence-of-evidence failure this fleet forbids.
//
// An empty Selector returns "", meaning "no selector — use the default agent",
// so `bashy chat` with no flags behaves like `invoke` with no --agent.
func PickAgent(sel Selector) (string, error) {
	if strings.TrimSpace(sel.Agent) != "" {
		if sel.Band != 0 || strings.TrimSpace(sel.Tool) != "" {
			return "", fmt.Errorf("chat: give --agent OR --band/--tool, not both — " +
				"--agent names one, --band/--tool pick one for you")
		}
		if a, ok := newCatalog().Agent(sel.Agent); ok {
			return a.Name, nil
		}
		// `chat --agent X` CLAIMS AN IDENTITY, so X must be one. This is the
		// call site that differs from weave's raw argv: `weave start -- claude`
		// runs a command and gets no principal, while a chat session is
		// addressable, keeps a conversation store, and answers mail under that
		// name. A bare tool here mints a name nothing can route to.
		//
		// --tool and --band are unaffected: they SELECT among registered agents
		// rather than naming one, and are resolved below.
		return "", agentlaunch.RegistrationRefusal(sel.Agent)
	}
	if sel.Band == 0 && strings.TrimSpace(sel.Tool) == "" {
		return "", nil // no selector: caller falls back to the default agent
	}
	if sel.Band < 0 || sel.Band > fleet.MaxBand {
		return "", fmt.Errorf("chat: --band %d out of range (1-%d)", sel.Band, fleet.MaxBand)
	}
	picks, skipped := pickCandidates(sel)
	if len(picks) == 0 {
		hint := ""
		if len(skipped) > 0 {
			hint = " (skipped, not routable: " + strings.Join(skipped, ", ") + ")"
		}
		return "", fmt.Errorf("chat: no operable agent matches %s%s — "+
			"`bashy agent list` shows the fleet", describeSelector(sel), hint)
	}
	return picks[0].Name, nil
}

// pickCandidates returns the operable agents matching a band/tool selector,
// strongest first, plus the ones the band/tool selected but the host cannot drive.
func pickCandidates(sel Selector) (picks []pickCandidate, skipped []string) {
	cat := newCatalog()
	agents, _ := cat.Agents()
	wantTool := strings.TrimSpace(sel.Tool)
	for _, a := range agents {
		_, tool, model, err := cat.Binding(a.Name)
		if err != nil {
			continue // dangling: no band selects it
		}
		if sel.Band != 0 && model.Band < sel.Band {
			continue
		}
		if wantTool != "" && tool.Name != wantTool {
			continue
		}
		if ok, reason := capability.Operable(tool.Name); !ok {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", a.Name, reason))
			continue
		}
		picks = append(picks, pickCandidate{
			Name: a.Name, Binding: a.MatrixKey(), Nick: a.NickName(),
			Tool: tool.Name, Band: model.Band,
		})
	}
	// Strongest first, so "any one" means the best one.
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].Band != picks[j].Band {
			return picks[i].Band > picks[j].Band
		}
		return picks[i].Name < picks[j].Name
	})
	return picks, skipped
}

func describeSelector(sel Selector) string {
	var parts []string
	if sel.Band != 0 {
		parts = append(parts, "band L"+fmt.Sprint(sel.Band)+"+")
	}
	if strings.TrimSpace(sel.Tool) != "" {
		parts = append(parts, "tool "+sel.Tool)
	}
	if len(parts) == 0 {
		return "the request"
	}
	return strings.Join(parts, " + ")
}
