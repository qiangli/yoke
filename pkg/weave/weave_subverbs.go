package weave

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Per-subverb constructors. Each registers its flags + RunE and
// dispatches to a runWeave* body in weave_impl.go (or
// weave_autopilot.go). Every subverb emits its own structured
// envelope and propagates an *exitCodeError carrying a stable
// weavecli exit code.
//
// Every subverb runs entirely on the local filesystem. The lone
// exception is `prio --auto`, which delegates queue ranking to an LLM
// provider and degrades to a dependency_unhealthy envelope when none is
// configured; it carries weaveStatusAnnotation so `weave check` can
// report the dependency.

// weaveStatusAnnotation is the cobra annotation key a subverb sets to
// override the default "implemented" status reported by `weave check`.
// Subverbs that work fully on the local filesystem leave it unset; the
// LLM-gated path sets it to name its dependency.
const weaveStatusAnnotation = "weave_status"

func newWeaveAddCmd() *cobra.Command {
	var flags weaveOutputFlags
	var title, body, tool, priority string
	var fromFile string
	var fromIssue string
	var fromTodo string
	var verify string
	var suiteGate string
	var stage string
	var points int
	var judge string
	var band int
	cmd := &cobra.Command{
		Use:   `add "<title>"`,
		Short: "Seed an issue into the queue",
		Long: `Files a new issue into the local queue, tags it loom:todo, and
applies priority + source labels. The next 'weave start' picks it up
according to the priority sort order.

--stage says WHICH PART OF THE LIFECYCLE this work belongs to: plan, code,
test or deploy. Until it existed, every weave issue was implicitly a coding
issue, so "review the design", "triage the flaky fixture" and "promote to
staging" had nowhere to live and the board could only describe the middle of
the lifecycle. It defaults to "code", which is what every existing issue is.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				title = args[0]
			}
			_ = tool
			if fromTodo != "" {
				return runWeaveAddFromTodo(cmd, fromTodo, &flags)
			}
			if fromIssue != "" {
				return runWeaveAddFromIssue(cmd, fromIssue, &flags)
			}
			if fromFile != "" {
				return runWeaveAddFromFile(cmd, fromFile, priority, &flags)
			}
			return runWeaveAddStaged(cmd, title, body, priority, verify, suiteGate, stage, judge, points, band, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&body, "body", "", "Issue body (optional)")
	cmd.Flags().StringVar(&tool, "tool", "", "Pin a specific agentic tool for this issue (label tool:X)")
	cmd.Flags().StringVar(&priority, "priority", "", "Priority tier: p0|p1|p2|p3 (default p2)")
	cmd.Flags().StringVar(&stage, "stage", "", "SDLC stage: plan|code|test|deploy (default code)")
	cmd.Flags().IntVar(&points, "points", 0, "Story points (1,2,3,5,8; 8 = ~30m cap — `weave split` bigger work)")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "Bulk seed: markdown (`- [ ] title`) or JSON list of {title,body,priority}")
	cmd.Flags().StringVar(&fromIssue, "from-issue", "", "Seed from a TRIAGED `bashy issue` register entry (id or unique prefix); links both ways")
	cmd.Flags().StringVar(&fromTodo, "from-todo", "", "Seed from a repo/host `bashy todo` entry (id, unique prefix, or #seq); links both ways")
	cmd.Flags().StringVar(&verify, "verify", "", "Verify command the wrapper runs (`bash -c`) in the workspace at terminal time; verify_exit/verify_output recorded on the item, non-zero blocks `weave pull`")
	cmd.Flags().StringVar(&suiteGate, "suite-gate", "", "Integration suite command run (`bash -c`) at the base repo root after merge; non-zero resets the merge and records suite_gate_exit/suite_gate_output")
	cmd.Flags().StringVar(&judge, "judge", "", "Verifiability metadata: none (default) | required (compatibility label; use --review-agent on pull to invoke model review)")
	cmd.Flags().IntVar(&band, "band", 0, "Issue difficulty band (1-4; 0 = infer from the coding agent). Raises the judge floor to max(L3, band)")
	return cmd
}

func newWeaveStartCmd() *cobra.Command {
	var flags weaveOutputFlags
	var issue int64
	var tool string
	var resume bool
	var noSpawn bool
	var cloneAgent bool
	var autoCommit bool
	var ptyMode string
	var idleTimeout time.Duration
	var maxRuntime time.Duration
	var memLimit string
	cmd := &cobra.Command{
		Use:   "start [-- <agent>|<tool> [args...]]",
		Short: "Allocate a workspace and launch an agent",
		Long: `start atomically claims the top of the loom:todo queue (or the
issue specified with --issue), allocates a workspace, and launches the
named agent or tool inside it with WEAVE_* env vars set.

The trailing '-- <agent>' form is the human-natural shape; --tool is
the programmatic alternative.

Naming an AGENT is the way to select a model. A single token that names
an agent (a nickname, an alias, or a bare tool:model) expands from the
fleet registry into that tool's headless argv with its model selected,
and the issue body as the prompt:

    weave start --issue 3 -- 007
      -> claude --dangerously-skip-permissions --model fable "<issue body>"

The worker is stamped with the principal it acts as, so what it writes
resolves: BASHY_PRINCIPAL=dhnt:agent/007. WEAVE_AGENT stays the per-issue
SEAT (007-a) — the slot, not the agent. See "bashy agent" / "bashy whois".

ONE AGENT WORKS ONE RUN AT A TIME. An agent is a single identity — one
conversation store, one kb attribution, one bus cursor — so handing it two
live runs mixes their context and produces answers that look right and are
not. Starting an agent that is already working another run is therefore
refused, and the run simply stays queued for it to pick up next.

To actually run several at once, use --clone: it mints a per-issue
ephemeral agent (007-w3) with its own name and its own context, so the
runs are parallel because they are separate. Those workers are hidden from
"bashy agent list" (see --all) and are reclaimed when their run finishes.

Anything else is passed through untouched. A bare tool name (-- claude)
still launches raw. A multi-token raw executable argv is honored exactly as
written. A registered agent followed by a weave flag is rejected because that
is the flags-after-'--' mistake that silently disables agent expansion; put
weave flags before '--'.

POINTS ARE A HARD EXECUTION BUDGET. A pointed run gets a max-runtime even when
the flag is omitted: 1=3m45s, 2=7m30s, 3=11m15s, 5=18m45s, 8=30m. An explicit
--max-runtime may tighten that ceiling but cannot exceed it. Resume and
reassignment use the same cap. Legacy unpointed standalone runs remain
launchable, but a sprint refuses to link them.

PTY: by default the subagent runs inside a freshly-allocated PTY
(claude-code, codex, opencode and similar TUIs need one to render).
When stdout is a terminal the PTY passes through interactively;
when it isn't (orchestrator pipe / backgrounded by shell &) the
PTY output goes to a per-issue log file under the queue dir and
the file path appears in the result envelope.

On exit, the queue item's state becomes "submitted" (exit 0 with commits),
"no-op" (exit 0 with a clean tree and no commits), or "failed" (non-zero).
A dirty terminal tree is committed to its isolated branch for recovery before
that state is recorded; this preservation does not assert that the work passed.
The exit_code and finished_at are persisted.
"weave pull" picks up submitted branches; "weave wait --issue N"
blocks until N reaches a terminal state.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveStart(cmd, issue, tool, args, weaveStartOptions{
				noSpawn:     noSpawn,
				resume:      resume,
				clone:       cloneAgent,
				pty:         ptyMode,
				idleTimeout: idleTimeout,
				maxRuntime:  maxRuntime,
				memLimit:    memLimit,
			}, &flags)
		},
	}
	flags.attach(cmd)
	runFlag(cmd, &issue, "Claim a specific run instead of top-of-queue")
	cmd.Flags().StringVar(&tool, "tool", "", "Agent nickname, tool:model, or tool name (alternative to trailing -- <agent>)")
	cmd.Flags().BoolVar(&resume, "resume", false, "Reattach to an existing lease for the given issue")
	cmd.Flags().BoolVar(&noSpawn, "no-spawn", false, "Allocate the workspace but do not exec the tool")
	cmd.Flags().BoolVar(&cloneAgent, "clone", false, "If the named agent is already working another run, mint a per-issue ephemeral clone (own name, own context) instead of waiting for it")
	cmd.Flags().BoolVar(&autoCommit, "auto-commit", false, "Compatibility flag; dirty terminal trees are preserved automatically when verification permits")
	cmd.Flags().StringVar(&ptyMode, "pty", "auto", "PTY allocation: auto (default) | always | never")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 0, "Kill the subagent tree if no PTY output for this long (e.g. 5m); default off — caught the claude-TUI stuck case in the dogfood")
	cmd.Flags().DurationVar(&maxRuntime, "max-runtime", 0, "Hard wall-clock ceiling; pointed runs derive 1=3m45s,2=7m30s,3=11m15s,5=18m45s,8=30m and reject a larger explicit value; unpointed default off")
	cmd.Flags().StringVar(&memLimit, "mem-limit", "16g", "Kill the subagent tree when its total RSS exceeds this (e.g. 16g, 512m); 0 disables — the OOM backstop")
	return cmd
}

func newWeaveNextCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "next",
		Short: "Peek at the next issue 'weave start' would claim (non-mutating)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveNext(cmd, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeavePrioCmd() *cobra.Command {
	var flags weaveOutputFlags
	var auto bool
	cmd := &cobra.Command{
		Use:         "prio <issue> p0|p1|p2|p3",
		Short:       "Set an issue's priority tier (or --auto to LLM-rank the queue)",
		Annotations: map[string]string{weaveStatusAnnotation: "implemented; --auto requires an LLM provider"},
		// Validate inside RunE (not Args) so bad input emits a structured
		// invalid_arg envelope. The leaf's SilenceErrors would otherwise
		// swallow an Args-returned error, leaving a bare non-zero exit
		// with no message (the silent `prio <issue> --auto` foot-gun).
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := flags.mode()
			if auto {
				if len(args) != 0 {
					return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prio",
						weavecli.ExitInvalidArg, fmt.Errorf("--auto re-ranks the whole queue and takes no arguments (got %d); use `weave prio <issue> p0|p1|p2|p3` to set a single issue", len(args))))
				}
				return runWeavePrio(cmd, 0, "", true, &flags)
			}
			if len(args) != 2 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prio",
					weavecli.ExitInvalidArg, fmt.Errorf("expected: weave prio <issue> p0|p1|p2|p3 (or --auto to LLM-rank the queue)")))
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prio",
					weavecli.ExitInvalidArg, fmt.Errorf("issue must be an integer: %q", args[0])))
			}
			return runWeavePrio(cmd, id, args[1], false, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVar(&auto, "auto", false, "Delegate ranking to an LLM (re-ranks the whole queue)")
	return cmd
}

func newWeavePointCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "point <issue> <1|2|3|5|8>",
		Short: "Set an issue's story points (sprint planning; 8 = ~30m cap)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("points must be an integer: %q", args[1])
			}
			return runWeavePoint(cmd, id, n, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveListCmd() *cobra.Command {
	var flags weaveOutputFlags
	var watch bool
	var history bool
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show active weaves (--all: every queue on the machine)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if watch {
				return runWeaveListWatch(cmd, history, &flags)
			}
			if all {
				return runWeaveListAll(cmd, history, false, &flags)
			}
			return runWeaveList(cmd, history, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVar(&watch, "watch", false, "Stream state transitions (NDJSON when paired with --json)")
	cmd.Flags().BoolVar(&history, "history", false, "Include reaped/abandoned leases")
	cmd.Flags().BoolVar(&all, "all", false, "Every weave queue on the machine, grouped by repo")
	return cmd
}

func newWeavePauseCmd() *cobra.Command {
	var flags weaveOutputFlags
	var reason string
	cmd := &cobra.Command{
		Use:   "pause",
		Short: "Cleanly suspend every running weave worker",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeavePause(cmd, reason, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason recorded in the pause manifest")
	return cmd
}

func newWeaveResumeCmd() *cobra.Command {
	var flags weaveOutputFlags
	var issue int64
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Relaunch paused weave workers from their recorded launch specs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveResume(cmd, issue, &flags)
		},
	}
	flags.attach(cmd)
	runFlag(cmd, &issue, "Resume only one paused run")
	return cmd
}

func newWeaveAutopilotCmd() *cobra.Command {
	var flags weaveOutputFlags
	var fleet, brief, reviewAgent string
	var standby bool
	var leaseTTL, heartbeat, backoff time.Duration
	cmd := &cobra.Command{
		Use:   "autopilot --orchestrator-fleet 007,claude:opus[,codex...]",
		Short: "Autonomously drive THIS repo's run queue (claim→launch→verify→merge) with failover",
		Long: `autopilot holds a queue-scoped orchestration lease and launches one
roster member from the configured fleet as the active orchestrator. If the
orchestrator exits non-zero or emits overload/rate-limit output,
autopilot rotates to the next member and resumes from the same durable
weave queue.

A roster member may name an AGENT (a nickname, an alias, or a bare
tool:model) or a bare tool. Naming agents is what makes failover a MODEL
decision rather than only a binary one: --orchestrator-fleet claude:opus,
claude:fable rotates between two models on one CLI, which a tool-keyed
roster cannot express. The lease and the result record the binding.

The whole roster is resolved BEFORE the lease is taken. Naming an agent
asserts a model, and a binding that cannot run — a dangling tool, a
metered model with no vault key — is a configuration error you hear now
rather than at 3am, on failover, from a dead member.

With --standby, autopilot waits for the active holder's heartbeat to
expire before taking over. This supports a second process or machine as
a cold standby for unattended campaigns.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveAutopilot(cmd, weaveAutopilotOptions{
				fleetCSV:    fleet,
				briefPath:   brief,
				standby:     standby,
				leaseTTL:    leaseTTL,
				heartbeat:   heartbeat,
				backoff:     backoff,
				reviewAgent: reviewAgent,
			}, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&fleet, "orchestrator-fleet", "", "Comma-separated roster of agents (007, claude:opus) or tools, in preference order")
	cmd.Flags().StringVar(&brief, "brief", "", "Optional orchestration brief file to prepend to the queue context")
	cmd.Flags().BoolVar(&standby, "standby", false, "Wait for the orchestration lease to expire, then take over")
	cmd.Flags().DurationVar(&leaseTTL, "lease-ttl", 30*time.Second, "Orchestrator lease TTL")
	cmd.Flags().DurationVar(&heartbeat, "heartbeat", 5*time.Second, "Lease heartbeat interval while the orchestrator is alive")
	cmd.Flags().DurationVar(&backoff, "backoff", 10*time.Second, "Backoff after a full failed fleet rotation")
	cmd.Flags().StringVar(&reviewAgent, "review-agent", "", "Adversarial pair agent for every submitted run (default off; must differ from the coder)")
	return cmd
}

func newWeaveLogCmd() *cobra.Command {
	var flags weaveOutputFlags
	var follow bool
	var tailN int
	var summary bool
	cmd := &cobra.Command{
		Use:   "log <issue>",
		Short: "Print (or --follow) the captured PTY log of an issue's subagent",
		Long: `log prints the per-issue PTY capture file — everything the subagent
wrote to its terminal. The capture exists whenever 'weave start' ran
non-interactively (orchestrator pipe, backgrounded with &); a start
from a real terminal passes the PTY through instead, so there is
nothing to print.

  bashy weave log 4              # whole log so far
  bashy weave log 4 -n 100       # last 100 lines
  bashy weave log 4 -f           # stream live; exits when the issue
                                 # reaches a terminal state
  bashy weave log 4 -f -n 0      # follow, new output only

Output is the raw PTY byte stream (ANSI escapes included) — pipe
through 'less -R' for paging. Anyone on the host can watch a running
subagent this way; the file persists after the run as the post-
mortem artifact.

NOTE: some tools buffer in non-interactive modes (e.g. 'claude -p'
holds all output until exit) — an empty log under -f means "nothing
emitted yet", not "nothing happening". With --json, emits the log
metadata (path, size, state) instead of the raw stream — agent
callers read the file themselves.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			return runWeaveLog(cmd, id, follow, tailN, summary, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream appended output until the issue reaches a terminal state")
	cmd.Flags().IntVarP(&tailN, "tail", "n", -1, "Print only the last N lines (0 = none, useful with -f; -1 = whole file)")
	cmd.Flags().BoolVar(&summary, "summary", false, "Print a compact outcome (state, exit, verify, commits, merged) instead of the raw PTY stream")
	return cmd
}

func newWeaveRememberCmd() *cobra.Command {
	var flags weaveOutputFlags
	var issue int64
	var tags []string
	cmd := &cobra.Command{
		Use:   `remember "<text>"`,
		Short: "Append a manual persistent memory observation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveRemember(cmd, args[0], issue, tags, &flags)
		},
	}
	flags.attach(cmd)
	runFlag(cmd, &issue, "Associate the note with a run")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Tag for recall scoring (repeatable; comma-separated accepted)")
	return cmd
}

func newWeaveRecallCmd() *cobra.Command {
	var flags weaveOutputFlags
	var issue int64
	var files string
	cmd := &cobra.Command{
		Use:   "recall <query>",
		Short: "Recall persistent memory observations related to a query",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveRecall(cmd, args[0], issue, files, &flags)
		},
	}
	flags.attach(cmd)
	runFlag(cmd, &issue, "Boost observations for a run")
	cmd.Flags().StringVar(&files, "files", "", "Comma-separated file paths for overlap scoring")
	return cmd
}

func newWeaveMemoryCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "memory list|show <issue>|export",
		Short: "Inspect or export persistent weave memory",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "list" || args[0] == "export") {
				return nil
			}
			if len(args) == 2 && args[0] == "show" {
				_, err := parseMemoryIssueArg(args[1])
				return err
			}
			return fmt.Errorf("expected: weave memory list|show <issue>|export")
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var issue int64
			if len(args) == 2 {
				var err error
				issue, err = parseMemoryIssueArg(args[1])
				if err != nil {
					return err
				}
			}
			return runWeaveMemory(cmd, args[0], issue, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveAttachCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "attach <issue>",
		Short: "Interactively watch and steer a running subagent",
		Long: `attach streams the per-issue PTY capture while reading lines from
stdin and sending them to the running subagent's terminal. It is a
line-oriented same-host session: type instructions, then /detach (or
/quit) to leave. Detaching does not stop or kill the subagent.

The issue must be state=working with a live wrapper, a control socket,
and a PTY capture log.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			return runWeaveAttach(cmd, id, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveSayCmd() *cobra.Command {
	var flags weaveOutputFlags
	var tab, enter bool
	var raw string
	cmd := &cobra.Command{
		Use:   `say <issue> ["<text>"]`,
		Short: "Inject a line into a running subagent's terminal",
		Long: `say connects to the running wrapper's per-issue control socket and
types the text into the subagent's PTY, followed by Enter. To the
subagent it is indistinguishable from a human typing into its TUI —
so mid-run steering works the way you'd expect:

  bashy weave say 4 "/btw what is the status? reply in the log"
  bashy weave say 4 "stop exploring; commit what passes and exit"

Flags provide additional control over the injected bytes:

  --tab          Prepend a literal Tab keystroke before the text
  --enter        Send ONLY a bare Enter (the text arg becomes optional)
  --raw "<bytes>" Send C-style decoded bytes (\t \r \n \x1b) verbatim,
                  no implicit trailing Enter

Plain usage is unchanged (text + Enter). Anyone on the host can
inject; watch the reaction with 'weave log <issue> -f'.

Caveats: the issue must be state=working with a live wrapper that
allocated a PTY. Tools that don't read terminal input in their
non-interactive modes (e.g. 'claude -p') receive the keystrokes but
ignore them — use a TUI/streaming mode when you plan to steer.
Wrappers started by an older ycode have no control socket.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("issue required")
			}
			// Text is optional when --enter is used or when --raw provides the payload.
			if !enter && raw == "" && len(args) < 2 {
				return fmt.Errorf("text required (or use --enter for bare Enter, or --raw for verbatim bytes)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			text := ""
			if len(args) > 1 {
				text = strings.Join(args[1:], " ")
			}
			return runWeaveSay(cmd, id, text, tab, enter, raw, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVar(&tab, "tab", false, "Prepend a literal Tab keystroke")
	cmd.Flags().BoolVar(&enter, "enter", false, "Send only a bare Enter (text becomes optional)")
	cmd.Flags().StringVar(&raw, "raw", "", "Send C-style decoded bytes verbatim (\\t \\r \\n \\x1b etc.)")
	return cmd
}

func newWeavePullCmd() *cobra.Command {
	var flags weaveOutputFlags
	var watch bool
	var requireReview bool
	var force bool
	var reviewAgent string
	cmd := &cobra.Command{
		Use:   "pull [issue]",
		Short: "Fast-forward your local main with the merged agent branches",
		Long: `pull merges each submitted agent branch into the base branch, after
measuring — never merely believing — that it is mergeable: commits ahead,
a clean workspace, any configured verify/suite gates passing, and an intact
isolation baseline. A run need not configure a test gate to merge.

A run is "isolation-violated" when the LIVE checkout changed while the run
held its workspace: either the agent escaped its workspace and wrote here
directly, or you edited the repo while it ran. Either way its branch is no
longer the whole diff, so pull refuses it and names the paths. Review them,
then --force to merge anyway.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = watch
			var issueID int64
			var issueSpecified bool
			if len(args) > 1 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave pull",
					weavecli.ExitInvalidArg, fmt.Errorf("expected at most one issue argument")))
			}
			if len(args) == 1 {
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil || id <= 0 {
					return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave pull",
						weavecli.ExitInvalidArg, fmt.Errorf("invalid issue %q", args[0])))
				}
				issueID = id
				issueSpecified = true
			}
			return runWeavePull(cmd, &flags, issueID, issueSpecified, requireReview, force, false, reviewAgent)
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVar(&watch, "watch", false, "Daemonize: fast-forward whenever a PR merges")
	cmd.Flags().BoolVar(&requireReview, "require-review", false, "Require a passing `weave review` verdict before merging")
	cmd.Flags().BoolVar(&force, "force", false, "Merge a run flagged isolation-violated (the live checkout changed while it ran) anyway")
	cmd.Flags().StringVar(&reviewAgent, "review-agent", "", "Run an adversarial pair in each submitted workspace before verification (default off)")
	return cmd
}

func newWeaveReviewCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "review <issue>",
		Short: "Re-verify submitted work in a fresh clean-room checkout",
		Long: `review re-derives an issue's evidence in a fresh local clone of the
submitted branch, reruns the recorded verify command, and persists a structured
local verdict used by pull --require-review.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave review",
					weavecli.ExitInvalidArg, fmt.Errorf("expected exactly one issue argument")))
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave review",
					weavecli.ExitInvalidArg, fmt.Errorf("invalid issue %q", args[0])))
			}
			return runWeaveReview(cmd, id, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveReverifyCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "reverify <issue>",
		Short: "Refresh an issue's git and verify attestation from its workspace",
		Long: `reverify re-reads an issue's workspace git state and reruns its recorded
verify command, then updates the persisted attestation used by weave pull.
Use this after committing late/manual workspace residue so pull sees the fresh
clean HEAD instead of the stale terminal-time dirty record.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave reverify",
					weavecli.ExitInvalidArg, fmt.Errorf("expected exactly one issue argument")))
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave reverify",
					weavecli.ExitInvalidArg, fmt.Errorf("invalid issue %q", args[0])))
			}
			return runWeaveReverify(cmd, id, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveSalvageCmd() *cobra.Command {
	var flags weaveOutputFlags
	var reviewAgent string
	var noReview bool
	cmd := &cobra.Command{
		Use:   "salvage <issue>",
		Short: "Review and merge the committed work of a killed/failed item (via pull's gate)",
		Long: `salvage rescues an item whose committed work weave pull won't auto-merge
because its tool's (TUI) session was killed, leaving it in 'killed' state.

It promotes the item to 'submitted' only after confirming it has commits ahead
of the base branch and a clean working tree, then runs the normal pull merge —
so the dirty and verify-exit gates still apply. It is not a blind force.

Model review is optional: pass --review-agent <agent> to run the same pair as
'weave pull'. Without it, deterministic dirty, verify, suite and isolation gates
decide the merge. --no-review remains accepted as a compatibility no-op.

Salvage shows the diff it is about to merge, and never pushes: publishing
salvaged work to a remote is a separate, deliberate decision.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave salvage",
					weavecli.ExitInvalidArg, fmt.Errorf("expected exactly one issue argument")))
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave salvage",
					weavecli.ExitInvalidArg, fmt.Errorf("invalid issue %q", args[0])))
			}
			return runWeaveSalvage(cmd, &flags, id, reviewAgent, noReview)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&reviewAgent, "review-agent", "", "Run an adversarial pair in the workspace before merging (same gate as `weave pull --review-agent`)")
	cmd.Flags().BoolVar(&noReview, "no-review", false, "Compatibility flag; model review is already off unless --review-agent is supplied")
	return cmd
}

