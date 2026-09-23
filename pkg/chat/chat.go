package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/procguard"
	"github.com/qiangli/yoke/pkg/recall"
	"github.com/qiangli/yoke/pkg/reduce"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/secrets"
	"github.com/qiangli/yoke/pkg/telemetry"
	"github.com/spf13/cobra"
)

const schemaVersion = "bashy-chat-v1"

// Options describes one unattended agent invocation. It is exported so workflow
// commands such as sdlc can use the same primitive as human operators.
type Options struct {
	Agent       string
	Role        string
	Task        string
	Instruction string
	Files       []string
	Context     []string
	Cwd         string
	Timeout     time.Duration
	Sandbox     string
	JSON        bool
	DryRun      bool
	// ReadOnly launches the agent with NO write authority: the approval-gate
	// kill-switches are stripped from its argv and a sandboxing tool is pinned to
	// its read-only mode.
	//
	// It exists for `bashy judge`, and the reason is an integrity property, not a
	// convenience: A REVIEWER MUST NOT BE ABLE TO MODIFY WHAT IT REVIEWS. An agent
	// with write access could "fix" the code and then approve its own fix, and the
	// verdict would be worthless. It is also simply unnecessary — judge passes the
	// diff INLINE in the prompt, so the reviewer needs no filesystem access to do
	// its job, which means it can be denied all of it.
	ReadOnly bool

	// KillOnParentExit makes an unattended one-shot's inherited process group
	// self-clean when the caller disappears. Meet turns opt into this mode
	// because a participant is a bounded conversation, not a durable worker. A
	// descendant that deliberately calls setpgid/setsid needs an OS containment
	// backend and is outside the portable guarantee (procguard reports this
	// boundary explicitly). It is deliberately opt-in: actions and login-backed
	// sessions must survive the launcher/terminal going away. Unsupported
	// platforms fail closed instead of silently launching without the guard.
	KillOnParentExit bool

	// Steer resolves the tool's INTERACTIVE launch (its `steer_exec:` template)
	// instead of its headless one-shot, so the process stays alive and can be
	// talked to mid-turn. Set by Start; a one-shot Invoke has nothing to steer.
	//
	// It fails loudly on a tool that declares no interactive launch, rather than
	// handing back a one-shot that will have exited before the first steer lands.
	Steer bool

	// Attended marks a session a HUMAN is driving (bashy chat interactive). It
	// strips the auto-approve kill-switches (--dangerously-skip-permissions and
	// kin) from the launch — WITHOUT pinning read-only like ReadOnly does — so the
	// tool runs in its normal interactive mode with its OWN approval gate active
	// and full write capability. The human is the containment, so the launch is
	// both safer (it prompts instead of auto-approving) and passes the uncontained-
	// host guard, exactly as running the tool by hand would.
	Attended bool

	// AllowUnsafe is the operator's explicit --yolo acceptance: keep the approval
	// gate OFF (do not strip the kill-switches) AND skip the uncontained-host
	// guard — for a session supervised REMOTELY via steer, where no one sits at the
	// agent's terminal to answer an approval prompt.
	AllowUnsafe bool
	// AllowPremium is the explicit human override for urgent work that may cross
	// LLM budget or subscription exhaustion bounds.
	AllowPremium bool

	// Fork resolves the tool's context-inheriting fork launch (fork_exec): the
	// spawned session inherits the caller's live transcript. Session is the current
	// session id substituted into the fork template. Set by `delegate self` when the
	// current tool CanFork and a session id is readable; otherwise it delegates to a
	// fresh instance instead.
	Fork    bool
	Session string

	// ACP resolves the tool's ACP launch (acp_exec) instead of its headless or
	// steerable one. Set only by startACPSession, and only behind the BASHY_ACP
	// gate; every other launch in the fleet leaves it false and is resolved by
	// the identical code that ran before ACP existed.
	ACP bool

	// Stream, when set, receives the agent's stdout AS IT IS WRITTEN, in
	// addition to the captured Result. It is a tee, not a redirect: the caller
	// still gets the whole output at the end, so nothing downstream has to
	// change to keep working.
	//
	// Only stdout is teed. Stderr is the agent CLI's chrome — banners, spinners,
	// progress bars — and streaming it would show a watcher the harness talking
	// rather than the agent. The answer is on stdout.
	//
	// Honoured only by the default runner: an injected Runner captures output
	// however it likes, and it is not this package's business to make it stream.
	Stream io.Writer

	// EventStream, when set, receives the tool's STRUCTURED-EVENT side-channel
	// (ycode's NDJSON events file), framed as complete lines, SEPARATELY from
	// Stream (which carries the stdout prose tee). These are two concurrent
	// sources: the stdout tee flushes arbitrary chunks that may end mid-line, while
	// the event goroutine appends whole JSON envelopes. Teeing both into one
	// io.Writer lets a torn stdout chunk splice onto the front of the next event
	// line, so a consumer that frames by newline can no longer tell the two apart —
	// giving each source its own sink lets the consumer keep per-source line
	// boundaries. When nil, the event side-channel falls back to Stream (the prior
	// behaviour), so a caller that does not care keeps working unchanged.
	EventStream io.Writer

	// PTY runs the agent attached to a pseudo-terminal instead of a pipe.
	//
	// Agent CLIs are TUIs, and several of them stop dead on a pipe: the first
	// thing they do in an unfamiliar directory is ask "do you trust the contents
	// of this folder?" — on a terminal, expecting a keystroke. A launcher that
	// cannot answer produces not a slow agent but a silent one, which then times
	// out reporting the wrong cause. `agy` does this on EVERY launch. Under a PTY
	// the prompt is seen and cleared (pkg/agentpty), and the agent just works.
	//
	// It also opens CtlSock, which is the only way to steer a running agent.
	//
	// The cost is that a PTY has one stream: stdout and stderr merge, so an
	// agent's CLI chrome lands in the captured turn alongside its answer. Pipe
	// mode keeps them apart. So this is opt-in per call, not a default.
	//
	// Windows has no PTY here; the runner falls back to a pipe rather than
	// failing. A Windows user loses trust-clearing and steering — degraded, not
	// broken.
	PTY bool

	// CtlSock is the unix socket an operator writes to in order to steer this
	// agent mid-run (see agentpty). Only meaningful with PTY.
	CtlSock string
}

// Result is the stable envelope returned by Invoke and optionally printed by
// the CLI.
//
// Agent stays the executable that ran, so every existing consumer keeps
// working. Nick and Model are additive: they record what the caller asked for
// and which inference backend was actually selected.
type Result struct {
	SchemaVersion string   `json:"schema_version"`
	Agent         string   `json:"agent"`
	Nick          string   `json:"nick,omitempty"`  // the name the caller used: 007, claude:opus, claude
	Model         string   `json:"model,omitempty"` // the provider-side id passed to the tool
	Role          string   `json:"role,omitempty"`
	Cwd           string   `json:"cwd,omitempty"`
	Args          []string `json:"args,omitempty"`
	DryRun        bool     `json:"dry_run,omitempty"`
	ExitCode      int      `json:"exit_code"`
	Output        string   `json:"output,omitempty"`
}

// LaunchProfile is the minimal headless launch contract needed by chat.
//
// Deprecated: launch contracts now live in the fleet registry
// (coreutils/pkg/fleet), where one declaration serves chat, weave, and the
// capability matrix. seededProfiles remains only as the fallback for a tool
// the registry does not know.
type LaunchProfile struct {
	// Args is the SAFE launch argv — the agent runs under its own approval gate
	// / sandbox. The prompt is appended after these, so any flag that consumes
	// the prompt (aider's --message, agy's -p) stays last.
	Args []string
	// UnsafeArgs are the agent's own approval-gate kill-switches. They are
	// prepended to Args ONLY when unsafeLaunchAllowed() (an explicit opt-in or a
	// verified container) — never by default. Prepending keeps a prompt-consuming
	// trailing flag last.
	UnsafeArgs []string
}

