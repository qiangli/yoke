package agentlaunch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
)

// Options controls launch rendering for callers that need to layer local
// policy such as read-only review or a codex sandbox override.
type Options struct {
	Sandbox  string
	ReadOnly bool
	// Workspace is the orchestrator-allocated project directory. Tools opt in to
	// receiving it through their fleet launch metadata; an empty value leaves
	// historical argv unchanged. Callers may pass fleet.WorkspaceToken when the
	// concrete path will only be known immediately before exec.
	Workspace string
	// Attended strips the auto-approve kill-switches (leaving the tool's own
	// approval gate ON and its write capability intact) for a session a human is
	// driving. Unlike ReadOnly it does NOT pin the sandbox read-only. See
	// StripKillSwitches. Overridden by AllowUnsafe.
	Attended bool
	// AllowUnsafe is the operator's explicit acceptance of unattended full access
	// (the --yolo flag): keep the approval-gate kill-switches AND skip the
	// uncontained-host guard, for a session supervised remotely via steer rather
	// than at the agent's terminal.
	AllowUnsafe bool
	DryRun      bool

	// Steer resolves the tool's INTERACTIVE launch (steer_exec) instead of its
	// headless one-shot, because a one-shot has nothing to interrupt: it runs the
	// prompt and exits before any steer could arrive.
	//
	// Fails loudly when the tool has no steerable launch. A caller that asked to
	// be able to interrupt an agent must not be silently handed one it cannot.
	Steer bool

	// Fork resolves the tool's context-inheriting FORK launch (fork_exec) instead
	// of the fresh headless one-shot: the spawned session inherits the caller's live
	// transcript (this is `delegate self`). Session is the current session id the
	// fork template substitutes for {session}. Fails loudly when the tool declares
	// no fork_exec — the caller (delegate) checks CanFork first and falls back to a
	// fresh instance, so reaching this error means a fork was forced on a tool that
	// cannot do one.
	Fork    bool
	Session string

	// ACP resolves the tool's ACP launch (acp_exec) instead of its headless or
	// steerable one — the transport in pkg/chat/acp.go, rungs 1 and 2.
	//
	// It is a launch KIND like Steer and Fork, and for the same reason: the ACP
	// argv is its own template with its own {model} slot, and a tool may declare
	// it and NOTHING else. Resolving such a tool through the headless template
	// asks a question it has no answer to, and the launcher refuses a perfectly
	// drivable binding on the strength of a template it will never run.
	//
	// Fails loudly when the tool declares no acp_exec. chat's ACP branch treats
	// that failure as "not mine" and falls through to the rung below, so a tool
	// that never declared ACP is untouched.
	ACP bool
}

// Launch is a fully resolved agent invocation. Args excludes the prompt.
type Launch struct {
	Nick      string
	Tool      string
	ToolName  string
	Model     string
	ModelName string
	Args      []string
	// WorkspacePreflight is a fully resolved, read-only command (including its
	// reporting prompt) whose {workspace} token is bound immediately before exec.
	// Empty means the tool does not declare a workspace preflight.
	WorkspacePreflight []string
	// PreserveEnv contains environment-variable names whose launcher-owned
	// credential entries must survive the child credential firewall. Values are
	// never stored in a Launch.
	PreserveEnv []string

	// TakesPrompt reports whether the prompt goes on the command line. A steerable
	// launch may open an EMPTY session (codex, opencode) and expect the first
	// message to arrive over the control channel instead.
	TakesPrompt bool
}

func (l Launch) Binding() string {
	if l.ModelName == "" {
		return ""
	}
	return l.ToolName + ":" + l.ModelName
}

func (l Launch) Named() bool {
	return l.Nick != "" && l.Nick != l.ToolName && !strings.Contains(l.Nick, ":")
}

func (l Launch) Argv(prompt string) []string {
	out := make([]string, 0, len(l.Args)+2)
	out = append(out, l.Tool)
	out = append(out, l.Args...)
	return append(out, prompt)
}

// RenderWorkspace replaces the generic workspace placeholder in argv. It is
// intentionally tool-agnostic: the fleet profile, not weave, chooses which CLI
// flag carries the path.
func RenderWorkspace(argv []string, workspace string) ([]string, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("agent launch: allocated workspace is empty")
	}
	out := append([]string(nil), argv...)
	for i, arg := range out {
		out[i] = strings.ReplaceAll(arg, fleet.WorkspaceToken, workspace)
	}
	for _, arg := range out {
		if strings.Contains(arg, fleet.WorkspaceToken) {
			return nil, fmt.Errorf("agent launch: unresolved %s in argv", fleet.WorkspaceToken)
		}
	}
	return out, nil
}