func newWeavePruneCmd() *cobra.Command {
	var flags weaveOutputFlags
	var yes bool
	var stale, force bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove workspace directories and merged branches for terminal items",
		Long: `prune cleans up after terminal queue items:
- Removes lingering workspace directories for done, abandoned, failed, and killed items
- Deletes agent/weave-issue-N branches from the user repo if fully merged

It REFUSES to delete a workspace that still holds work: unmerged commits, or an
uncommitted tree. Those are skipped with a reason, and --force is required to
remove them. For a queue-backed run, --force first commits a dirty tree and
imports its tip as refs/salvage/abandoned-<issue>; a preservation failure
refuses the removal.

That guard is the load-bearing one. An agent branch lives ONLY inside its
workspace clone until "weave pull" fetches it — so deleting the workspace does
not leave the commits "unmerged", it DESTROYS them. (The old help called prune
safe because branches are deleted with -d, which refuses unmerged. True, and
beside the point: -d protects the branch in the USER repo, which is not where
the work is.)

Use --yes to skip the confirmation prompt.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeavePrune(cmd, yes, stale, force, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "Remove guarded workspaces; queue-backed work is first preserved under refs/salvage/abandoned-<issue>")
	cmd.Flags().BoolVar(&stale, "stale", false, "Also sweep orphaned 'allocated' items (workspace created but never launched / launched-and-died with no commits) — clears leftover clutter from prior sessions; never touches items with committed work")
	return cmd
}

func newWeaveAbandonCmd() *cobra.Command {
	var flags weaveOutputFlags
	var reason string
	var yes, force bool
	cmd := &cobra.Command{
		Use:     "abandon <issue>",
		Aliases: []string{"resolve", "done", "dispose"},
		Short:   "Tear down a weave (workspace + branch + any running wrapper)",
		Long: `abandon stops the running wrapper (if any) AND removes the workspace
+ branch. Use this when giving up on an issue entirely, OR to dispose a run
whose issue is already satisfied (merged/superseded elsewhere) so it stops
inflating the board — hence the ` + "`resolve`/`done`/`dispose`" + ` aliases.

For "stop the runaway but keep the partial work for inspection",
use ` + "`weave kill`" + ` instead.

It REFUSES to tear down a workspace that still holds work not yet merged into
base: unmerged commits, or an uncommitted tree (the same guard ` + "`weave prune`" + `
has, and for the same reason — the branch lives ONLY inside the workspace
clone). Pass --force to destroy it anyway; --force first fetches the branch
tip into the user's repo as refs/salvage/abandoned-<issue>, so the commits
survive the teardown even when forced.

At a TTY this prompts before tearing down; pass --yes to skip the
prompt (required in non-interactive / --json invocations). --yes only skips
the prompt — it does not bypass the unmerged-work guard; use --force for that.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			return runWeaveAbandon(cmd, id, reason, yes, force, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&reason, "reason", "", "Optional human-readable reason for logs")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "Destroy the workspace even if it holds unmerged commits or uncommitted changes; unmerged commits are first preserved as refs/salvage/abandoned-<issue>")
	return cmd
}

func newWeaveStatusCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "status <issue>",
		Short: "Show an issue's reconciled state, merge status, and verify result",
		Long: `status answers "where does this issue stand?" without a manual git
investigation: it reports the recorded state reconciled against git
(a "submitted" item already in base reads as done), the branch +
workspace HEAD, whether the work is merged into the base branch, how
many commits it is ahead, and the last substrate-verified result.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			return runWeaveStatus(cmd, id, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveKillCmd() *cobra.Command {
	var flags weaveOutputFlags
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "kill <issue>",
		Short: "Stop the running wrapper precisely, preserve workspace + branch",
		Long: `kill SIGTERMs the recorded wrapper PID for the issue and flips the
queue item to state=failed. The workspace + branch + any commits the
subagent already made are preserved — the orchestrator can:

  - ` + "`weave shell <issue>`" + ` to inspect the partial work
  - ` + "`weave start --resume --issue N -- <tool>`" + ` to retry inside the same workspace
  - ` + "`weave abandon <issue>`" + ` to throw it all away

IMPORTANT for orchestrator agents: never shell out to ` + "`pkill`" + ` /
` + "`killall`" + ` / ` + "`kill -9`" + ` to stop a stuck subagent. Pattern matchers
also catch peer ycode / claude / codex sessions belonging to OTHER
agents on the same machine. ` + "`weave kill`" + ` reads the recorded
wrapper PID and signals only that process group — safe in shared
agentic environments.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			return runWeaveKill(cmd, id, reason, yes, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&reason, "reason", "", "Optional human-readable reason for the failure record")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

