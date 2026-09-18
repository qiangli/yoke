package fleet

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/secrets"
	"github.com/qiangli/yoke/pkg/spacetime"
)

// Check is one entry's verdict at this host's coordinate.
type Check struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Skipped marks an entry that was never a candidate — a harness we
	// recognize but do not drive, say. Not usable and not a failure: a
	// healthy host must not report an error for a tool it never intended
	// to launch.
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"` // version, target id, launch argv
	// Warn carries something true but not disqualifying — an entry that
	// works yet is missing something a caller may be counting on. It never
	// affects OK: a warning that failed the check would just get silenced.
	Warn string `json:"warn,omitempty"`
}

// Probes builds the probe set fleet checks read. It is the same engine
// pkg/skills gates applicability on, so a tool that a skill's `has=codex`
// clause can see is a tool `fleet verify` can see.
func Probes(cache spacetime.Cache) *spacetime.ProbeSet {
	return spacetime.DefaultProbes(cache)
}

// VerifyTool reports whether a tool is installed and has a launch declaration.
//
// Standalone and offline: it asks the PATH whether the binary exists and
// what version it reports. It never runs the tool's own work.
func (c *Catalog) VerifyTool(name string, ps *spacetime.ProbeSet) Check {
	chk := Check{Kind: KindTool, Name: name}
	t, ok := c.Tool(name)
	if !ok {
		chk.Reason = "not in the catalog"
		return chk
	}
	chk.Name = t.Name

	if !t.IsCLI() {
		chk.Skipped = true
		chk.Reason = fmt.Sprintf("kind %q is not an agentic CLI", t.Kind)
		return chk
	}
	if t.CLI.Launch.Exec == "" {
		chk.Skipped = true
		chk.Reason = "recognized for self-identification only; no launch template"
		return chk
	}

	bin := t.CLI.Binary
	if bin == "" {
		bin = t.Name
	}
	v, present := ps.Value("tool." + bin)
	if !present || v == "absent" {
		chk.Reason = "not installed: " + bin + " is not on PATH"
		return chk
	}
	if v != "present" {
		chk.Detail = v
	}

	chk.OK = true
	chk.Reason = "installed; PATH and version evidence only (work has not been run)"
	// codex runs the /etc/passwd login shell on macOS rather than $SHELL,
	// so shell-forcing does not reach it without an explicit install step.
	if t.Name == "codex" && runtime.GOOS == "darwin" {
		chk.Reason = "installed; PATH and version evidence only; shell = the login shell (run `bashy install-agent codex` to route through bashy)"
	}
	if t.CLI.Launch.AuthHint != "" {
		chk.Reason += "; " + t.CLI.Launch.AuthHint
	}
	return chk
}

// SmokeToken is the exact stdout token a live fleet probe requires. Exit
// status is deliberately not evidence: harnesses can report provider errors
// and still exit zero.
const SmokeToken = "SMOKE-OK"

const smokePrompt = "Reply with exactly: " + SmokeToken

// SmokeArgv renders a real, minimal headless turn for tool. A bare tool has
// no model in its name, so select its first declared binding; catalog agents
// are name-sorted, making that choice deterministic.
func (c *Catalog) SmokeArgv(tool string) ([]string, bool) {
	t, ok := c.Tool(tool)
	if !ok || !t.IsCLI() || t.CLI.Launch.Exec == "" {
		return nil, false
	}
	modelID := ""
	agents, _ := c.Agents()
	for _, a := range agents {
		if a.Tool != t.Name {
			continue
		}
		if m, ok := c.Model(a.Model); ok {
			modelID = m.TargetFor(t.Name)
			break
		}
	}
	argv := t.Argv(modelID, smokePrompt)
	return argv, len(argv) > 0
}