const workspacePreflightPrompt = "Report the absolute current working directory or project directory. Do not modify files. Print exactly PWD=<absolute-path> and nothing else."

type LaunchProfile struct {
	Args       []string
	UnsafeArgs []string
}

var SeededProfiles = map[string]LaunchProfile{
	// -p is print mode, and it is stated rather than inferred. Without it claude
	// decides it is headless by looking at whether stdout is a terminal — true on
	// a pipe, FALSE the moment the agent is given a PTY (to clear a trust prompt,
	// or to be steered mid-run), at which point it opens its REPL and waits
	// forever. Every other tool here already declares its headless mode; claude
	// was the only one relying on the guess.
	"claude":   {Args: []string{"-p"}, UnsafeArgs: []string{"--dangerously-skip-permissions"}},
	"codex":    {Args: []string{"exec", "--skip-git-repo-check", "--sandbox", "workspace-write"}},
	"agy":      {Args: []string{"--print-timeout", "40m", "-p"}, UnsafeArgs: []string{"--dangerously-skip-permissions"}},
	"opencode": {Args: []string{"run"}, UnsafeArgs: []string{"--auto"}},
	"aider":    {Args: []string{"--no-git", "--message"}, UnsafeArgs: []string{"--yes-always"}},
	"ycode":    {Args: []string{"prompt", "--print"}, UnsafeArgs: []string{"--danger-skip-permissions"}},
}

var NewCatalog = func() *fleet.Catalog { return fleet.New() }

type CatalogFunc func() *fleet.Catalog

func Resolve(name string, opt Options) (Launch, error) {
	return ResolveWithCatalog(name, opt, NewCatalog)
}