func newWeaveFinalizeCmd() *cobra.Command {
	var flags weaveOutputFlags
	var observedIdle bool
	cmd := &cobra.Command{
		Use:   "finalize <issue>",
		Short: "Terminalize an explicitly observed-idle interactive run",
		Long: `finalize is the narrow escape hatch for an interactive agent that has
reported completion and returned to its prompt but keeps its TUI process open.
It never parses terminal prose or guesses that an idle prompt is success: the
conductor must pass --observed-idle, then weave stops only the named wrapper and
measures its isolated workspace. A clean committed branch becomes submitted;
anything else remains failed for inspection.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			return runWeaveFinalize(cmd, id, observedIdle, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVar(&observedIdle, "observed-idle", false, "Attest that the named interactive agent has returned idle")
	return cmd
}

func newWeaveShellCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "shell <issue>",
		Short: "Drop into a shell inside the issue's workspace",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			return runWeaveShell(cmd, id, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveOpenCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "open <issue>",
		Short: "Surface an issue's workspace path (file:// URL)",
		Long: `open prints the file:// URL of an issue's workspace worktree so you
can jump straight to the files an agent produced. weave is local-only —
there is no remote page to open — so this resolves entirely on the
local filesystem.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			return runWeaveOpen(cmd, id, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveResetCmd() *cobra.Command {
	var flags weaveOutputFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Tear down every weave and remove all weave state for this project",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveReset(cmd, yes, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirm prompt")
	return cmd
}

func newWeaveWaitCmd() *cobra.Command {
	var flags weaveOutputFlags
	var issue int64
	var all bool
	var broker bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait [--issue N | --all]",
		Short: "Block until issue(s) reach a terminal state",
		Long: `wait polls the queue every 1s until the target reaches a terminal
state (submitted, no-op, failed, killed, done, or abandoned). Use --issue N to wait
on one issue or --all to wait until no non-terminal items remain.

Pairs with --detach-style backgrounding (` + "`bashy weave start ... &`" + `).
A typical orchestrator flow:

  bashy weave start --issue 1 -- codex 'fix #1' &
  bashy weave start --issue 2 -- claude-code 'fix #2' &
  bashy weave wait --all --timeout 30m
  bashy weave pull

Default timeout is 1h. On timeout, exits with precondition_failed
(exit code 3) so the caller can react.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveWait(cmd, issue, all, timeout, broker, &flags)
		},
	}
	flags.attach(cmd)
	runFlag(cmd, &issue, "Wait on a specific run ID")
	cmd.Flags().BoolVar(&all, "all", false, "Wait until no `working` items remain")
	cmd.Flags().BoolVar(&broker, "broker", false, "Auto-route live interactive auth/trust gates while waiting")
	cmd.Flags().DurationVar(&timeout, "timeout", time.Hour, "Maximum wait duration (e.g. 30m, 1h)")
	return cmd
}

func newWeaveSessionsCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Cloudbox shared session tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave sessions",
					weavecli.ExitInvalidArg, fmt.Errorf("expected no arguments")))
			}
			return runWeaveSessions(cmd, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveJoinCmd() *cobra.Command {
	var flags weaveOutputFlags
	var observer bool
	var once bool
	cmd := &cobra.Command{
		Use:   "join [task-id]",
		Short: "Join a Cloudbox shared session and follow its event feed",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave join",
					weavecli.ExitInvalidArg, fmt.Errorf("expected at most one task id")))
			}
			taskID := ""
			if len(args) == 1 {
				taskID = args[0]
			}
			return runWeaveJoin(cmd, taskID, observer, once, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVar(&observer, "observer", false, "Join with observer role instead of contributor")
	cmd.Flags().BoolVar(&once, "once", false, "Print the current continuity record and exit")
	return cmd
}

func newWeaveNoteCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   `note "<text>"`,
		Short: "Append a note to the joined shared session",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave note",
					weavecli.ExitInvalidArg, fmt.Errorf("expected exactly one note argument")))
			}
			return runWeaveNote(cmd, args[0], &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveSteerCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   `steer <run> "<text>"`,
		Short: "Append a session-scoped directive for a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave steer",
					weavecli.ExitInvalidArg, fmt.Errorf("expected: weave steer <run> \"<text>\"")))
			}
			return runWeaveSteer(cmd, args[0], args[1], &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveTakeCmd() *cobra.Command {
	var flags weaveOutputFlags
	var holder string
	var force bool
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "take",
		Short: "Claim the lease for the joined shared session",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave take",
					weavecli.ExitInvalidArg, fmt.Errorf("expected no arguments")))
			}
			return runWeaveTake(cmd, holder, force, ttl, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&holder, "as", "", "Lease holder name (default USER@hostname)")
	cmd.Flags().BoolVar(&force, "force", false, "Force-claim a lease held by someone else")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "Optional lease TTL (for example 30s, 5m)")
	return cmd
}

func newWeaveHandoffCmd() *cobra.Command {
	var flags weaveOutputFlags
	var to string
	cmd := &cobra.Command{
		Use:   "handoff --to <holder>",
		Short: "Record a manual handoff and release the session lease",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave handoff",
					weavecli.ExitInvalidArg, fmt.Errorf("expected no arguments")))
			}
			if to == "" {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave handoff",
					weavecli.ExitInvalidArg, fmt.Errorf("--to is required")))
			}
			return runWeaveHandoff(cmd, to, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&to, "to", "", "Successor lease holder")
	return cmd
}

func newWeaveRosterCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "roster",
		Short: "Show the joined session's current holder and recent participants",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave roster",
					weavecli.ExitInvalidArg, fmt.Errorf("expected no arguments")))
			}
			return runWeaveRoster(cmd, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveShareCmd() *cobra.Command {
	var flags weaveOutputFlags
	var role string
	cmd := &cobra.Command{
		Use:   "share <email>",
		Short: "Share the joined session with another user",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave share",
					weavecli.ExitInvalidArg, fmt.Errorf("expected exactly one email argument")))
			}
			return runWeaveShare(cmd, args[0], role, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&role, "role", "observer", "Share role: observer or contributor")
	return cmd
}

func newWeaveSharesCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "shares",
		Short: "List users shared into the joined session",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave shares",
					weavecli.ExitInvalidArg, fmt.Errorf("expected no arguments")))
			}
			return runWeaveShares(cmd, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveUnshareCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "unshare <email>",
		Short: "Revoke a user's share on the joined session",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave unshare",
					weavecli.ExitInvalidArg, fmt.Errorf("expected exactly one email argument")))
			}
			return runWeaveUnshare(cmd, args[0], &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func newWeaveCheckCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "check",
		Short: "List every subcommand and its implementation status",
		Long: `check enumerates the weave subverbs and reports, for each, whether
it is fully implemented on the local filesystem or degrades for a
path that needs an external dependency (today only ` + "`prio --auto`" + `,
which needs an LLM provider). It is a non-mutating introspection aid
for agents and humans auditing the surface.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := flags.mode()
			parent := cmd.Parent()
			if parent == nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave check",
					weavecli.ExitGenericFail, fmt.Errorf("weave command tree not initialized")))
			}
			type subStatus struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			}
			var rows []subStatus
			for _, sub := range parent.Commands() {
				// Skip self and any cobra-injected helper (help, and the
				// completion command on hosts that don't disable it).
				if sub == cmd || sub.Name() == "help" || sub.Name() == "completion" || !sub.IsAvailableCommand() {
					continue
				}
				status := "implemented"
				if s := sub.Annotations[weaveStatusAnnotation]; s != "" {
					status = s
				}
				rows = append(rows, subStatus{Name: sub.Name(), Status: status})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

			if mode == weavecli.OutputJSON {
				subs := make([]map[string]any, len(rows))
				for i, r := range rows {
					subs[i] = map[string]any{"name": r.Name, "status": r.Status}
				}
				return ec(emitOK(cmd.OutOrStdout(), mode, "weave check", map[string]any{
					"subcommands": subs,
				}))
			}
			for _, r := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "%-12s %s\n", r.Name, r.Status)
			}
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave check", nil))
		},
	}
	flags.attach(cmd)
	return cmd
}