func toAgentLaunchOptions(opt Options) agentlaunch.Options {
	return agentlaunch.Options{
		Sandbox:     opt.Sandbox,
		ReadOnly:    opt.ReadOnly,
		Attended:    opt.Attended,
		AllowUnsafe: opt.AllowUnsafe,
		DryRun:      opt.DryRun,
		Steer:       opt.Steer,
		Fork:        opt.Fork,
		Session:     opt.Session,
		ACP:         opt.ACP,
	}
}

// Launch is a fully resolved invocation: which binary, which model, which argv.
//
// Tool and Model are what the OS needs — an executable and the provider's own
// model id. ToolName and ModelName are what the REGISTRY calls them, and they
// are what attribution records: a binding written with a binary path would not
// match the capability matrix, whose rows are keyed by tool:model.
type Launch struct {
	Nick      string   // canonical agent nickname, or the bare name the caller typed
	Tool      string   // the executable to run
	ToolName  string   // the tool's registry name
	Model     string   // provider-side model id ("" when none was selected)
	ModelName string   // the model's registry alias
	Args      []string // argv after the binary, prompt NOT included
	// PreserveEnv carries environment-variable names only. The runner copies
	// matching entries from its own environment after scrubbing.
	PreserveEnv []string

	// TakesPrompt reports whether the prompt goes on the command line. A headless
	// launch always does. A STEERABLE launch sometimes does not — codex and
	// opencode open an empty session — and those get their opening message down
	// the control channel instead.
	TakesPrompt bool
}

// Binding is the capability-matrix key for this launch, or "" when no model
// was selected.
func (l Launch) Binding() string {
	if l.ModelName == "" {
		return ""
	}
	return l.ToolName + ":" + l.ModelName
}

func fromAgentLaunch(l agentlaunch.Launch) Launch {
	return Launch{
		Nick:        l.Nick,
		Tool:        l.Tool,
		ToolName:    l.ToolName,
		Model:       l.Model,
		ModelName:   l.ModelName,
		Args:        l.Args,
		PreserveEnv: append([]string(nil), l.PreserveEnv...),
		TakesPrompt: l.TakesPrompt,
	}
}

func toAgentLaunch(l Launch) agentlaunch.Launch {
	return agentlaunch.Launch{
		Nick:        l.Nick,
		Tool:        l.Tool,
		ToolName:    l.ToolName,
		Model:       l.Model,
		ModelName:   l.ModelName,
		Args:        l.Args,
		PreserveEnv: append([]string(nil), l.PreserveEnv...),
		TakesPrompt: l.TakesPrompt,
	}
}

// Runner starts an agent process. Tests and higher-level workflows can replace
// it without spawning a real agent.
type Runner interface {
	Run(ctx context.Context, agent string, args []string, cwd string) (string, int, error)
}

type execRunner struct {
	stream           io.Writer
	pty              bool
	ctlSock          string
	killOnParentExit bool
}

// runPTY runs the agent attached to a pseudo-terminal.
//
// It builds the SAME *exec.Cmd as the pipe path — same argv, same cwd, and
// crucially the same agentChildEnv — and only changes how the process is
// attached. That is why the PTY runner lives here rather than in agentpty or in
// a caller: agentChildEnv is what scrubs secrets out of the child's environment,
// forces its shell to be bashy, and stamps its principal identity. A PTY runner
// built anywhere else would silently launch agents without any of the three, and
// nothing would fail loudly enough to notice.
//
// A PTY merges stdout and stderr — a terminal has one stream — so the captured
// turn includes whatever chrome the CLI prints. That is the price of being able
// to answer the agent's questions and steer it, and it is why PTY is opt-in.
// NoteCoach surfaces a reflex-coach loop trip to the live watcher when the coach
// intervened on (or, on a socket-less pipe run, detected) a suspected loop. In
// agent mode it emits a structured `bashy-advice-v1` line — the same wire shape
// the space-time advisor uses, so an agentic tool parses a coach loop-trip
// exactly as it parses any other advice; otherwise a human-readable line. It
// writes ONLY to the observer stream, never the recorded turn — observing must
// not change the record.
func NoteCoach(c *Coach, stream io.Writer) {
	if c == nil {
		return
	}
	// Token/time tracking: record the run's spend estimate + intervention count on
	// the OTel plane EVERY run (even a clean one), so a supervisor watching the
	// plane sees the shape of every delegated run, not only the ones that looped.
	rep := c.Report()
	telemetry.Provenance(c.telCtx(), "coach.output_tokens", int64(c.OutputTokens()), "coach-estimate")
	telemetry.Provenance(c.telCtx(), "coach.interventions", int64(len(rep.Steers)), "coach")
	if stream == nil || len(rep.Steers) == 0 {
		return
	}
	unresolved := c.Unresolved()
	if agenticMode() {
		dim, suggest := "agent-loop", "the coach steered it off; if a loop recurs, the task is likely unsatisfiable or mis-scoped — re-scope rather than retry"
		if unresolved {
			dim = "coach-unresolved"
			suggest = "SUPERVISOR ACTION: the reflex and agent-coach did not resolve this loop — kill, reassign, or re-scope the run; inject a steer via `weave say`/`foreman say` if you can see the fix"
		}
		adv := map[string]any{
			"schema_version": "bashy-advice-v1",
			"kind":           "advice",
			"dimension":      dim,
			"interventions":  len(rep.Steers),
			"lines":          rep.Total,
			"distinct":       rep.Distinct,
			"output_tokens":  c.OutputTokens(),
			"hint": fmt.Sprintf("the reflex coach detected a loop: %d intervention(s) over %d output lines (%d distinct, ~%d output tokens)",
				len(rep.Steers), rep.Total, rep.Distinct, c.OutputTokens()),
			"suggest": suggest,
			"off":     "BASHY_NO_COACH",
		}
		if b, err := json.Marshal(adv); err == nil {
			fmt.Fprintf(stream, "%s\n", b)
			return
		}
	}
	tag := "[coach] suspected loop"
	if unresolved {
		tag = "[coach] ⚠ UNRESOLVED LOOP — supervisor attention needed"
	}
	fmt.Fprintf(stream, "\n%s — %d intervention(s) over %d output lines (%d distinct, ~%d output tokens)\n",
		tag, len(rep.Steers), rep.Total, rep.Distinct, c.OutputTokens())
}

// agenticMode reports whether an agentic tool is driving bashy, so hints go out
// as structured lines rather than prose.
func agenticMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BASHY_AGENTIC"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (r execRunner) runPTY(cmd *exec.Cmd, agent string) (string, int, error) {
	var buf bytes.Buffer
	sink := io.Writer(&buf)
	// Reflex coach (P2a): a pty invoke/delegate HAS a control socket, so this is
	// the full detect+steer path — the same protection weave gets. Off with
	// BASHY_NO_COACH.
	var coach *Coach
	if ReflexEnabled() {
		coach = NewLineCoach(DefaultCoachPolicy(), NewCtlSteerer(r.ctlSock))
		// P2b: a steerable invoke can escalate to an agent one band above `agent`.
		coach.SetEscalation(context.Background(), agent, BandGraduatedEscalator)
		sink = io.MultiWriter(sink, coach)
	}
	exit, killReason, err := agentpty.Run(cmd, sink, agentpty.Options{
		CtlSock:          r.ctlSock,
		KillOnParentExit: r.killOnParentExit,
		// Always capture. The caller records this turn; the human, if there is
		// one, is watching through an observer rather than typing at the agent.
		Capture: true,
	})
	NoteCoach(coach, nil)
	out := buf.String()
	if err != nil {
		return out, exit, err
	}
	if killReason != "" {
		return out, exit, fmt.Errorf("agent terminated: %s", killReason)
	}
	if exit != 0 {
		return out, exit, fmt.Errorf("%s exited %d", cmd.Path, exit)
	}
	return out, 0, nil
}

