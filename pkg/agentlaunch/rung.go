package agentlaunch

import (
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
)

// THE RUNG LADDER — how well can we hear this tool?
//
// Every agent CLI in the fleet is driven through one of three transports, and
// they are not equivalent: they differ in whether bashy KNOWS a turn ended or
// GUESSES it. The ladder is ordered by that, best first:
//
//	1  ACP native     the tool speaks the Agent Client Protocol itself
//	2  native events  the tool streams NDJSON and says `turn.end` (ycode today)
//	3  PTY scrape     the tool says nothing; a turn boundary is inferred from
//	                  25 seconds of silence (chat.Session.WaitIdle)
//
// Rung 1 is the prize: ACP answers a prompt with a structured StopReason, so
// the end of a turn is a fact the agent REPORTED rather than a silence bashy
// interpreted. Rung 2 has the same virtue over a side channel. Rung 3 is the
// status quo for every third-party CLI, and it stays exactly as it is — the
// retrofit degrades, it never drops.
//
// NO ADAPTER RUNG, deliberately. An earlier draft had a rung between 1 and 2
// for a tool reached through an external ACP bridge (a Node adapter for claude
// or codex). It is gone by design decision, for two reasons that compound:
//
//   - The binary bashy execs must be the TOOL named in the tool:model binding.
//     An adapter rung necessarily execs something else — the bridge — so the
//     process running the work is no longer the tool the binding names.
//   - Nothing was on it. The probe in run #190 found neither claude-agent-acp
//     nor codex-acp installed, and no catalog entry ever declared one.
//
// A tool that cannot speak ACP itself takes rung 3, like any other tool that
// says nothing. That is the ladder working, not a gap in it.
//
// The declaration is the whole input: a tool lands on the highest rung its
// fleet entry can support, and a tool that declares nothing new lands on the
// rung it was already on.
type Rung int

const (
	// RungACPNative is rung 1: acp_exec set. The tool speaks ACP itself;
	// there is no other way onto this rung.
	RungACPNative Rung = 1
	// RungEvents is rung 2: events_arg set (a real turn.end over NDJSON).
	RungEvents Rung = 2
	// RungPTY is rung 3: a pty and the silence heuristic. Everything else.
	RungPTY Rung = 3
)

// String names the rung the way the design doc does.
func (r Rung) String() string {
	switch r {
	case RungACPNative:
		return "acp-native"
	case RungEvents:
		return "events"
	case RungPTY:
		return "pty"
	}
	return "unknown"
}

// IsACP reports whether this rung drives the tool over the Agent Client
// Protocol — the one rung with a real end-of-turn signal from the tool itself.
func (r Rung) IsACP() bool { return r == RungACPNative }

// RungFor is THE rung-selection function: given a tool's launch declaration,
// which transport does it get?
//
// It reads declarations only — no env, no host probing, no catalog lookup — so
// it is a pure function of what the registry says, and a test can pin all four
// answers. ACPExec alone is authoritative: a tool that declares one is claiming
// to speak ACP itself, and there is no second ACP rung to disambiguate.
func RungFor(l fleet.ToolLaunch) Rung {
	if strings.TrimSpace(l.ACPExec) != "" {
		return RungACPNative
	}
	if strings.TrimSpace(l.EventsArg) != "" {
		return RungEvents
	}
	return RungPTY
}

// EffectiveRung is the rung a launch will ACTUALLY be driven on right now:
// RungFor, then the opt-in gate.
//
// The two differ only while ACP is opt-in. A tool can declare rung 1 and still
// be driven on rung 3 or 4 today, because the ACP path has no fleet mileage yet
// and chat.Session is the hot path for every agent launch there is. When the
// gate is off, an ACP-declaring tool falls to the next rung its declaration
// supports — which is the same ladder, one step down, never a drop.
func EffectiveRung(l fleet.ToolLaunch) Rung {
	r := RungFor(l)
	if r.IsACP() && !ACPEnabled() {
		if strings.TrimSpace(l.EventsArg) != "" {
			return RungEvents
		}
		return RungPTY
	}
	return r
}

// ACPEnv opts a host into the ACP rungs. Set BASHY_ACP=1.
//
// This is deliberately a gate and not a default. The ACP path replaces the pty
// transport that every agent launch in the fleet currently runs on; merging it
// hot would put an untested transport under foreman, meet, weave and coach at
// once. Off, the launcher behaves exactly as it did before ACP existed.
const ACPEnv = "BASHY_ACP"

// ACPEnabled reports whether the operator turned the ACP rungs on.
func ACPEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ACPEnv))) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

// ACPArgv renders a tool's ACP launch: the binary to exec and the args after
// it. It reports false when the tool declares no acp_exec, i.e. it is not on an
// ACP rung and the caller must fall to the next one.
//
// The binary is ALWAYS the tool's declared binary — the tool named in the
// tool:model binding, never something else. There is no adapter rung, so there
// is no case in which the process bashy launches is not the tool itself.
//
// # No {prompt}, and no {model}
//
// There is no {prompt}: an ACP prompt travels in the session, not in argv,
// which is the whole reason the transport has a real turn boundary at all.
//
// There is no {model} either, and that is a DESIGN DECISION rather than an
// omission. Over ACP a tool is driven with its binding already fixed: an agent
// IS a tool:model pair, chosen by tool and band, and the protocol carries no
// model-selection call for a client to override it with. The one ACP-native
// tool in the catalog agrees — `opencode acp` accepts no model flag at all
// (--print-logs --log-level --pure --port --hostname --mdns --cors --cwd).
//
// The practical consequence, stated plainly because it constrains routing: on
// an ACP rung a tool:model binding collapses to the tool. The model is whatever
// that tool is configured to use. A caller that needs a specific model must
// pick a tool whose configuration provides it, or take a non-ACP rung.
func ACPArgv(t fleet.Tool, _ string) (bin string, args []string, ok bool) {
	tmpl := strings.TrimSpace(t.CLI.Launch.ACPExec)
	if tmpl == "" {
		return "", nil, false
	}
	fields := strings.Fields(tmpl)
	if len(fields) == 0 {
		return "", nil, false
	}
	return t.Binary(), append([]string(nil), fields[1:]...), true
}
