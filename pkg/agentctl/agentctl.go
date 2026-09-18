// Package agentctl is the control contract for driving a third-party agent CLI
// unattended: how to make it run headless, how to get past its trust prompt, and
// how to steer it once it is running.
//
// # The two layers
//
// pkg/agentpty is TERMINAL control — allocate a pty, watch the output, kill a
// runaway, carry keystrokes on a socket. It knows nothing about any particular
// tool.
//
// This package is TOOL control — what claude, codex, aider, opencode and agy each
// need in order to behave like a subroutine instead of an application. It sits on
// top of agentpty and can be imported on its own.
//
// The split matters because the two fail differently. A terminal problem is a
// hang or an OOM; a tool problem is an agent that runs perfectly and answers the
// wrong question, or opens a REPL you did not ask for.
//
// # The one rule
//
// A TERMINAL CHANGES WHAT AN AGENT CAN BE ASKED. IT MUST NEVER CHANGE WHAT THE
// AGENT DOES.
//
// This is not abstract. Agent CLIs decide whether to run headless by sniffing
// whether stdout is a terminal. That inference is correct on a pipe and WRONG the
// moment you give the agent a pty — which is precisely what you must do to clear
// its trust prompt or steer it. claude, given a pty and no explicit print flag,
// opened its REPL and sat there forever while the captured "answer" filled with
// box-drawing.
//
// So every tool's headless mode is DECLARED, in the fleet registry, and never
// inferred. If a tool needs a flag to be a subroutine, that flag is part of its
// contract, not something the environment is allowed to imply.
package agentctl

import (
	"strings"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Profile is a tool's control contract, as the fleet registry declares it.
//
// Everything here answers a question the launcher cannot answer for itself:
// what makes this tool headless, what it will ask before it starts, and whether
// it listens once it has.
type Profile struct {
	Tool string

	// Headless is the argv that makes the tool a subroutine — print mode, an
	// `exec` subcommand, a `--message` flag. Declared, never inferred; a bare
	// launch hangs at the tool's own interactive prompt.
	Headless []string

	// Preseed is a config file that suppresses the first-run trust prompt before
	// it can appear ("" = nothing to do). Prevention: cheaper and quieter than
	// answering the prompt after the fact.
	Preseed string

	// Clear is how to answer a trust prompt that appears anyway, as "say:<keys>"
	// ("say:1" = press 1). The cure, when prevention misses — a per-directory
	// prompt the preseed did not cover.
	Clear string

	// Steerable reports whether the tool reads its terminal mid-run, i.e. whether
	// a `say` reaches it at all. A steer sent to a tool that does not listen is
	// not an error, but it is not a steer either, and the caller should be able
	// to say so.
	Steerable bool

	// GracefulQuit reports whether the tool can be asked to exit rather than
	// killed.
	GracefulQuit bool

	// SteerExec is the argv that actually accepts steering — usually NOT the
	// headless launch, which has nothing to interrupt. Empty means the tool has no
	// interactive session to reach.
	SteerExec string
}

// NeedsTerminal reports whether a HEADLESS turn needs a pty anyway — i.e. whether
// this tool has a prompt that must be cleared reactively while it runs.
//
// It is deliberately NOT "is the tool steerable". A pty is not free: it merges
// stdout and stderr, so the tool's chrome — codex prints a version banner and its
// workdir — lands inside the captured answer, where a pipe keeps it out. And a
// terminal buys a headless launch nothing anyway: `codex exec` and `agy -p` run
// the prompt and EXIT. There is no session there to steer.
//
// So today only claude qualifies, because only claude has a trust prompt that can
// appear mid-run (trust_clear). codex and agy are steerable — measured — but their
// steering lives in a DIFFERENT launch (SteerExec), and asking for it means asking
// for it, not getting a terminal by accident.
func (p Profile) NeedsTerminal() bool {
	return strings.TrimSpace(p.Clear) != ""
}

// CanSteer reports whether this tool can be interrupted mid-run at all, and names
// the launch that allows it.
//
// Steerable is a MEASURED capability (pkg/agentpty/steer_live_test.go drives the
// real tool through the real control socket). SteerExec is the launch that
// delivers it — usually not the headless one, because a one-shot has nothing to
// interrupt.
func (p Profile) CanSteer() bool {
	return p.Steerable && strings.TrimSpace(p.SteerExec) != ""
}

// ProfileFor reads a tool's contract from the fleet registry. The registry is the
// single source of truth — no package keeps its own table of what claude needs.
func ProfileFor(tool string) (Profile, bool) {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return Profile{}, false
	}
	t, ok := fleet.New().Tool(tool)
	if !ok || !t.IsCLI() {
		return Profile{}, false
	}
	l := t.CLI.Launch
	headless, _ := t.ArgvPrefix("")
	return Profile{
		Tool:         t.Name,
		Headless:     headless,
		Preseed:      l.TrustPreseed,
		Clear:        l.TrustClear,
		Steerable:    l.SupportsSay,
		SteerExec:    l.SteerExec,
		GracefulQuit: l.SupportsGracefulQuit,
	}, true
}

// ClearPayload decodes a trust-clear spec ("say:1") into the keystrokes that
// answer it. Anything that is not a `say:` is not something we know how to
// answer, and reports false rather than guessing at a keypress.
func ClearPayload(spec string) (string, bool) {
	method, payload, ok := strings.Cut(strings.TrimSpace(spec), ":")
	if !ok || strings.TrimSpace(method) != "say" {
		return "", false
	}
	return payload, true
}

// Say steers a running agent: the text arrives as keystrokes on its terminal.
//
// It is a thin call over the control socket agentpty serves, and it exists here
// so a caller does not have to know that a "steer" is really a pty write. What
// the caller does have to know is that a tool with Steerable=false may ignore it.
func Say(ctlSock, text string) error { return agentpty.BrokerSay(ctlSock, text) }