func (r execRunner) Run(ctx context.Context, agent string, args []string, cwd string) (string, int, error) {
	observeBudgetProcess(ctx, nil)
	// Preflight, so a missing CLI is reported as a missing CLI. Left to os/exec
	// it surfaces as `exec: "claude": executable file not found in $PATH` — which
	// names a $PATH the operator never set and cannot see, since the launcher is
	// usually a daemon. 127 is the shell's own "command not found".
	if ResolveToolBinary(agent) == "" {
		return "", 127, ErrToolNotFound(agent)
	}
	// macOS: a cask/download-installed agent (e.g. codex) carries
	// com.apple.quarantine, so a background/CI launch (act_runner) hangs on the
	// Gatekeeper "downloaded from the Internet" popup. The operator explicitly
	// configured this agent as the conductor — strip the quarantine best-effort
	// so the headless launch proceeds. No-op off darwin / when already clear.
	if p := ResolveToolBinary(agent); p != "" {
		stripQuarantine(p)
	}
	cmd := agentCommand(ctx, agent, args, cwd)
	defer func() { observeBudgetProcess(ctx, cmd) }()

	// Attach to a PTY once the command — and above all its environment — is
	// fully built, so the PTY path cannot diverge from the pipe path in what the
	// agent actually inherits. On Windows there is no PTY: fall through to the
	// pipe rather than failing, since a run without trust-clearing and steering
	// is degraded, not broken.
	if r.pty && agentpty.Supported() {
		// Prevention before cure. A trust prompt that never appears cannot be
		// mis-answered, cannot be raced, and costs nothing to avoid — so the
		// tool's own config is seeded first, and agentpty's reactive gate clearing
		// is the backstop for the prompts a preseed does not cover.
		//
		// Best-effort: an unwritable config is a reason to fall back on clearing
		// the prompt, never a reason to refuse to launch the agent.
		if p, ok := agentctl.ProfileFor(agent); ok && p.Preseed != "" {
			_ = agentctl.ApplyTrustPreseed(cmd.Dir, p.Preseed)
		}
		return r.runPTY(cmd, agent)
	}

	// Own process group, so cancelling this turn can reach the agent's CHILDREN.
	// Set only on the pipe path: the PTY path needs Setsid (the subagent must be a
	// session leader to own the terminal), Setsid and Setpgid together are invalid,
	// and agentpty.Run already does its own tree teardown.
	setProcessGroup(cmd)
	// Own cancellation until Wait finishes draining the pipes. os/exec's context
	// watcher stops when the direct child exits, even if descendants still hold
	// those pipes. waitForProcessTree below also retries a successful group kill:
	// on macOS a concurrent fork can miss the kernel's membership snapshot.
	cmd.Cancel = nil

	// Capture stdout and stderr SEPARATELY. The agent's actual answer is on
	// stdout; CLI chrome (banners, warnings, progress) goes to stderr and would
	// otherwise pollute a captured turn — and a truncated multibyte char in that
	// chrome becomes invalid UTF-8 that crashes a downstream tool when the turn is
	// replayed as its prompt. On success we return stdout only; on failure we
	// append stderr so the error is still visible to the caller.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Reflex coach (P2a), pipe path: a plain one-shot has NO control socket, so
	// the coach is DETECT-ONLY here (NewCtlSteerer("") is a no-op) — it watches
	// STDERR, where an agent's tool-call/progress chrome (and thus a loop) shows,
	// and notes it. A one-shot rarely loops (the finding), but when it does this
	// records it for observability + the training seed. Off with BASHY_NO_COACH.
	var coach *Coach
	if ReflexEnabled() {
		coach = NewLineCoach(DefaultCoachPolicy(), NewCtlSteerer(""))
		cmd.Stderr = io.MultiWriter(&stderr, coach)
	}
	// Because Stdout/Stderr are buffers rather than *os.File, os/exec pipes them
	// through copying goroutines, and Wait blocks until EVERY writer closes the
	// pipe. An agent CLI that spawns children (a shell, a language server) leaves
	// those children holding it, so killing the agent on ctx cancellation does NOT
	// unblock Wait — the "a wedged agent can't hang the round" timeout silently
	// waits for the grandchild instead. WaitDelay bounds that: after the context
	// ends, Wait gives the pipes this long to drain, then closes them and returns.
	cmd.WaitDelay = 5 * time.Second
	var parentGuard *procguard.Guard
	if r.killOnParentExit {
		var err error
		parentGuard, err = procguard.Arm(cmd)
		if err != nil {
			return "", 127, err
		}
	}
	startErr := cmd.Start()
	if parentGuard != nil {
		parentGuard.Started(startErr)
	}
	if startErr != nil {
		return "", 127, startErr
	}
	err := waitForProcessTree(ctx, cmd, func() error { return killProcessTree(cmd) })
	if parentGuard != nil {
		parentGuard.Disarm()
	}
	NoteCoach(coach, nil)
	out := stdout.String()
	if ctx.Err() != nil {
		if !errors.Is(err, ctx.Err()) {
			err = errors.Join(ctx.Err(), err)
		}
		return appendStderr(out, stderr.String()), 124, err
	}
	if err == nil {
		return out, 0, nil
	}
	out = appendStderr(out, stderr.String())
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), err
	}
	return out, 127, err
}

