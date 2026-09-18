package agentlaunch

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// THE LADDER TEST. All three rungs, chosen from declarations alone.
func TestRungForCoversEveryRung(t *testing.T) {
	for _, tc := range []struct {
		name   string
		launch fleet.ToolLaunch
		want   Rung
		str    string
	}{
		{
			name:   "rung 1: acp_exec",
			launch: fleet.ToolLaunch{ACPExec: "bashy acp"},
			want:   RungACPNative,
			str:    "acp-native",
		},
		{
			name:   "rung 2: events_arg, no acp_exec",
			launch: fleet.ToolLaunch{Exec: "ycode prompt {prompt}", EventsArg: "--events {path}"},
			want:   RungEvents,
			str:    "events",
		},
		{
			name:   "rung 3: neither",
			launch: fleet.ToolLaunch{Exec: "agy -p {prompt}"},
			want:   RungPTY,
			str:    "pty",
		},
		{
			// The prize outranks the side channel: a tool that can do both is
			// driven over ACP.
			name:   "acp_exec outranks events_arg",
			launch: fleet.ToolLaunch{ACPExec: "ycode acp", EventsArg: "--events {path}"},
			want:   RungACPNative,
			str:    "acp-native",
		},
		{
			// There is no adapter rung to demote to: declaring an acp_exec IS
			// the claim to speak ACP, and it is the only way onto rung 1.
			name:   "acp_exec alone is the whole claim",
			launch: fleet.ToolLaunch{ACPExec: "thing acp"},
			want:   RungACPNative,
			str:    "acp-native",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RungFor(tc.launch); got != tc.want {
				t.Errorf("RungFor = %d (%s), want %d (%s)", got, got, tc.want, tc.want)
			}
			if got := RungFor(tc.launch).String(); got != tc.str {
				t.Errorf("String = %q, want %q", got, tc.str)
			}
		})
	}
}

func TestRungIsACP(t *testing.T) {
	for r, want := range map[Rung]bool{
		RungACPNative: true,
		RungEvents:    false,
		RungPTY:       false,
	} {
		if got := r.IsACP(); got != want {
			t.Errorf("%s.IsACP() = %v, want %v", r, got, want)
		}
	}
}

// The gate is the merge safety. Off (the default), an ACP-declaring tool is
// driven on the rung it was already on — one step down the SAME ladder, never
// a drop.
func TestEffectiveRungIsGatedByTheOptIn(t *testing.T) {
	acpOnly := fleet.ToolLaunch{ACPExec: "thing acp"}
	acpAndEvents := fleet.ToolLaunch{ACPExec: "ycode acp", EventsArg: "--events {path}"}
	events := fleet.ToolLaunch{EventsArg: "--events {path}"}
	pty := fleet.ToolLaunch{Exec: "agy -p {prompt}"}

	// Default: the env var is unset for the whole process under `go test`, but
	// pin it so a developer's exported BASHY_ACP cannot make this pass wrongly.
	t.Setenv(ACPEnv, "")
	if ACPEnabled() {
		t.Fatal("ACPEnabled() with an empty BASHY_ACP")
	}
	for l, want := range map[*fleet.ToolLaunch]Rung{
		&acpOnly:      RungPTY,    // nothing else declared: back to the silence heuristic
		&acpAndEvents: RungEvents, // it also has a real turn.end: use that
		&events:       RungEvents,
		&pty:          RungPTY,
	} {
		if got := EffectiveRung(*l); got != want {
			t.Errorf("gate off: EffectiveRung(%+v) = %s, want %s", *l, got, want)
		}
	}

	t.Setenv(ACPEnv, "1")
	if !ACPEnabled() {
		t.Fatal("ACPEnabled() with BASHY_ACP=1")
	}
	for l, want := range map[*fleet.ToolLaunch]Rung{
		&acpOnly:      RungACPNative,
		&acpAndEvents: RungACPNative,
		&events:       RungEvents,
		&pty:          RungPTY,
	} {
		if got := EffectiveRung(*l); got != want {
			t.Errorf("gate on: EffectiveRung(%+v) = %s, want %s", *l, got, want)
		}
	}

	// The gate reads like every other bashy opt-in.
	for _, off := range []string{"", "0", "false", "off", "no", "  "} {
		t.Setenv(ACPEnv, off)
		if ACPEnabled() {
			t.Errorf("BASHY_ACP=%q enabled ACP", off)
		}
	}
	for _, on := range []string{"1", "true", "yes", "on"} {
		t.Setenv(ACPEnv, on)
		if !ACPEnabled() {
			t.Errorf("BASHY_ACP=%q did not enable ACP", on)
		}
	}
}