// VerifyModel reports whether a model is usable from this host.
//
// The default is a structural check with no network: a probe that dialed a
// provider on every `verify` would make an offline host look broken.
func (c *Catalog) VerifyModel(name string, _ *spacetime.ProbeSet) Check {
	chk := Check{Kind: KindModel, Name: name}
	m, ok := c.Model(name)
	if !ok {
		chk.Reason = "not in the catalog"
		return chk
	}
	chk.Name, chk.Detail = m.Name, m.Target()
	if m.Band < 1 {
		chk.Warn = "unpegged: no band, so a --min-band roster will never seat an agent bound to it"
	}

	// AUTH and BILLING are separate questions now, so verify asks them separately.
	// Every message here used to have to describe both — which was the tell that one
	// field was doing two jobs. See types.go.
	switch m.Kind {
	case ModelKindAPI:
		if m.APIKeyRef == "" {
			chk.Reason = "kind is api but no api_key_ref is declared — nothing to authenticate with"
			return chk
		}
		chk.OK = true
		chk.Reason = "authenticates with the vault key " + m.APIKeyRef
	case ModelKindSubscription:
		chk.OK = true
		chk.Reason = "authenticates interactively; the CLI holds the seat on this host"
	case ModelKindLocal:
		chk.OK = true
		chk.Reason = "no credential; served by a paired host"
	case "":
		chk.OK = true
		chk.Reason = "no access kind declared"
	default:
		chk.Reason = fmt.Sprintf("unknown kind %q (want subscription, api, or local)", m.Kind)
		return chk
	}

	switch m.BillingMode() {
	case BillingMetered:
		chk.Reason += "; metered per token"
	case BillingFlat:
		chk.Reason += "; flat-rate plan with a HARD quota — exhausting it BLOCKS this agent until the window resets"
	case BillingFlatThenMetered:
		// The one an unattended run has to know about. Overrunning a seat does not fail
		// loudly; it starts spending money quietly.
		chk.Reason += "; flat-rate plan that OVERRUNS INTO PER-TOKEN BILLING — exhausting the " +
			"quota does not block, it starts charging. An unattended fleet run can spend real " +
			"money here after the seat is gone, and nothing will fail to tell you"
		chk.Warn = "billing overruns into money: quota exhaustion is a COST event here, not an availability one"
	case BillingFree:
		chk.Reason += "; free (your own hardware)"
	case "":
		// no kind, no billing — already reported above
	default:
		chk.OK = false
		chk.Reason = fmt.Sprintf("unknown billing %q (want metered, flat, flat_then_metered, or free)", m.Billing)
	}
	return chk
}

// VerifyAgent reports whether an agent can actually be launched: both
// halves of its binding resolve, the tool is operable, and the tool can
// select the model it is bound to.
func (c *Catalog) VerifyAgent(name string, ps *spacetime.ProbeSet) Check {
	chk := Check{Kind: KindAgent, Name: name}
	a, tool, model, err := c.Binding(name)
	if err != nil {
		chk.Reason = err.Error()
		return chk
	}
	chk.Name = a.Name

	if tc := c.VerifyTool(tool.Name, ps); !tc.OK {
		chk.Reason = "tool " + tool.Name + ": " + tc.Reason
		return chk
	}
	if mc := c.VerifyModel(model.Name, ps); !mc.OK {
		chk.Reason = "model " + model.Name + ": " + mc.Reason
		return chk
	}
	if !tool.TakesModel() {
		chk.Reason = fmt.Sprintf("tool %s has no {model} placeholder, so it cannot select %s — the binding is a label, not a selection", tool.Name, model.Name)
		return chk
	}
	if ref := tool.CredentialRefFor(model); ref != "" {
		if _, ok := secrets.GrantAgentKey(os.Environ(), ref); !ok {
			chk.Reason = fmt.Sprintf("tool %s requires the %s provider credential for model %s", tool.Name, model.Provider, model.Name)
			return chk
		}
	}

	chk.OK = true
	chk.Reason = "launchable"
	chk.Detail = strings.Join(tool.Argv(model.TargetFor(tool.Name), PromptToken), " ")
	return chk
}