// agentCommand is the single child-process construction path for both a chat
// turn and a steerable Session (coach). The caller chooses argv and attachment;
// cwd and environment must not drift with that choice.
func agentCommand(ctx context.Context, agent string, args []string, cwd string) *exec.Cmd {
	// Resolve before constructing: exec resolves a bare name against the
	// LAUNCHER's PATH, which under a supervised daemon is a service PATH rather
	// than the operator's. ResolveToolBinary falls back to the per-user binary
	// homes so the same agent launches from a login shell and from `apps serve`.
	// "" means unresolvable — left as the bare name so the caller's own preflight
	// reports it, rather than substituting an empty argv[0].
	bin := agent
	if p := ResolveToolBinary(agent); p != "" {
		bin = p
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	// Force the spawned agent to run its shell commands through bashy (this
	// running binary) rather than the system shell — so the pure-Go userland,
	// the space-time advisor, and OTel apply to every command the agent runs.
	// Covers claude (CLAUDE_CODE_SHELL), aider/opencode ($SHELL), and agy and any
	// bare-name `bash`/`sh`/`zsh` shell-out (PATH shim). codex reads /etc/passwd
	// and is unreachable this way — see `bashy install-agent codex`. On by
	// default; BASHY_FORCE_AGENT_SHELL=0 disables. cmd.Path is already resolved
	// against the real PATH, so prepending the shim dir never shadows the agent.
	cmd.Env = agentChildEnv(ctx)
	return cmd
}

// fleetTokenEnv adds BASHY_FLEET_TOKEN when the launcher can resolve the
// host's pairing token and the env carries none of the explicit spellings.
func fleetTokenEnv(env []string) []string {
	for _, kv := range env {
		for _, name := range []string{"BASHY_FLEET_TOKEN=", "BASHY_API_KEY=", "CLOUDBOX_TOKEN="} {
			if strings.HasPrefix(kv, name) && len(kv) > len(name) {
				return env
			}
		}
	}
	if _, token := fleet.ResolveCloud("", ""); token != "" {
		env = append(env, "BASHY_FLEET_TOKEN="+token)
	}
	return env
}

// agentChildEnv builds the environment for a spawned agent process.
//
// It starts from the launcher's own environment, then, in order:
//   - CREDENTIAL FIREWALL: strips the operator's vault secrets, so a spawned
//     third-party agent does not inherit them by default. An agent CLI processes
//     untrusted content and has its own network egress; an inherited vault is the
//     lethal trifecta. Restore with secrets.AllowAgentSecretsEnv (explicit,
//     auditable). No-op when the host projects no vault secrets.
//   - shell forcing: route the agent's shell-outs back through bashy.
//   - principal stamping: tell the child which agent it is.
func agentChildEnv(ctx context.Context) []string {
	parent := os.Environ()
	env := secrets.ScrubAgentEnv(parent)

	// Deny by default, then restore only names carried by the resolved launch.
	//
	// The scrub above strips every vault-projected credential, so an agent cannot
	// inherit the operator's keyring. But a metered model needs ONE key in order
	// to answer, and the registry says which via `api_key_ref`. agentlaunch
	// resolves that reference to names only; values remain opaque and are copied
	// from this launcher's own environment.
	//
	// Preserving the model's own declared key — and only declared names — makes
	// the firewall a security boundary rather than an outage.
	if l, ok := LaunchFrom(ctx); ok {
		if len(l.PreserveEnv) > 0 {
			env = secrets.PreserveEnvNames(env, parent, l.PreserveEnv)
		} else if l.ModelName != "" {
			// Backward compatibility: callers may still construct Launch values
			// manually. Resolve the same catalog declaration the old path used,
			// and grant only its first matching credential rather than widening
			// an empty contract to arbitrary operator secrets.
			if m, found := newCatalog().Model(l.ModelName); found && m.APIKeyRef != "" {
				if kv, granted := secrets.GrantAgentKey(parent, m.APIKeyRef); granted {
					env = append(env, kv)
				}
			}
		}
	}
	// The host's own cloudbox pairing credential, so the bashy verbs the agent
	// runs (mb/inbox/sprint session) reach the shared session from INSIDE the
	// agent's sandbox. The launcher resolves it by exec'ing `outpost token
	// print`; a sandboxed child (codex workspace-write) cannot, and every
	// cross-host send it made fell back to a local post — "this host is not
	// paired" from a paired host (sprint 220, story ad5b0de9). This is bashy's
	// identity token, not an operator vault secret: the scrub above is about
	// keys an agent must not inherit; this one the agent's own bashy needs.
	// Only when no explicit token is already carried.
	env = fleetTokenEnv(env)
	// Give the child the same search path its launcher just used to find it.
	// Appended, so nothing already on PATH is shadowed; applied BEFORE the shim
	// dir is prepended, so shell forcing keeps priority.
	env = toolPathEnv(env)
	if forceAgentShell() {
		if bashy, err := os.Executable(); err == nil && bashy != "" {
			env = forcedShellEnv(env, bashy, ensureShims(bashy))
		}
	}
	if l, ok := LaunchFrom(ctx); ok {
		env = principalEnv(env, l)
		// PER-AGENT conversation store, keyed by the agent's identity.
		//
		// ycode locks its data dir, so two ycode processes sharing one store die
		// on launch — which is why this exists at all. It used to key on
		// `<nick>-<pid>`, and that made the cure worse than the disease in slow
		// motion: every launch of one agent got a BRAND NEW store, so an agent
		// remembered nothing across runs and orphaned a directory each time.
		//
		// Keyed on the identity, the store is what an agent's memory should be:
		// one per agent, durable across runs, and — because room.Join permits
		// only one live session per identity — never contended. An agent that
		// should start from nothing is a different agent: `bashy agent clone`.
		// Non-ycode tools and any operator-set YCODE_DATA_DIR/YCODE_HOME are left
		// untouched. Shares the SAME helper weave uses so the two launch surfaces
		// cannot drift.
		env = agentlaunch.ApplyYcodeDataDir(env, parent, toAgentLaunch(l), chatStateDir(), agentID(l))
	}
	return env
}

// chatStateDir is the bashy state root used for per-run agent stores launched by
// `bashy chat`. It is a sibling of the other ~/.bashy subdirs (sessions/logs,
// ctl, events) so a chat-launched store lives next to them rather than under the
// caller's working tree.
func chatStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".bashy", "chat")
}

// reduceInvokeOutput is the agent-context boundary for every one-shot turn.
//
// Invoke is shared by chat, delegate, meet, foreman, and supervise.  Its result
// is consequently agent-read context, not a human-facing command format: put
// the reduction here once rather than letting each workflow invent a different
// truncation rule.  Reduce spills before it returns an elided view, so a model
// can recover the complete turn from the marker instead of silently reasoning
// from a partial answer.
//
// The chat state root already owns per-agent conversation data.  Keeping the
// content-addressed output blobs beside it avoids a new, private artifact store
// for one-shot turns.
func reduceInvokeOutput(out string) (string, error) {
	return reduceChatBytes([]byte(out))
}

func reduceChatBytes(out []byte) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("chat: no state directory for reduced output")
	}
	root := filepath.Join(home, ".bashy", "chat")
	redactor := secrets.NewRedactor()
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			values[name] = value
		}
	}
	for name := range secrets.VaultEnvNames() {
		if value, ok := values[name]; ok {
			_ = redactor.Register(name, value)
		}
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("BASHY_OUTPUT_REDUCE")), "off") {
		out = reduce.CanonicalizeHome(out, home)
		return string(redactor.Redact(out)), nil
	}
	result, err := reduce.Reduce(reduce.NewStore(filepath.Join(root, "output")), out, reduce.Config{HomeDir: home, Redactor: redactor})
	if err != nil {
		return "", fmt.Errorf("chat: reduce invocation output: %w", err)
	}
	return result.Text, nil
}

func emitReduced(dst io.Writer, text string) error {
	if dst == nil || text == "" {
		return nil
	}
	_, err := io.WriteString(dst, text)
	return err
}

// appendStderr joins captured stderr onto stdout for error reporting.
func appendStderr(stdout, stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return stdout
	}
	stdout = strings.TrimRight(stdout, "\n")
	if stdout != "" {
		stdout += "\n"
	}
	return stdout + stderr
}

// seededProfiles is the LAST-RESORT launch contract, for a tool the fleet
// registry has never heard of. The registry (coreutils/pkg/fleet) is the
// source of truth; these rows exist so an operator who names an unregistered
// binary still gets the behavior they had before the registry existed.
//
// Do not add rows here. Add a tool to the registry instead — a row here can
// only describe how to start a binary, never which model to give it.
// The kill-switch flags (--dangerously-skip-permissions, --yes-always) live in
// UnsafeArgs, not Args, so the DEFAULT launch runs each agent under its own
// safety system. They are restored only under an explicit opt-in / verified
// container (see unsafeLaunchAllowed). codex needs none here: its safe default
// (--sandbox workspace-write) is already in Args, and the unsafe form is reached
// only by an explicit `--sandbox danger-full-access`, which applySandbox maps and
// guardUnsafeArgs then gates.
var seededProfiles = func() map[string]LaunchProfile {
	out := make(map[string]LaunchProfile, len(agentlaunch.SeededProfiles))
	for name, p := range agentlaunch.SeededProfiles {
		out[name] = LaunchProfile{
			Args:       append([]string(nil), p.Args...),
			UnsafeArgs: append([]string(nil), p.UnsafeArgs...),
		}
	}
	return out
}()

// newCatalog builds the fleet catalog the launcher resolves against. It is a
// var so tests can pin it to a scratch store instead of the developer's own.
var newCatalog = func() *fleet.Catalog { return fleet.New() }

// resolveLaunch turns a name into a runnable invocation.
//
// The name may be an agent nickname (007), a bare tool:model binding
// (claude:opus), or a plain tool (claude). Only the first two can select a
// model — which is the whole point: before the registry, `claude:opus` was a
// label the launcher logged and threw away, and every run silently used
// whatever model the tool's own config happened to name.
func resolveLaunch(name string, opt Options) (Launch, error) {
	prevContainerized := agentlaunch.Containerized
	agentlaunch.Containerized = containerized
	defer func() { agentlaunch.Containerized = prevContainerized }()
	l, err := agentlaunch.ResolveWithCatalog(name, toAgentLaunchOptions(opt), newCatalog)
	return fromAgentLaunch(l), err
}

// forceAgentShell reports whether the launcher routes a spawned agent's shell
// through bashy. On by default; BASHY_FORCE_AGENT_SHELL=0 disables (and --posix
// hosts / the lean `bash` binary never reach this path).
func forceAgentShell() bool { return os.Getenv("BASHY_FORCE_AGENT_SHELL") != "0" }