func ResolveWithCatalog(name string, opt Options, newCatalog CatalogFunc) (Launch, error) {
	if newCatalog == nil {
		newCatalog = NewCatalog
	}
	lnch := Launch{Nick: name, Tool: name, ToolName: name}
	cat := newCatalog()

	var toolName, modelName string
	namedAgent := false
	if a, ok := cat.Agent(name); ok {
		toolName, modelName, lnch.Nick = a.Tool, a.Model, a.Name
		namedAgent = true
	} else if t, m, ok := strings.Cut(name, ":"); ok && t != "" && m != "" {
		toolName, modelName = t, m
	} else {
		toolName = name
	}
	lnch.Tool, lnch.ToolName = toolName, toolName

	tool, known := cat.Tool(toolName)

	// The model id is resolved AFTER the tool is known, because the id a model
	// answers to is a property of the TOOL: litellm wants `deepseek/deepseek-v4-pro`,
	// ycode wants the bare `deepseek-v4-pro`, agy wants `Gemini 3.1 Pro (High)`.
	// Resolving it first — as this used to — hands somebody the wrong string, and
	// a wrong model id is a dead binding that looks perfectly healthy right up
	// until an agent tries to speak.
	if modelName != "" {
		lnch.Model, lnch.ModelName = modelName, modelName
		if m, ok := cat.Model(modelName); ok {
			lnch.Model, lnch.ModelName = m.TargetFor(toolName), m.Name
			lnch.PreserveEnv = append(lnch.PreserveEnv,
				secrets.CredentialEnvNames(tool.CredentialRefFor(m))...)

		}
	}
	if namedAgent && !known {
		return lnch, fmt.Errorf("agent launch: agent %q names tool %q, which is not in the catalog (see `bashy tool list`)", name, toolName)
	}
	if known {
		lnch.Tool = tool.Binary()
		// A BOUND MODEL REFUSES ACP, and the caller falls to the rung below —
		// which delivers the model the way it always has.
		//
		// ACP carries no model-selection call, and the ACP-native tools take no
		// model flag: `opencode acp --model moonshot/kimi-k3` prints its help
		// instead of speaking the protocol (measured 2026-07-27 with a real
		// binding out of `bashy agent list`, not an invented one). So driving
		// opencode-kimi-k3 over ACP would exec byte-for-byte the same
		// `opencode acp` process as opencode-deepseek-v4-pro, and all four
		// opencode agents — L2 through L3 — would collapse into whatever model
		// that install happens to be configured for.
		//
		// That is the fleet-evidence invariant violated exactly: `--min-band 3`
		// would be SATISFIED BY THE ABSENCE of any check. A slower transport is a
		// cost; a band that silently lies is a defect. So ACP serves tool-level
		// bindings only, and a tool:model binding takes a rung that can honour it.
		if opt.ACP && lnch.Model != "" {
			return lnch, fmt.Errorf("agent launch: %q binds model %q, which cannot be delivered over ACP — the protocol has no model selection and %q takes no model flag in ACP mode; use a rung that can carry it",
				name, lnch.Model, tool.Name)
		}
		if lnch.Model != "" && !tool.TakesModel() {
			return lnch, fmt.Errorf("agent launch: tool %q cannot select a model, so %q is a label, not a selection (its launch template has no %s)",
				tool.Name, name, fleet.ModelToken)
		}
		// The ACP launch resolves from its own template and returns here: it has
		// no workspace preflight (that is a headless, prompt-carrying command) and
		// no prompt in argv at all — an ACP prompt travels in the session.
		if opt.ACP {
			bin, args, ok := ACPArgv(tool, lnch.Model)
			if !ok {
				return lnch, fmt.Errorf("agent launch: %q cannot be driven over ACP — tool %q declares no ACP launch (acp_exec)", name, tool.Name)
			}
			out, err := FinalizeArgs(tool.Name, args, opt)
			if err != nil {
				return lnch, err
			}
			lnch.Tool, lnch.Args, lnch.TakesPrompt = bin, out, false
			return lnch, nil
		}
		render := func(m string) ([]string, bool) {
			return tool.ArgvPrefixWithWorkspace(opt.Workspace, m)
		}
		if opt.Steer {
			render = func(m string) ([]string, bool) {
				return tool.SteerArgvPrefixWithWorkspace(opt.Workspace, m)
			}
		} else if opt.Fork {
			session := opt.Session
			render = func(m string) ([]string, bool) {
				return tool.ForkArgvPrefixWithWorkspace(opt.Workspace, m, session)
			}
		}
		if args, ok := render(lnch.Model); ok {
			out, err := FinalizeArgs(tool.Name, args, opt)
			if err != nil {
				return lnch, err
			}
			lnch.Args = out
			if preflight, ok := tool.WorkspacePreflightArgv(opt.Workspace, lnch.Model, workspacePreflightPrompt); ok {
				preflightArgs, err := FinalizeArgs(tool.Name, preflight[1:], opt)
				if err != nil {
					return lnch, err
				}
				lnch.WorkspacePreflight = append([]string{preflight[0]}, preflightArgs...)
			}
			lnch.TakesPrompt = !opt.Steer || tool.SteerTakesPrompt()
			return lnch, nil
		}
		if opt.Steer {
			return lnch, fmt.Errorf("agent launch: %q cannot be steered — tool %q declares no interactive launch (steer_exec). "+
				"Its headless launch is a one-shot: it runs the prompt and exits, so there is nothing to interrupt", name, tool.Name)
		}
		if opt.Fork {
			return lnch, fmt.Errorf("agent launch: %q cannot be forked — tool %q declares no fork_exec (a headless, non-mutating context fork). "+
				"delegate self should have detected this and started a fresh instance instead", name, tool.Name)
		}
	}

	if lnch.Model != "" {
		return lnch, fmt.Errorf("agent launch: no launch template for tool %q, so model %q cannot be passed to it; add it with `bashy tool add`",
			toolName, modelName)
	}

	// Neither can an ACP launch, and for the same reason the steer guard below
	// spells out: below this line there is no catalog entry, so no acp_exec, and
	// the seeded-profile fallback would hand back a headless argv for a caller
	// that is about to speak JSON-RPC at it.
	if opt.ACP {
		return lnch, fmt.Errorf("agent launch: %q cannot be driven over ACP — tool %q is not in the catalog, "+
			"so there is no ACP launch to resolve (see `bashy tool list`)", name, toolName)
	}

	// A STEER cannot be resolved from a fallback.
	//
	// Below this line is the unknown-tool path: no catalog entry, so no launch
	// template, so no `steer_exec` — we are guessing an argv from a seeded profile
	// or from nothing at all. That guess is a reasonable headless one-shot (it is
	// how you run an agent CLI the registry has never heard of), and it is a
	// catastrophic steerable session: the caller would get a process that exits
	// immediately and a control socket nobody is listening on, and every symptom
	// would look like success.
	//
	// This was the bug. `known` is false here, so the "cannot be steered" check
	// above never ran, and opt.Steer fell straight through to a headless argv —
	// a steerable session resolved by the ABSENCE of a tool definition.
	if opt.Steer {
		if !known {
			return lnch, fmt.Errorf("agent launch: %q cannot be steered — tool %q is not in the catalog, "+
				"so there is no interactive launch to resolve (see `bashy tool list`)", name, toolName)
		}
		return lnch, fmt.Errorf("agent launch: %q cannot be steered — tool %q declares no interactive launch (steer_exec)", name, toolName)
	}

	prof := SeededProfiles[toolName]
	base := append([]string{}, prof.Args...)
	if len(prof.UnsafeArgs) > 0 {
		if ok, _ := UnsafeLaunchAllowed(); ok {
			base = append(append([]string{}, prof.UnsafeArgs...), base...)
		}
	}
	out, err := FinalizeArgs(toolName, base, opt)
	if err != nil {
		return lnch, err
	}
	lnch.Args = out
	lnch.TakesPrompt = true // a headless one-shot always takes its prompt on the command line
	return lnch, nil
}

