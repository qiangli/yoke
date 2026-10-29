// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
)

// NewDelegateCmd builds `bashy delegate` — the ergonomic verb for handing a task to an
// agent: a DIFFERENT one (codex, claude, a tool:model), or YOURSELF (the same tool
// driving this shell, run detached so you stay responsive to the user).
//
// It is the LIGHTWEIGHT one-shot path over the same primitive `invoke` uses. For heavier
// ISOLATED work — its own workspace, a gate, and a merge — the steward routes to
// `bashy weave` / the conductor instead; delegating a tracked todo through
// `weave add --from-todo` also auto-flips that todo to "assigned".
//
// Design of record: bashy/docs/delegate-verb-design.md.
func NewDelegateCmd() *cobra.Command {
	var opt Options
	var self bool
	var model string
	var account string
	var allow string
	var idle time.Duration
	var budget float64
	var tokens int64
	cmd := &cobra.Command{
		Use:   "delegate <agent|self> <instruction...>",
		Short: "hand a task to an agent — another one, or yourself (same tool, run detached)",
		Long: `delegate hands a task to an agent and returns its result.

  bashy delegate codex "add a test for X"      # a DIFFERENT agent — fresh context, task = brief
  bashy delegate self "summarize the changes"   # YOURSELF — same tool, run detached to stay responsive
  bashy delegate --agent claude:opus "…"        # a specific tool:model

The FIRST positional is the target (an agent nickname, 'self', or tool:model); the rest is
the instruction. Or set --agent/--self and pass the whole line as the instruction.

delegate is the lightweight one-shot. For heavier ISOLATED work (its own workspace + gate +
merge), route to 'bashy weave' / the conductor; delegating a tracked todo with
'weave add --from-todo' also auto-flips its status to 'assigned'.

Delegation requires a verified subscription login (currently claude and codex).
--allow or BASHY_DELEGATE_ALLOW restricts resolved tool:model bindings; a caller
with BASHY_DELEGATE_CALLER=genie must supply a nonempty allow policy.
--idle-timeout watches stdout/stderr activity. Required --budget-usd/--max-tokens
caps fail closed when the vendor adapter cannot enforce them.
An enclosing attestation launcher may export its receipt as BASHY_ATTEST_ID.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --account resolves to the agent binding linked to that
			// account and then behaves as --agent. It runs FIRST, so the
			// conflicts refuse before anything launches and the positional
			// below stays part of the instruction.
			if err := delegateAccountConflict(account, opt.Agent, model, self); err != nil {
				return err
			}
			if strings.TrimSpace(account) != "" {
				binding, err := resolveDelegateAccount(cmd.Context(), strings.TrimSpace(account))
				if err != nil {
					return err
				}
				opt.Agent = binding
			}
			// The first positional is the target unless --agent/--self already named one.
			if opt.Agent == "" && !self && len(args) > 0 {
				if first := strings.TrimSpace(args[0]); strings.EqualFold(first, "self") {
					self = true
				} else {
					opt.Agent = first
				}
				args = args[1:]
			}
			if self {
				tool, ok := fleet.DetectTool()
				if !ok || strings.TrimSpace(tool) == "" {
					return fmt.Errorf("delegate self: cannot detect the tool driving this shell — name a target explicitly (bashy delegate <agent> ...)")
				}
				// --model transplants the inherited context onto a DIFFERENT model
				// (fresh eyes); without it the fork inherits the session's model.
				if m := strings.TrimSpace(model); m != "" {
					opt.Agent = tool + ":" + m
				} else {
					opt.Agent = tool
				}
				// TRUE context-inheriting fork when the tool declares a headless,
				// non-mutating fork (fork_exec) AND its session id is readable — the
				// spawn inherits your live transcript, no re-briefing. Otherwise fall
				// back to a fresh same-tool instance (task = brief).
				if t, known := newCatalog().Tool(tool); known && t.CanFork() {
					if sid := t.CurrentSession(); sid != "" {
						opt.Fork, opt.Session = true, sid
						if model != "" {
							fmt.Fprintf(cmd.ErrOrStderr(), "delegate self: forking your %s session onto %s (context inherited)\n", tool, model)
						} else {
							fmt.Fprintf(cmd.ErrOrStderr(), "delegate self: forking your %s session (context inherited)\n", tool)
						}
					} else {
						fmt.Fprintf(cmd.ErrOrStderr(), "delegate self: %s can fork but its session id is not set — running a fresh instance (task = brief)\n", tool)
					}
				}
			}
			if strings.TrimSpace(opt.Agent) == "" && strings.TrimSpace(opt.Role) == "" {
				return fmt.Errorf("delegate: name a target — an agent, 'self', or tool:model (bashy delegate <agent> <instruction>)")
			}
			if len(args) > 0 {
				opt.Instruction = strings.TrimSpace(strings.Join(append([]string{strings.TrimSpace(opt.Instruction)}, args...), " "))
			}
			if strings.TrimSpace(opt.Instruction) == "" {
				return fmt.Errorf("delegate: what should the agent do? add an instruction")
			}
			plain, _ := cmd.Flags().GetBool("plain")
			opt.JSON = opt.JSON || (os.Getenv("BASHY_AGENTIC") != "" && !plain)
			g := &delegateGuard{allow: allow, genie: os.Getenv("BASHY_DELEGATE_CALLER") == "genie", idle: idle,
				capRequired: cmd.Flags().Changed("budget-usd") || cmd.Flags().Changed("max-tokens")}
			if !cmd.Flags().Changed("allow") {
				g.allow = os.Getenv("BASHY_DELEGATE_ALLOW")
			}
			ctx := context.WithValue(cmd.Context(), delegateKey{}, g)
			if opt.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, opt.Timeout)
				defer cancel()
			}
			var raw Result
			var invokeErr error
			if idle < 0 || opt.Timeout < 0 {
				raw = Result{SchemaVersion: schemaVersion, Agent: opt.Agent}
				invokeErr = delegateFailure("denied", "delegate: timeouts must not be negative")
			} else {
				raw, invokeErr = Invoke(ctx, opt, execRunner{})
			}
			res, err := finishDelegate(raw, invokeErr, g)
			if opt.JSON {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else if res.Output != "" {
				fmt.Fprint(cmd.OutOrStdout(), res.Output)
				if !strings.HasSuffix(res.Output, "\n") {
					fmt.Fprintln(cmd.OutOrStdout())
				}
			}
			return err
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.Flags().StringVar(&opt.Agent, "agent", "", "target agent (nickname or tool:model); alternative to the first positional")
	cmd.Flags().BoolVar(&self, "self", false, "delegate to YOURSELF — the same tool driving this shell, run detached (forks your live context when the tool can)")
	cmd.Flags().StringVar(&model, "model", "", "with --self: transplant your inherited context onto a DIFFERENT model (e.g. opus) — best-effort")
	cmd.Flags().StringVar(&account, "account", "", "delegate to the agent binding linked to this account alias or id (see 'bashy account'); conflicts with --agent/--self/--model")
	cmd.Flags().StringVar(&opt.Role, "role", "", "role alias when no agent is named: conductor, reviewer, qa, release")
	cmd.Flags().StringVar(&opt.Task, "task", "", "safe one-line assignment label shown in the live agent roster")
	cmd.Flags().StringVar(&opt.Instruction, "instruction", "", "the instruction (or pass it as positional words)")
	cmd.Flags().StringArrayVar(&opt.Files, "file", nil, "append file contents to the instruction")
	cmd.Flags().StringArrayVar(&opt.Context, "context", nil, "append context text to the instruction")
	cmd.Flags().StringVar(&opt.Cwd, "cwd", "", "working directory for the agent process")
	cmd.Flags().DurationVar(&opt.Timeout, "timeout", 0, "agent timeout, e.g. 30m")
	cmd.Flags().DurationVar(&idle, "idle-timeout", 0, "fail when the child stops writing output for this duration")
	cmd.Flags().StringVar(&allow, "allow", "", "allowed tool:model pairs (comma separated); defaults to BASHY_DELEGATE_ALLOW")
	cmd.Flags().Float64Var(&budget, "budget-usd", 0, "require a hard dollar cap; refuse adapters that cannot enforce it")
	cmd.Flags().Int64Var(&tokens, "max-tokens", 0, "require a hard total token cap; refuse adapters that cannot enforce it")
	cmd.Flags().StringVar(&opt.Sandbox, "sandbox", "", "sandbox override, e.g. workspace-write or danger-full-access")
	cmd.Flags().StringArrayVar(&opt.WritableRoots, "writable-root", nil, "explicitly grant an extra writable path to a --sandbox workspace-write launch (repeatable)")
	cmd.Flags().BoolVar(&opt.JSON, "json", false, "print a bashy-chat-v1 JSON result envelope")
	cmd.Flags().BoolVar(&opt.DryRun, "dry-run", false, "print the resolved invocation (incl. a self-fork) without running it")
	cmd.Flags().Bool("plain", false, "force plain output even under BASHY_AGENTIC")
	_ = cmd.Flags().MarkHidden("plain")
	return cmd
}