// shimDir is the directory of sh/bash/zsh symlinks to the bashy binary, prepended
// to a spawned agent's PATH so bare-name shell lookups resolve to bashy. Override
// with BASHY_SHIM_DIR (used by tests).
func shimDir() string {
	if d := os.Getenv("BASHY_SHIM_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bashy", "shims")
}

// ensureShims makes shimDir hold sh/bash/zsh symlinks to bashy (idempotent,
// best-effort). No-op on Windows (POSIX shell names don't apply) or when the dir
// is unavailable; returns "" in those cases so forcedShellEnv skips the PATH shim.
func ensureShims(bashy string) string {
	if runtime.GOOS == "windows" || bashy == "" {
		return ""
	}
	dir := shimDir()
	if dir == "" || os.MkdirAll(dir, 0o755) != nil {
		return ""
	}
	for _, name := range []string{"sh", "bash", "zsh"} {
		link := filepath.Join(dir, name)
		if target, err := os.Readlink(link); err == nil && target == bashy {
			continue
		}
		_ = os.Remove(link)
		_ = os.Symlink(bashy, link)
	}
	return dir
}

// forcedShellEnv returns base with the bashy-shell routing vars applied: shimDir
// prepended to PATH (when non-empty), and SHELL + CLAUDE_CODE_SHELL pinned to
// bashy. Pure and deterministic so it can be tested without spawning a process.
func forcedShellEnv(base []string, bashy, shimDir string) []string {
	out := make([]string, 0, len(base)+2)
	pathSet := false
	for _, kv := range base {
		switch {
		case shimDir != "" && strings.HasPrefix(kv, "PATH="):
			out = append(out, "PATH="+shimDir+string(os.PathListSeparator)+kv[len("PATH="):])
			pathSet = true
		case strings.HasPrefix(kv, "SHELL="), strings.HasPrefix(kv, "CLAUDE_CODE_SHELL="):
			// re-added canonically below
		default:
			out = append(out, kv)
		}
	}
	if shimDir != "" && !pathSet {
		out = append(out, "PATH="+shimDir)
	}
	if runtime.GOOS != "windows" {
		out = append(out, "SHELL="+bashy)
	}
	out = append(out, "CLAUDE_CODE_SHELL="+bashy)
	return out
}

// principalEnv tells the spawned process who it is.
//
// The child can only sign its work with what the launcher gave it, which is
// what makes "agent 007 commented" trustworthy inside one host: forging the
// name means already controlling the launcher. Nothing is stamped for a bare
// tool — a tool is not an agent, and inventing a nickname for it would put a
// name in the record that resolves to nothing.
func principalEnv(base []string, l Launch) []string {
	return agentlaunch.PrincipalEnv(base, toAgentLaunch(l))
}

var roleDefaults = map[string]string{
	"conductor": "claude",
	"reviewer":  "codex",
	"qa":        "codex",
	"release":   "claude",
}

// NewChatCmd returns the `bashy chat` command.
func NewChatCmd() *cobra.Command {
	var opt Options
	var capStr string
	var toolSel string
	var bandSel int
	var interactive bool
	var attachLive bool
	cmd := &cobra.Command{
		Use:   "chat [--agent AGENT | --band N | --tool T] [--instruction TEXT]",
		Short: "talk to an agent — a live governed session (no instruction) or a one-shot (with --instruction)",
		// A failed launch is a runtime error, not a usage error. Dumping the
		// flag list on "this tool cannot select a model" buries the sentence
		// that explains what to do about it.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opt.Instruction = strings.TrimSpace(strings.Join(append([]string{opt.Instruction}, args...), " "))
			}
			// --capability routes to the best-fit ROUTABLE agent from the living
			// matrix (see pkg/capability). The matrix is keyed by tool:model, and
			// that whole binding is what we launch: the router picked a model for
			// a reason, and dispatching by tool alone would silently run whatever
			// the tool's own config happened to name.
			if strings.TrimSpace(capStr) != "" && opt.Agent == "" {
				c, ok := capability.ParseCapability(capStr)
				if !ok {
					return fmt.Errorf("chat: unknown capability %q", capStr)
				}
				m, err := capability.Load()
				if err != nil {
					return err
				}
				ranked := m.Best(c, true, capability.ByValue)
				if len(ranked) == 0 {
					return fmt.Errorf("chat: no routable agent for capability %q", capStr)
				}
				best := ranked[0]
				opt.Agent = best.Agent
				fmt.Fprintf(cmd.ErrOrStderr(), "chat: capability %s → %s (q=%.2f)\n",
					c, best.Agent, best.Cell.Quality)
			}

			// --band/--tool pick ONE operable agent for you (a specific --agent names
			// it). This resolves both the interactive and the one-shot path, so `chat
			// --band 3` and `chat --band 3 -m "..."` select the same agent.
			if bandSel != 0 || strings.TrimSpace(toolSel) != "" {
				picked, err := PickAgent(Selector{Agent: opt.Agent, Tool: toolSel, Band: bandSel})
				if err != nil {
					return err
				}
				if picked != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "chat: selected %s\n", picked)
					opt.Agent = picked
				}
			}

			// A question (--instruction/--file/--context) is a one-shot Invoke. No
			// question, at a terminal, is a CONVERSATION — the native interactive
			// session. --interactive forces the conversation even with a prompt.
			// --dry-run never launches, so it always takes the Invoke print path.
			asked := strings.TrimSpace(opt.Instruction) != "" || len(opt.Files) > 0 || len(opt.Context) > 0
			wantInteractive := !opt.DryRun && (interactive || (!asked && stdinIsTTY(cmd)))
			if wantInteractive {
				// Without a TTY, an explicit -i is a HEADLESS STEERABLE session: the
				// agent still gets its own PTY (agentpty's capture branch, which also
				// answers terminal startup queries), the prompt arrives over the
				// control socket, and `chat steer <id>` reaches it mid-turn. This is
				// what a scripted caller — the agent bench — needs to steer an agent
				// through the same channel a human would. Implicit interactive (no
				// -i) still requires a terminal.
				if !stdinIsTTY(cmd) && !interactive {
					return fmt.Errorf("chat: an interactive session needs a controlling terminal; " +
						"use -m/--instruction for a one-shot, or -i for a headless steerable session")
				}
				exit, err := Interact(cmd.Context(), opt.Agent, InteractOptions{
					Prompt:       opt.Instruction,
					Cwd:          opt.Cwd,
					Timeout:      opt.Timeout,
					ReadOnly:     opt.ReadOnly,
					Unattended:   opt.AllowUnsafe,
					AllowPremium: opt.AllowPremium,
					Status:       cmd.ErrOrStderr(),
					Attach:       attachLive,
				})
				if err != nil {
					return err
				}
				_ = exit // a human saw the tool exit; the launcher's own exit stays 0
				return nil
			}

			plain, _ := cmd.Flags().GetBool("plain")
			opt.JSON = opt.JSON || (os.Getenv("BASHY_AGENTIC") != "" && !plain)
			// A human-facing one-shot is live by default. Besides avoiding a long
			// blank terminal, this activates each tool's registry-declared event
			// channel (stdout for AGY/Claude/Codex, side-file for ycode). JSON mode
			// remains a single result envelope and dry-run remains deterministic.
			if !opt.JSON && !opt.DryRun {
				opt.Stream = cmd.OutOrStdout()
			}
			res, err := Invoke(cmd.Context(), opt, nil)
			if opt.JSON {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else if opt.Stream == nil && res.Output != "" {
				fmt.Fprint(cmd.OutOrStdout(), res.Output)
				if !strings.HasSuffix(res.Output, "\n") {
					fmt.Fprintln(cmd.OutOrStdout())
				}
			}
			return err
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.Flags().StringVar(&opt.Agent, "agent", "", "agent command to run, such as claude, codex, agy, or opencode")
	cmd.Flags().StringVar(&toolSel, "tool", "", "launch ANY operable agent using this tool (e.g. codex)")
	cmd.Flags().IntVar(&bandSel, "band", 0, "launch ANY operable agent pegged at this capability band or above (1-4)")
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "force a live interactive session even with an instruction")
	cmd.Flags().BoolVar(&attachLive, "attach", false, "if the agent is already live, watch and steer that session instead of being refused")
	cmd.Flags().BoolVar(&opt.AllowUnsafe, "yolo", false,
		"disable the agent's approval gate (keep --dangerously-skip-permissions) and accept the "+
			"uncontained-host risk — for a session you SUPERVISE REMOTELY via `chat steer`, where no one "+
			"sits at the agent's terminal to answer an approval prompt. The steer loop is the oversight")
	cmd.Flags().BoolVar(&opt.AllowPremium, "allow-premium", false, "explicitly bypass LLM budget/subscription gates for urgent human-authorized work")
	cmd.Flags().StringVar(&opt.Role, "role", "", "role alias when --agent is omitted: conductor, reviewer, qa, release")
	cmd.Flags().StringVar(&opt.Task, "task", "", "safe one-line assignment label shown in the live agent roster")
	cmd.Flags().StringVar(&capStr, "capability", "", "route to the best-fit routable agent for this capability (e.g. deep-research, coding)")
	cmd.Flags().StringVarP(&opt.Instruction, "instruction", "m", "", "instruction to send to the agent (one-shot; omit for an interactive session)")
	cmd.Flags().StringArrayVar(&opt.Files, "file", nil, "append file contents to the instruction")
	cmd.Flags().StringArrayVar(&opt.Context, "context", nil, "append context text to the instruction")
	cmd.Flags().StringVar(&opt.Cwd, "cwd", "", "working directory for the agent process")
	cmd.Flags().DurationVar(&opt.Timeout, "timeout", 0, "agent timeout, for example 30m")
	cmd.Flags().StringVar(&opt.Sandbox, "sandbox", "", "agent sandbox override, for example workspace-write or danger-full-access")
	cmd.Flags().BoolVar(&opt.JSON, "json", false, "print a bashy-chat-v1 JSON result envelope")
	cmd.Flags().Bool("plain", false, "force plain output even under BASHY_AGENTIC")
	cmd.Flags().BoolVar(&opt.DryRun, "dry-run", false, "print the resolved invocation without running the agent")
	cmd.Flags().BoolVar(&opt.ReadOnly, "read-only", false,
		"launch with NO write authority: strip the approval-gate kill-switches and pin any sandbox to read-only. "+
			"An agent that only has to ANSWER needs no filesystem access — and because this removes the dangerous "+
			"flags rather than asking permission to keep them, it passes the launch guard on an ordinary uncontained "+
			"host, so nobody has to weaken a machine just to ask an agent a question")
	_ = cmd.Flags().MarkHidden("plain")

	// The host-room control surface: observe (sessions/timeline/attach) and
	// participate (steer/interrupt). Every launch path joins the same room.
	cmd.AddCommand(newChatSessionsCmd(), newChatTimelineCmd(), newChatSteerCmd(),
		newChatGrantCmd(), newChatInterruptCmd(), newChatAttachCmd())
	return cmd
}