const UnsafeLaunchEnv = "BASHY_ALLOW_UNSAFE_AGENT_LAUNCH"

var Containerized = func() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func UnsafeLaunchAllowed() (bool, string) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(UnsafeLaunchEnv))) {
	case "", "0", "false", "off", "no":
	default:
		return true, UnsafeLaunchEnv + " is set"
	}
	if Containerized() {
		return true, "running inside a container"
	}
	return false, ""
}

var UnsafeLaunchFlags = map[string]string{
	"--dangerously-skip-permissions":             "disables the agent's approval gate",
	"--dangerously-bypass-approvals-and-sandbox": "disables the agent's approval gate AND its sandbox",
	"--yolo":       "disables the agent's approval gate",
	"--yes-always": "auto-confirms every action, disabling the agent's approval gate",
	"--auto":       "auto-approves the agent's permission requests",

	// ycode's own. It belongs here for the same reason as everyone else's: the
	// guard must REFUSE it on an uncontained host, and ReadOnly must STRIP it so a
	// judge or a meeting participant cannot touch the thing it is reviewing.
	//
	// It was missing, and the omission was invisible in the only way that matters:
	// ycode's exec template never passed it either, so nothing ever tripped the
	// guard and nothing ever looked wrong. The model would work out the answer,
	// discover it had no write permission, print the code into its reply, and exit
	// 0. Measured in a three-harness bake-off — ycode produced a correct
	// implementation and wrote NOTHING to disk, and it said so:
	//
	//   "I have the implementation ready, but I don't have workspace-write
	//    permissions in this environment to write the file."
	"--danger-skip-permissions": "disables the agent's approval gate",
}

func GuardUnsafeArgs(tool string, args []string) error {
	if ok, _ := UnsafeLaunchAllowed(); ok {
		return nil
	}
	for i, a := range args {
		why, bad := UnsafeLaunchFlags[a]
		if !bad && a == "--sandbox" && i+1 < len(args) && args[i+1] == "danger-full-access" {
			why, bad = "disables the agent's sandbox", true
			a = "--sandbox danger-full-access"
		}
		if !bad {
			continue
		}
		return fmt.Errorf(`agent launch: refusing to launch %q with %s

%s, giving it unattended full access to this machine - and nothing here is
containing it. Choose one:

  - contain it     run the fleet inside a container (e.g. bashy podman), or
  - accept it      %s=1 (explicit, logged risk acceptance)`,
			tool, a, why, UnsafeLaunchEnv)
	}
	return nil
}