// agy MUST NOT MOVE. `agy -p` prints nothing and exits 0 when stdout is not a
// TTY, so its whole reason for being on rung 3 is that the pty IS the
// transport. This is the acceptance criterion, pinned against the compiled-in
// baseline catalog rather than a hand-written struct.
//
// The tools that CANNOT speak ACP are the load-bearing half: the retrofit
// degrades, it never drops, so a tool that declares nothing new must be driven
// exactly as it was before ACP existed.
func TestBaselineToolsStayOnTheRungTheyAreOnToday(t *testing.T) {
	t.Setenv(ACPEnv, "1") // even with ACP fully on
	cat := fleet.New(fleet.WithRoot(t.TempDir()))
	for name, want := range map[string]Rung{
		"agy":    RungPTY,
		"claude": RungPTY,
		"codex":  RungPTY,
		"aider":  RungPTY,
	} {
		tool, ok := cat.Tool(name)
		if !ok {
			t.Errorf("%s is not in the baseline catalog", name)
			continue
		}
		if got := RungFor(tool.CLI.Launch); got != want {
			t.Errorf("%s: RungFor = %s, want %s", name, got, want)
		}
		if got := EffectiveRung(tool.CLI.Launch); got != want {
			t.Errorf("%s: EffectiveRung = %s, want %s", name, got, want)
		}
		if _, _, ok := ACPArgv(tool, ""); ok {
			t.Errorf("%s: renders an ACP argv but declares no acp_exec", name)
		}
	}
}

// DECLARED is not EFFECTIVE, and the baseline catalog is where that stops being
// theoretical.
//
// Both ACP-declaring tools were measured on the wire, not asserted: `opencode
// acp` in run #190, `ycode acp` after run #24 merged. With the opt-in gate off
// each is still driven on the rung it was on before ACP existed — and they fall
// to DIFFERENT rungs, which is the point of the ladder being a ladder:
//
//	opencode  declares acp only            -> gate off: pty
//	ycode     declares acp AND events      -> gate off: events
//
// A single-step fallback would have collapsed both to pty and quietly cost
// ycode its structured turn boundaries.
func TestOpencodeDeclaresACPButIsGatedUntilOptedIn(t *testing.T) {
	cat := fleet.New(fleet.WithRoot(t.TempDir()))
	for _, tc := range []struct {
		name        string
		whenGateOff Rung
	}{
		{"opencode", RungPTY}, // no events_arg: falls all the way to pty
		{"ycode", RungEvents}, // has events_arg: falls one step, to events
	} {
		tool, ok := cat.Tool(tc.name)
		if !ok {
			t.Fatalf("%s is not in the baseline catalog", tc.name)
		}
		if got := RungFor(tool.CLI.Launch); got != RungACPNative {
			t.Errorf("%s: RungFor = %s, want acp-native — measured on the wire", tc.name, got)
		}
		t.Setenv(ACPEnv, "0")
		if got := EffectiveRung(tool.CLI.Launch); got != tc.whenGateOff {
			t.Errorf("%s: gate off EffectiveRung = %s, want %s", tc.name, got, tc.whenGateOff)
		}
		t.Setenv(ACPEnv, "1")
		if got := EffectiveRung(tool.CLI.Launch); got != RungACPNative {
			t.Errorf("%s: gate on EffectiveRung = %s, want acp-native", tc.name, got)
		}
	}

	tool, _ := cat.Tool("opencode")

	// The argv is the TOOL, never a bridge: there is no adapter rung.
	bin, args, ok := ACPArgv(tool, "")
	if !ok {
		t.Fatal("opencode declares acp_exec but renders no ACP argv")
	}
	if bin != tool.Binary() {
		t.Errorf("bin = %q, want the tool binary %q", bin, tool.Binary())
	}
	if len(args) == 0 || args[0] != "acp" {
		t.Errorf("args = %q, want the acp subcommand", args)
	}
}

func TestACPArgvRendersTheTemplate(t *testing.T) {
	tool := fleet.Tool{
		Name: "thing",
		CLI:  fleet.ToolCLI{Binary: "/opt/thing/bin/thing", Launch: fleet.ToolLaunch{ACPExec: "thing acp --pure"}},
	}
	bin, args, ok := ACPArgv(tool, "gpt-9")
	if !ok {
		t.Fatal("ACPArgv reported false for a declared template")
	}
	// The BINARY is the tool's, always: on an ACP rung the process bashy
	// launches is the tool named in the tool:model binding, never a bridge.
	if bin != "/opt/thing/bin/thing" {
		t.Errorf("bin = %q, want the tool's declared binary", bin)
	}
	if got, want := strings.Join(args, " "), "acp --pure"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	// A model argument is IGNORED, not substituted. An ACP rung drives a tool
	// with its binding already fixed; the protocol has no model-selection call
	// and `opencode acp` accepts no model flag. Passing one changes nothing.
	_, noModel, _ := ACPArgv(tool, "")
	if strings.Join(noModel, " ") != strings.Join(args, " ") {
		t.Errorf("model argument changed the argv: %q vs %q", noModel, args)
	}

	// A {model} token in an acp_exec is rendered LITERALLY — it is not a slot.
	// This pins the decision: a template author who writes one gets a visibly
	// wrong argv rather than a silent, model-less launch that looks correct.
	lit := fleet.Tool{Name: "y", CLI: fleet.ToolCLI{Binary: "y", Launch: fleet.ToolLaunch{ACPExec: "y acp --model {model}"}}}
	_, litArgs, _ := ACPArgv(lit, "gpt-9")
	if got, want := strings.Join(litArgs, " "), "acp --model {model}"; got != want {
		t.Errorf("args = %q, want %q — {model} must not be substituted", got, want)
	}

	if _, _, ok := ACPArgv(fleet.Tool{Name: "x"}, "m"); ok {
		t.Error("ACPArgv reported true for a tool with no acp_exec")
	}
}