// stdinIsTTY reports whether the command's stdin is an interactive terminal — the
// signal that "no instruction" means "open a conversation" rather than "read a
// piped prompt". Falls back to the process stdin when the cobra stream is not a
// *os.File (tests inject buffers, which are correctly treated as non-interactive).
func stdinIsTTY(cmd *cobra.Command) bool {
	if f, ok := cmd.InOrStdin().(*os.File); ok {
		return term.IsTerminal(int(f.Fd()))
	}
	return false
}

// Invoke resolves the agent, builds the prompt, and runs it.
func Invoke(ctx context.Context, opt Options, runner Runner) (Result, error) {
	if runner == nil {
		runner = execRunner{pty: opt.PTY, ctlSock: opt.CtlSock, killOnParentExit: opt.KillOnParentExit}
	}
	name, err := ResolveAgent(opt.Agent, opt.Role)
	if err != nil {
		return Result{SchemaVersion: schemaVersion, Agent: opt.Agent, Role: opt.Role, ExitCode: 2}, err
	}
	lnch, err := resolveLaunch(name, opt)
	if err != nil {
		return Result{SchemaVersion: schemaVersion, Agent: lnch.Tool, Nick: name, Role: opt.Role, ExitCode: 2}, err
	}
	prompt, err := BuildPrompt(opt)
	if err != nil {
		return Result{SchemaVersion: schemaVersion, Agent: lnch.Tool, Nick: name, Role: opt.Role, ExitCode: 2}, err
	}
	if !opt.DryRun {
		var d llmbudget.Decision
		lnch, d, err = governLaunch(ctx, name, lnch, prompt, opt)
		if err != nil {
			return Result{SchemaVersion: schemaVersion, Agent: lnch.Tool, Nick: name, Role: opt.Role, ExitCode: 2}, err
		}
		if d.Action == llmbudget.Queue {
			return Result{SchemaVersion: schemaVersion, Agent: lnch.Tool, Nick: name, Role: opt.Role, ExitCode: 75}, fmt.Errorf("chat: LLM budget queued %s for %s: %s", d.Delay, lnch.Binding(), d.Reason)
		}
		if d.Action == llmbudget.Block {
			return Result{SchemaVersion: schemaVersion, Agent: lnch.Tool, Nick: name, Role: opt.Role, ExitCode: 75}, fmt.Errorf("chat: LLM budget blocked %s: %s", lnch.Binding(), d.Reason)
		}
	}
	// KNOWLEDGE INJECTION (stage 1 of the agent turn — see
	// dhnt/docs/agent-turn-lifecycle-and-verb-map.md). Off unless
	// BASHY_KNOWLEDGE=on, because this is the treatment arm of an experiment
	// whose result is not in yet: making it the default would both change every
	// agent's behaviour on an unproven feature AND destroy the control arm.
	//
	// It goes HERE rather than in agentCommand because a preamble must precede
	// the prompt, and this is the site where the prompt is still a separate
	// string. ArgvPrefixWithWorkspace guarantees {prompt} is the trailing token
	// for every tool that reaches this path, so prefixing the prompt text is
	// safe across all of them — including `aider --message {prompt}`, where
	// appending anything AFTER the prompt would be consumed by the flag.
	//
	// Best-effort by construction: a memory lookup must never stop an agent from
	// starting, so an unreadable ring yields "" and the launch is unchanged.
	if recall.Enabled() {
		res := recall.Context(recall.Query{
			Text: prompt, Rings: []string{recall.RingRepo, recall.RingHost},
			Forms: []string{kb.FormNote, kb.FormPage}, Budget: recall.PreambleBudget,
			OS: runtime.GOOS,
		}, recall.ContextReaders()...)
		if pre := recall.RenderContext(res); pre != "" {
			prompt = pre + "\n" + prompt
		}
	}
	args := append(lnch.Args, prompt)
	// A caller asking to observe a turn live needs the tool's declared event
	// stream, not a pipe around a CLI that buffers prose until exit. The fleet
	// registry is the adapter: AGY receives stream-json flags, while tools with
	// no stdout event contract keep their existing argv.
	var eventPath string
	if opt.Stream != nil || opt.EventStream != nil {
		args = agentlaunch.InsertBeforePrompt(args,
			agentlaunch.EventStdoutArgs(toAgentLaunch(lnch)))
		// ycode exposes the same structured stream through a file side-channel
		// rather than stdout. Ask for that channel here too; the runner bridge
		// below follows it into Stream while the process is alive.
		if extra := agentlaunch.EventFileArgsWithCatalog(toAgentLaunch(lnch), "<events>", newCatalog); len(extra) > 0 {
			if !opt.DryRun {
				dir, mkErr := os.MkdirTemp("", "bashy-chat-events-")
				if mkErr != nil {
					return Result{SchemaVersion: schemaVersion, Agent: lnch.Tool, Nick: name, Role: opt.Role, ExitCode: 2}, fmt.Errorf("chat: create event channel: %w", mkErr)
				}
				defer os.RemoveAll(dir)
				eventPath = filepath.Join(dir, "events.ndjson")
				extra = agentlaunch.EventFileArgsWithCatalog(toAgentLaunch(lnch), eventPath, newCatalog)
			}
			args = agentlaunch.InsertBeforePrompt(args, extra)
		}
	}
	cwd := opt.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	res := Result{
		SchemaVersion: schemaVersion,
		Agent:         lnch.Tool,
		Role:          opt.Role,
		Cwd:           cwd,
		Args:          args,
		DryRun:        opt.DryRun,
	}
	if lnch.Nick != lnch.ToolName {
		res.Nick = lnch.Nick
	}
	res.Model = lnch.Model
	if opt.DryRun {
		res.ExitCode = 0
		res.Output = strings.Join(append([]string{lnch.Tool}, args...), " ")
		return res, nil
	}
	// A one-shot is a TURN, not a session, so it does not take a seat in the
	// room. It does, however, open the agent's conversation store — and for a
	// store-locking tool that means firing a turn at an agent who is mid-session
	// dies inside the tool with "storage is locked by another ycode process",
	// naming neither the agent nor the session holding it.
	//
	// So: say it ourselves, in our own vocabulary, before launching. This is
	// ADVISORY and read-only — it takes no card and writes no timeline event,
	// because a turn every few seconds must not bloat the board it reports to.
	// The honest limit: it catches a turn racing a SESSION, which is the case
	// operators actually hit. Two simultaneous one-shots of one agent still race,
	// and the loser still gets the tool's own error.
	if err := refuseIfSessionLive(lnch); err != nil {
		res.ExitCode = 2
		return res, err
	}

	// PUBLISH A LIVE TASK CARD for the duration of the turn, so an operator running
	// `bashy chat sessions` sees the work in flight — the same board as interactive
	// sessions and weave/foreman workers. A one-shot is a UNIT OF WORK, not an
	// identity, so its card is WORK-KEYED (oneShotCardID: name + pid + a per-process
	// sequence), NOT the agent's singleton session id. That distinction is load-
	// bearing twice over: it keeps a fired turn from taking (and on exit evicting)
	// the identity seat a live interactive session of the same agent holds, and it
	// lets concurrent one-shots of one binding coexist instead of clobbering each
	// other. Registration is fail-closed: invisible work defeats workload routing,
	// so a failed room write prevents the child from starting. A successful card is
	// removed on the deferred Leave whether the turn returns, errors, or is cancelled.
	taskCard := room.Card{
		ID:        oneShotCardID(lnch),
		Principal: principalName(),
		Tool:      lnch.ToolName,
		Model:     lnch.ModelName,
		Binding:   lnch.Binding(),
		Nick:      lnch.Nick,
		Band:      bindingBand(name),
		Mode:      "oneshot",
		Role:      opt.Role,
		Task:      taskLabel(opt),
		PID:       os.Getpid(),
		Cwd:       cwd,
	}
	if err := room.Join(taskCard); err != nil {
		// Join writes the membership before appending its timeline event. If the
		// append fails, remove this invocation's unique card before refusing the
		// launch so publication failure cannot itself create phantom live work.
		room.Leave(taskCard.ID)
		res.ExitCode = 2
		return res, fmt.Errorf("chat: publish live assignment: %w", err)
	}
	defer room.Leave(taskCard.ID)

	if opt.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opt.Timeout)
		defer cancel()
	}
	// The launcher is the only place that knows which principal is about to
	// act, so it is the only place that can tell the spawned process who it
	// is. execRunner reads this back out to stamp the child's environment.
	budgetWork, budgetErr := reserveBudgetWork(ctx, lnch, prompt, taskCard.ID, true, opt.AllowPremium)
	if budgetErr != nil {
		res.ExitCode = 75
		return res, budgetErr
	}
	callCtx, endObservation := startGenAIObservation(ctx, lnch)
	proof := &budgetProcessProof{}
	callCtx = context.WithValue(callCtx, budgetProcessKey{}, proof)
	out, code, err := runner.Run(withLaunch(callCtx, lnch), lnch.Tool, args, cwd)
	endGenAIObservation(endObservation, lnch, prompt, out, "", err)
	var finishErr error
	if proof.Observed && !proof.Started {
		finishErr = budgetWork.abort()
	} else {
		budgetErr := err
		if proof.Observed {
			if proof.Terminated {
				budgetErr = nil
			} else {
				budgetErr = errors.New("inherited child lifetime unverified")
			}
		}
		finishErr = budgetWork.finish(out, budgetErr)
	}
	if finishErr != nil {
		err = errors.Join(err, finishErr)
	}
	// This is the shared return seam for every unattended agent turn.  Do not
	// return an over-budget transcript: its complete bytes have first been
	// spilled by reduceInvokeOutput, and the bounded view tells the next agent
	// exactly how to recover them.
	res.ExitCode = code
	reduced, reduceErr := reduceInvokeOutput(out)
	if reduceErr != nil {
		return res, reduceErr
	}
	res.Output = reduced
	// A stream is model-visible context. Buffer until this spill-backed view is
	// available; raw process output must never race out through a tee.
	if writeErr := emitReduced(opt.Stream, reduced); writeErr != nil {
		return res, fmt.Errorf("chat: emit reduced stream: %w", writeErr)
	}
	if eventPath != "" {
		events, readErr := os.ReadFile(eventPath)
		if readErr != nil && !os.IsNotExist(readErr) {
			return res, fmt.Errorf("chat: read event channel: %w", readErr)
		}
		if len(events) > 0 {
			reducedEvents, eventErr := reduceChatBytes(events)
			if eventErr != nil {
				return res, eventErr
			}
			evSink := opt.Stream
			if opt.EventStream != nil {
				evSink = opt.EventStream
			}
			if writeErr := emitReduced(evSink, reducedEvents); writeErr != nil {
				return res, fmt.Errorf("chat: emit reduced event stream: %w", writeErr)
			}
		}
	}
	return res, err
}