func FinalizeArgs(tool string, args []string, opt Options) ([]string, error) {
	// "allowed" = the operator has EXPLICITLY accepted unattended full access —
	// the --yolo flag (opt.AllowUnsafe), $BASHY_ALLOW_UNSAFE_AGENT_LAUNCH, or a
	// container. When so, the approval gate stays OFF (keep the kill-switches): a
	// session supervised REMOTELY via steer cannot answer a prompt at a terminal
	// nobody is sitting at, and the uncontained-host guard has nothing to refuse.
	allowed := opt.AllowUnsafe
	if !allowed {
		allowed, _ = UnsafeLaunchAllowed()
	}
	switch {
	case opt.ReadOnly:
		args = ReadOnlyArgs(tool, args) // a reviewer must never write — always strip
	case opt.Attended && !allowed:
		args = StripKillSwitches(tool, args) // a human is at the terminal → approval ON
	}
	args = ApplySandbox(tool, args, opt)
	args = normalizeUnsafeFlags(tool, args)
	if opt.DryRun {
		return args, nil
	}
	if !allowed {
		if err := GuardUnsafeArgs(tool, args); err != nil {
			return nil, err
		}
	}
	return args, nil
}

// StripKillSwitches removes ONLY the auto-approve kill-switch flags
// (--dangerously-skip-permissions and kin), leaving write capability and the
// tool's sandbox default intact. It is the attended-session transform: a human
// is present, so the tool's own approval gate should be ON (prompting the human)
// rather than skipped — which also means the uncontained-host guard has nothing
// to refuse. A `--sandbox danger-full-access` pair is downgraded to the tool's
// default (drop both tokens), since that too disables containment.
func StripKillSwitches(tool string, args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if _, unsafe := UnsafeLaunchFlags[args[i]]; unsafe {
			continue
		}
		if args[i] == "--sandbox" && i+1 < len(args) && args[i+1] == "danger-full-access" {
			i++ // drop the value too; the tool falls back to its default sandbox
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func ReadOnlyArgs(tool string, args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if _, unsafe := UnsafeLaunchFlags[args[i]]; unsafe {
			continue
		}
		if args[i] == "--sandbox" && i+1 < len(args) {
			out = append(out, "--sandbox", "read-only")
			i++
			continue
		}
		out = append(out, args[i])
	}
	if tool == "codex" && !containsArg(out, "--sandbox") {
		out = append(out, "--sandbox", "read-only")
	}
	return out
}

func ApplySandbox(agent string, args []string, opt Options) []string {
	if agent == "codex" {
		sb := strings.TrimSpace(opt.Sandbox)
		switch {
		case sb == "danger-full-access":
			return replaceSandboxFlag(args, "--dangerously-bypass-approvals-and-sandbox")
		case sb != "":
			// An explicit safe sandbox override also removes a stale bypass
			// flag from a host-local profile. Supplying both makes the bypass
			// win, which would turn a requested read-only/workspace-write launch
			// into full host access.
			return replaceSandboxMode(args, sb)
		case opt.AllowUnsafe && !opt.ReadOnly:
			// --yolo with no explicit override. codex's approval gate is a SANDBOX
			// VALUE, not a boolean kill-switch, so --yolo has to map it to codex's own
			// bypass flag — otherwise an unattended codex prompts on every action (its
			// "always allow" is per-project, so it re-prompts each new dir), and a
			// remotely-driven fleet agent has no one at the terminal to answer. Only
			// the explicit flag does this — merely permitting unsafe launches (the env
			// var / a container) does NOT, so a default codex still gets workspace-write.
			return replaceSandboxFlag(args, "--dangerously-bypass-approvals-and-sandbox")
		}
	}
	return args
}

func PrincipalEnv(base []string, l Launch) []string {
	if !l.Named() {
		return base
	}
	out := make([]string, 0, len(base)+4)
	for _, kv := range base {
		switch {
		case strings.HasPrefix(kv, "BASHY_PRINCIPAL="),
			strings.HasPrefix(kv, "BASHY_AGENT_ID="),
			strings.HasPrefix(kv, "BASHY_AGENT_BINDING="):
		default:
			out = append(out, kv)
		}
	}
	out = append(out,
		"BASHY_PRINCIPAL=dhnt:agent/"+l.Nick,
		"BASHY_AGENT_ID="+l.Nick,
	)
	if b := l.Binding(); b != "" {
		out = append(out, "BASHY_AGENT_BINDING="+b)
	}
	return out
}

type launchKey struct{}

func WithLaunch(ctx context.Context, l Launch) context.Context {
	return context.WithValue(ctx, launchKey{}, l)
}

func LaunchFrom(ctx context.Context) (Launch, bool) {
	l, ok := ctx.Value(launchKey{}).(Launch)
	return l, ok
}

// Per-run agent store isolation.
//
// ycode locks its data directory: a second ycode process pointed at the same
// store dies on launch with "storage ... is locked by another ycode process".
// An orchestrator that runs N agents concurrently (weave workers, parallel chat
// sessions) must therefore give each ycode run its OWN store, or it inherits a
// hidden per-tool concurrency limit of 1 — exactly the bug two weave workers
// hit, where each worker got an isolated git workspace but they still shared one
// global data store.
//
// ycode names the knobs: YCODE_DATA_DIR (just the store) and YCODE_HOME (the
// whole tree). This helper derives a per-run YCODE_DATA_DIR so the operator
// never has to know it exists. It lives in agentlaunch — the shared launch
// layer — so every launch surface (weave start, bashy chat, coach) applies the
// SAME rule rather than each growing its own copy that would drift. The
// related #160 (env not reaching a coach-launched child) is the same family of
// "two env-construction paths diverged" bug; centralizing this here keeps this
// fix from being the third.

// YcodeToolName is the fleet registry tool name ycode is keyed under. The
// per-run store injection is gated on the TOOL, not an agent nickname or a
// hardcoded argv sniff, so a non-ycode tool is untouched and any ycode binding
// (every nickname on tool=ycode) qualifies.
const YcodeToolName = "ycode"

// The two ycode environment knobs a launcher may set to relocate the store. An
// operator who set either one already has made a deliberate choice; a launcher
// must NEVER override it (it would silently relocate or collide their store).
const (
	YcodeDataDirEnv = "YCODE_DATA_DIR"
	YcodeHomeEnv    = "YCODE_HOME"
)

// YcodeDataDir derives a per-run data directory for a ycode-family agent, so two
// concurrently-launched ycode workers do not share one locked store.
//
// It returns "" (inject nothing) when:
//   - the launch is not a ycode tool — non-ycode tools are left untouched;
//   - stateDir or runID is empty — no stable identity to derive from;
//   - the operator already set YCODE_DATA_DIR or YCODE_HOME in parentEnv — a
//     deliberate choice is never overridden (this is why the bug is invisible
//     to an operator who happened to set one: their binding keeps working).
//
// The returned path is stateDir + runID-derived: stable across a --resume of
// the SAME run (a resumed run reuses its store rather than orphaning a new one
// every time), and distinct between two DIFFERENT runs. The caller MUST pass an
// out-of-tree stateDir (e.g. the weave queue dir, or a ~/.bashy state dir) —
// never the agent's git workspace, or the store pollutes the tree and the diff.
func YcodeDataDir(parentEnv []string, l Launch, stateDir, runID string) string {
	if l.ToolName != YcodeToolName {
		return ""
	}
	stateDir = strings.TrimSpace(stateDir)
	runID = strings.TrimSpace(runID)
	if stateDir == "" || runID == "" {
		return ""
	}
	if envHasName(parentEnv, YcodeDataDirEnv) || envHasName(parentEnv, YcodeHomeEnv) {
		return ""
	}
	return filepath.Join(stateDir, "agent-data", "ycode-"+sanitizeRunID(runID))
}

// ApplyYcodeDataDir appends YCODE_DATA_DIR to env when YcodeDataDir derives a
// path for this launch, and returns env unchanged otherwise. An existing entry
// is never duplicated or overwritten. parentEnv is the launcher's own (pre-scrub)
// environment, used only to honor an operator-set value.
func ApplyYcodeDataDir(env, parentEnv []string, l Launch, stateDir, runID string) []string {
	dir := YcodeDataDir(parentEnv, l, stateDir, runID)
	if dir == "" || envHasName(env, YcodeDataDirEnv) {
		return env
	}
	return append(env, YcodeDataDirEnv+"="+dir)
}

var runIDSep = strings.NewReplacer(
	string(filepath.Separator), "_",
	string(os.PathListSeparator), "_",
	" ", "_",
)

// sanitizeRunID flattens a run identity into a single path component so a
// caller-supplied id (an issue number, a "tool-pid" session handle) can never
// escape the agent-data/<name> slot or collide with a sibling directory.
func sanitizeRunID(id string) string {
	s := runIDSep.Replace(id)
	s = strings.Trim(s, "-_.")
	if s == "" {
		s = "run"
	}
	return s
}

// envHasName reports whether env contains a NAME=... entry for name. Shared by
// the store-isolation helpers; values are never inspected.
func envHasName(env []string, name string) bool {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// SendControlFrame is agentpty.SendFrame.
//
// This package used to carry its own copy of the transport — the same twenty
// lines, dialling the same socket, with the same file fallback. Two copies of one
// protocol is one protocol that will drift, and the comment in agentpty explaining
// why they were kept apart ("a terminal does not need to know what claude is") had
// the dependency backwards: agentpty depends on nothing, so anyone may import IT.
func SendControlFrame(path, frame string) error {
	return agentpty.SendFrame(path, frame)
}

func SendControlLine(path, line string) error {
	return SendControlFrame(path, line+"\n")
}

func SendJSONControl(path string, command, ack any, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(command); err != nil {
		return err
	}
	if ack == nil {
		return nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	return json.NewDecoder(conn).Decode(ack)
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func replaceSandboxFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args)+1)
	for i := 0; i < len(args); i++ {
		if args[i] == "--sandbox" {
			if i+1 < len(args) {
				i++
			}
			continue
		}
		// A host-local tool override may already carry the canonical bypass
		// flag. AllowUnsafe is a policy transform, not a second request to the
		// CLI, so normalize it to one occurrence. Codex rejects duplicates.
		if args[i] == flag {
			continue
		}
		out = append(out, args[i])
	}
	return append(out, flag)
}

func replaceSandboxMode(args []string, mode string) []string {
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--sandbox":
			if i+1 < len(args) {
				i++
			}
		case "--dangerously-bypass-approvals-and-sandbox":
			// The explicit sandbox mode supersedes the bypass.
		default:
			out = append(out, args[i])
		}
	}
	return append(out, "--sandbox", mode)
}

var canonicalUnsafeFlag = map[string]string{
	"codex":    "--dangerously-bypass-approvals-and-sandbox",
	"claude":   "--dangerously-skip-permissions",
	"agy":      "--dangerously-skip-permissions",
	"opencode": "--auto",
	"aider":    "--yes-always",
	"ycode":    "--danger-skip-permissions",
}

// normalizeUnsafeFlags makes a fleet template's dangerous-permission request
// valid for the selected CLI. Local overrides survive baseline upgrades, so an
// old template can otherwise combine a former spelling with a newly applied
// one. Keep at most one canonical flag and discard another tool's spelling.
func normalizeUnsafeFlags(tool string, args []string) []string {
	canonical := canonicalUnsafeFlag[tool]
	seen := make(map[string]bool)
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if _, unsafe := UnsafeLaunchFlags[arg]; !unsafe {
			out = append(out, arg)
			continue
		}
		if canonical != "" && arg != canonical {
			continue
		}
		if seen[arg] {
			continue
		}
		seen[arg] = true
		out = append(out, arg)
	}
	return out
}

var ErrNoAgent = errors.New("agent launch: not an agent")

// RegistrationRefusal is the shared fail-closed answer for a launch identity no
// `bashy agent` record owns.
//
// IT IS NOT USED HERE, deliberately. Resolution lets an unregistered binding
// LAUNCH: weave's TestUnNicknamedBindingIsNotAPrincipal pins that `aider:opus`
// runs and simply is not stamped as a principal, and weaveAgentEnv returns the
// base environment unchanged for it. That is the codebase's own answer to
// "registration precedes use", and it is a better one than refusing the launch:
// registration gates IDENTITY, not EXECUTION. An unregistered binding is a
// command; it just cannot be addressed, which is exactly the property that
// matters.
//
// So the refusal belongs at the call sites that REQUIRE an identity — a chat
// session that is addressable and keeps a conversation store, or a roster entry
// — and nowhere else. It names the rejected spelling AND the exact
// command that fixes it, deriving tool and model from a tool:model spelling so
// the suggestion is usable rather than generic.
func RegistrationRefusal(name string) error {
	name = strings.TrimSpace(name)
	tool, model, bound := strings.Cut(name, ":")
	if !bound || tool == "" || model == "" {
		tool, model = name, "<model>"
	}
	return fmt.Errorf("agent launch: %q is not a registered Bashy agent; register it before launching:\n  bashy agent add %s --tool %s --model %s",
		name, name, tool, model)
}