// oneShotSeq disambiguates concurrent one-shot task cards launched from THIS
// process. Two Invoke calls for the same binding share a pid, so pid alone would
// collide; the per-process sequence makes each turn's card id unique without
// leaking into the agent's singleton identity id.
var oneShotSeq atomic.Uint64

// oneShotCardID is the WORK key for a one-shot turn's room card: agent name, pid,
// and a per-process sequence. Deliberately NOT agentID(l) — that is the singleton
// SESSION key, and a turn must never sit in (or evict) it. The shape mirrors the
// task-card convention (`weave-<issue>-<pid>`) so `chat sessions` reads it as work,
// not identity.
func oneShotCardID(l Launch) string {
	return fmt.Sprintf("oneshot-%s-%d-%d", shortHash(agentID(l)), os.Getpid(), oneShotSeq.Add(1))
}

// taskLabel is a SAFE, CONCISE one-line summary of the instruction for the shared
// board. Safe: only the first line survives, control characters are dropped, and
// the whole prompt is never splashed onto a host-wide registry. Concise: collapsed
// whitespace, truncated on a rune boundary. Empty instruction yields no label.
func taskLabel(opt Options) string {
	s := strings.TrimSpace(opt.Task)
	if s == "" {
		s = opt.Instruction
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
		if r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	const max = 80
	if rs := []rune(s); len(rs) > max {
		s = strings.TrimSpace(string(rs[:max])) + "…"
	}
	if s == "" {
		return "one-shot agent task"
	}
	return s
}

func governLaunch(ctx context.Context, originalName string, l Launch, prompt string, opt Options) (Launch, llmbudget.Decision, error) {
	if l.ModelName == "" {
		return l, llmbudget.Decision{Action: llmbudget.Allow, Model: l.ModelName}, nil
	}
	seen := map[string]bool{}
	for hops := 0; hops < 4; hops++ {
		if seen[l.ModelName] {
			return l, llmbudget.Decision{Action: llmbudget.Block, Model: l.ModelName, Reason: "budget route cycle"}, nil
		}
		seen[l.ModelName] = true
		request := opaqueBudgetRequest(l, prompt, true)
		request.AllowPremium = opt.AllowPremium
		preview, e := llmbudget.Preview(ctx, request)
		if e != nil {
			return l, preview.Decision, e
		}
		d := preview.Decision
		switch d.Action {
		case "", llmbudget.Allow:
			return l, d, nil
		case llmbudget.Downgrade, llmbudget.RouteAlt:
			target := d.Model
			if target == "" {
				return l, d, fmt.Errorf("chat: LLM budget returned %s without a target for %s", d.Action, l.Binding())
			}
			if !strings.Contains(target, ":") {
				target = l.ToolName + ":" + target
			}
			next, err := resolveLaunch(target, opt)
			if err != nil {
				return l, d, fmt.Errorf("chat: LLM budget %s from %s to %s, but target cannot launch: %w", d.Action, originalName, target, err)
			}
			if d.Action == llmbudget.RouteAlt && d.Lane == llmbudget.LaneSubscription && !opt.AllowPremium {
				if lane, known := llmbudget.LaneForModel(next.ModelName); known && lane == llmbudget.LaneAPIKey {
					return l, llmbudget.Decision{Action: llmbudget.Block, Model: l.ModelName, Lane: d.Lane, Reason: "subscription exhaustion may use API-key fallback only with --allow-premium"}, nil
				}
			}
			l = next // The alternate is a new LLM call and must pass the same gate.
		default:
			return l, d, nil
		}
	}
	return l, llmbudget.Decision{Action: llmbudget.Block, Model: l.ModelName, Reason: "budget route hop limit"}, nil
}

func recordLaunchUsage(ctx context.Context, l Launch, prompt, output string) {
	recordLaunchUsageTokens(ctx, l, estimateTokens(prompt), estimateTokens(output))
}

func recordLaunchUsageTokens(ctx context.Context, l Launch, promptTokens, completionTokens int64) {
	if l.ModelName == "" {
		return
	}
	cost, ok := llmbudget.EstimatedCostUSD(l.ModelName, promptTokens+completionTokens)
	if !ok {
		cost = 0
	}
	llmbudget.RecordContext(ctx, l.ModelName, promptTokens, completionTokens, cost)
}

func estimateTokens(s string) int64 {
	n := int64(len([]rune(s)) / 4)
	if strings.TrimSpace(s) != "" && n == 0 {
		n = 1
	}
	return n
}

func withLaunch(ctx context.Context, l Launch) context.Context {
	return agentlaunch.WithLaunch(ctx, toAgentLaunch(l))
}

// LaunchFrom returns the invocation being run, when the caller came through
// Invoke. A Runner built by hand sees no value and simply skips attribution.
func LaunchFrom(ctx context.Context) (Launch, bool) {
	l, ok := agentlaunch.LaunchFrom(ctx)
	return fromAgentLaunch(l), ok
}

// unsafeLaunchFlags are the flags by which an agent CLI's OWN approval gate is
// switched off. Passing one hands the agent unattended, unreviewed access to
// whatever the launching process can reach — so each is legitimate only when
// something ELSE is already containing the agent (a container), and is a
// self-inflicted wound otherwise. Their vendors named them "dangerously"; we
// take that literally.
//
// codex's `--sandbox danger-full-access` is here for the same reason: it turns
// codex's built-in sandbox off. `--sandbox workspace-write` (the default) is
// codex sandboxing itself and is NOT unsafe.
var unsafeLaunchFlags = agentlaunch.UnsafeLaunchFlags

// UnsafeLaunchEnv opts a host into launching agents with their own safety
// systems disabled. It is the operator's explicit, auditable acceptance of the
// risk — never a default.
const UnsafeLaunchEnv = "BASHY_ALLOW_UNSAFE_AGENT_LAUNCH"

// containerized reports whether this process is already inside an OCI
// container, i.e. whether something else is containing the agent. A var so
// tests can simulate a contained host.
//
// This deliberately re-probes on every call instead of reading the shared
// spacetime probe cache: that cache is a user-writable file, and a security
// gate must not be unlockable by writing `"container":"true"` into it. The
// signals mirror spacetime's own probeContainer.
var containerized = func() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// unsafeLaunchAllowed reports whether stripping an agent's safety systems is
// permissible here, and why.
func unsafeLaunchAllowed() (bool, string) {
	prevContainerized := agentlaunch.Containerized
	agentlaunch.Containerized = containerized
	defer func() { agentlaunch.Containerized = prevContainerized }()
	return agentlaunch.UnsafeLaunchAllowed()
}

// guardUnsafeArgs refuses a launch that would disable an agent CLI's own safety
// systems on a host where nothing else is containing it.
//
// This is the ONE choke point for it: every launch — registry-templated or
// seeded-fallback — renders its argv through resolveLaunch, so a dangerous flag
// cannot reach an agent by any other route, including a `bashy tool add`
// template written later.
//
// It refuses rather than silently stripping the flag: stripping would leave a
// headless agent blocking forever on an approval prompt nobody can answer, and
// a hang is a worse failure than a clear error. The operator gets a one-line fix.
func guardUnsafeArgs(tool string, args []string) error {
	prevContainerized := agentlaunch.Containerized
	agentlaunch.Containerized = containerized
	defer func() { agentlaunch.Containerized = prevContainerized }()
	return agentlaunch.GuardUnsafeArgs(tool, args)
}

// finalizeArgs applies the --sandbox override, then gates the result. Both
// launch paths (registry template and seeded fallback) go through here.
func finalizeArgs(tool string, args []string, opt Options) ([]string, error) {
	prevContainerized := agentlaunch.Containerized
	agentlaunch.Containerized = containerized
	defer func() { agentlaunch.Containerized = prevContainerized }()
	return agentlaunch.FinalizeArgs(tool, args, toAgentLaunchOptions(opt))
}

// readOnlyArgs strips every approval-gate kill-switch from an argv and pins a
// sandboxing tool to read-only.
//
// The result passes guardUnsafeArgs by construction — not because the guard was
// bypassed, but because there is nothing left to guard: the agent is being launched
// with less authority, not more.
func readOnlyArgs(tool string, args []string) []string {
	return agentlaunch.ReadOnlyArgs(tool, args)
}

// applySandbox layers the --sandbox override onto an already-rendered argv.
// The prompt is not yet present, so appending a flag pair is safe here.
func applySandbox(agent string, args []string, opt Options) []string {
	return agentlaunch.ApplySandbox(agent, args, toAgentLaunchOptions(opt))
}

// ResolveAgent maps either an explicit agent or a workflow role to a command.
func ResolveAgent(agent, role string) (string, error) {
	agent = strings.TrimSpace(agent)
	if agent != "" {
		return agent, nil
	}
	role = strings.TrimSpace(strings.ToLower(role))
	if role == "" {
		return "", errors.New("chat: --agent or --role is required")
	}
	if a := roleDefaults[role]; a != "" {
		return a, nil
	}
	return "", fmt.Errorf("chat: unknown role %q", role)
}

// BuildPrompt composes the user instruction, inline context, and file snippets.
func BuildPrompt(opt Options) (string, error) {
	var b bytes.Buffer
	if s := strings.TrimSpace(opt.Instruction); s != "" {
		b.WriteString(s)
		b.WriteString("\n")
	}
	for _, c := range opt.Context {
		if c = strings.TrimSpace(c); c != "" {
			fmt.Fprintf(&b, "\nContext:\n%s\n", c)
		}
	}
	for _, name := range opt.Files {
		if strings.TrimSpace(name) == "" {
			continue
		}
		clean := filepath.Clean(name)
		data, err := os.ReadFile(clean)
		if err != nil {
			return "", fmt.Errorf("chat: read %s: %w", name, err)
		}
		fmt.Fprintf(&b, "\nFile %s:\n", clean)
		_, _ = io.Copy(&b, bytes.NewReader(data))
		if !bytes.HasSuffix(data, []byte("\n")) {
			b.WriteByte('\n')
		}
	}
	prompt := strings.TrimSpace(b.String())
	if prompt == "" {
		return "", errors.New("chat: --instruction, positional text, --context, or --file is required")
	}
	return prompt, nil
}
