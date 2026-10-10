// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/spf13/cobra"
)

// ExitConflict is the exit status of a refused claim, shared with the git
// write guard so a script tests one number for "someone else holds it".
const ExitConflict = 9

// claimPublish is the bus seam `claim request` sends through.
var claimPublish = bus.Publish

// exitError carries a process exit status through cobra's error plumbing.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }
func (e *exitError) ExitCode() int { return e.code }

// refuse reports a conflict the way an agent can act on it: the explanation on
// stderr (the JSON document on stdout under --json) and exit 9. Any other
// error passes through untouched.
func refuse(cmd *cobra.Command, err error) error {
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		return err
	}
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		if b, jerr := conflict.JSON(); jerr == nil {
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
		}
	} else {
		fmt.Fprint(cmd.ErrOrStderr(), conflict.Error())
	}
	return &exitError{code: ExitConflict, err: conflict}
}

// targetRef reads a claim target. --kind K NAME claims NAME ad hoc under K
// exactly as written; otherwise "kind:NAME" names its kind and a bare NAME is a
// name ref, which the engine resolves to a registered resource (by name or
// alias) before it falls back to a plain host-local name.
func targetRef(cmd *cobra.Command, arg string) Ref {
	if kind, _ := cmd.Flags().GetString("kind"); strings.TrimSpace(kind) != "" {
		return Ref{Kind: strings.TrimSpace(kind), Name: strings.TrimSpace(arg)}
	}
	return ParseRef(arg)
}

func ctxOf(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// shown is a claim's address as a user types it: a bare name for the default
// kind, kind:name otherwise, the project label for a project claim.
func shown(c *Claim) string {
	if c.Resource == "" {
		return c.Project
	}
	if a := c.Address(); a.Kind != KindName {
		return a.String()
	}
	return c.Resource
}

// NewClaimCmd builds `bashy claim` — ONE verb, subcommands for the rest.
//
// One verb, not three (claim / claims / release), because the Command Atlas asks of
// every front-door verb: which stage do you serve that nothing else already does?
// "Take a claim", "list claims" and "release a claim" are one concern, and giving
// each its own top-level word is exactly the accretion the coherence pass exists to
// stop.
func NewClaimCmd(roots func() []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claim [kind:]NAME [-- command...]",
		Short: "hold a project or named shared resource while you work",
		Long: `claim stops two agents from writing the same project at the same time.

It exists because that is not hypothetical. Two agent sessions worked these repos
with no coordinator: one swept the other's STAGED submodule pins into its own commit,
landing an untested engine regression that took the release gate from 86/86 to 85/86.
The other found an unexplained edit in the tree and had to guess whose it was.
Neither could see that the other existed.

Communication is not coordination — two agents chatting politely still stomp one
another's git index. What prevents collision is isolation, then a claim, then a gate.
This is the middle one.

SCOPE IS A PATH SET, not a repo. That regression proves why: the bug lived in one
repo, the gate that would have caught it in a second, and the pin that carried it in
a third. A claim on any one .git root would have prevented nothing. So a claim covers
the PROJECT — the repo plus the siblings it depends on — and two claims conflict when
their path sets INTERSECT. With no argument, claim holds the current project.

A TARGET names one shared thing, as KIND:NAME. A bare NAME resolves in order: a
registered resource (by name or alias), then an ad-hoc host-local name. KIND:NAME
uses that kind — path:/w/app, or any kind the registry derives (model:NAME,
command:NAME). --kind K NAME claims ad hoc under K without registering anything.

A RESOURCE is how an operator names something agents must not use at the same
time. Register it once, claim it by name:

  bashy resource add openai-models --kind model gpt-x gpt-y
  bashy claim openai-models

The resource carries its members and the kind it is held under, so the claim
conflicts with a claim on any of them — a direct model:gpt-x included.

The detached form is a lease across agent invocations; ` + "`bashy claim TARGET -- COMMAND`" + ` is a
kernel-backed hold for the child lifetime. Both are advisory and HOST-LOCAL: they
coordinate agents here and do not prove that a remote machine is idle. Every claim
carries an EPOCH, a fencing token that rises each time the claim changes hands;
release and refresh accept --epoch to act only on the claim you meant.

It refuses on CONFLICT, never on absence: the claim is taken silently on your first
write, and you are stopped only when someone else already holds one. A refusal
prints who holds it and how to reach them on stderr and exits 9 (--json prints the
bashy-claim-conflict-v1 document instead).`,
		Example: `  bashy claim                     # take/refresh the claim on this project
  bashy claim do1 --intent "sprint 251 tests"
  bashy claim do1 -- make test       # hold do1 for the child lifetime
  bashy resource add openai-models --kind model gpt-x gpt-y
  bashy claim openai-models          # a registered resource, by name
  bashy claim model:gpt-x --wait 10m # a registry-derived kind, waiting for the holder
  bashy claim --kind path /w/app     # ad hoc, nothing registered
  bashy claim list                # who is working right now, and where?
  bashy claim request openai-models -m "need it for a 5 minute eval"
  bashy claim refresh do1         # heartbeat a long-held lease
  bashy claim release do1         # let someone else have the resource
  bashy claim prune               # clear lapsed claims no live holder holds`,
		Args: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash >= 0 {
				if dash != 1 || len(args) < 2 {
					return fmt.Errorf("usage: bashy claim [kind:]NAME -- COMMAND [ARG...]")
				}
				return nil
			}
			if len(args) > 1 {
				return fmt.Errorf("claim accepts one target; use -- before a child command")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			intent, _ := cmd.Flags().GetString("intent")
			force, _ := cmd.Flags().GetBool("force")
			wait, _ := cmd.Flags().GetDuration("wait")
			asJSON, _ := cmd.Flags().GetBool("json")
			ctx := ctxOf(cmd)
			if cmd.ArgsLenAtDash() >= 0 {
				if force {
					return fmt.Errorf("--force is not valid with a child-scoped kernel hold")
				}
				g, l, err := AcquireAttachedRef(ctx, Request{
					Ref: targetRef(cmd, args[0]), Holder: Self(), Intent: intent, Mode: ModeAttached, Wait: wait,
				})
				if err != nil {
					return refuse(cmd, err)
				}
				child := exec.CommandContext(ctx, args[1], args[2:]...)
				child.Stdin = cmd.InOrStdin()
				child.Stdout = cmd.OutOrStdout()
				child.Stderr = cmd.ErrOrStderr()
				runErr := child.Run()
				releaseErr := ReleaseAttached(DefaultDir(), g.Claim, l)
				if runErr != nil {
					return runErr
				}
				return releaseErr
			}
			var g Grant
			var err error
			if len(args) == 1 {
				g, err = AcquireRef(ctx, Request{
					Ref: targetRef(cmd, args[0]), Holder: Self(), Intent: intent, Force: force, Wait: wait,
				})
			} else {
				g, err = store{DefaultDir()}.acquireWait(ctx, acquireSpec{
					req:    Request{Holder: Self(), Intent: intent, Force: force},
					legacy: true, roots: roots(),
				}, wait)
			}
			if err != nil {
				return refuse(cmd, err)
			}
			c := g.Claim
			if asJSON {
				b, _ := json.MarshalIndent(c, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			if c.Resource == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "claim: %s held by %s (%d root(s))\n",
					c.Project, c.Holder.Name, len(c.Roots))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "claim: %s held on %s by %s (%s, epoch %d)\n",
				shown(c), c.Holder.Host, c.Holder.Name, c.Mode, g.Epoch)
			return nil
		},
	}
	cmd.Flags().String("intent", "", "what you are doing (shown to whoever collides with you)")
	cmd.Flags().Bool("force", false, "take it even if someone else holds it (recorded)")
	cmd.Flags().Duration("wait", 0, "wait up to this duration for a held target")
	cmd.PersistentFlags().String("kind", "", "claim NAME ad hoc under this kind without registering it (e.g. --kind path /w/app)")
	cmd.Flags().Bool("json", false, "emit the claim, or the conflict document on refusal")

	list := &cobra.Command{
		Use:   "list",
		Short: "who is working right now, and where?",
		Long: `list answers a question that nothing in this codebase could answer before it:
WHO IS WORKING RIGHT NOW, AND WHERE?

weave knew about its own issues. sprint knew about its own board. Nothing knew about
a plain agent session a human launched in a terminal — which is precisely how two
sessions became invisible to each other.

Claims held in other backends (a sprint, a steward seat) are listed too. EPOCH is
the claim's fencing token.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			claims, err := List(DefaultDir())
			if err != nil {
				return err
			}
			now := time.Now()
			for _, c := range claims {
				c.normalize()
			}
			if asJSON {
				if claims == nil {
					claims = []*Claim{}
				}
				b, _ := json.MarshalIndent(claims, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			if len(claims) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "claim: nobody is working on this host")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "HOLDER\tKIND\tNAME\tSTATE\tMODE\tEPOCH\tINTENT")
			for _, c := range claims {
				mode := c.Mode
				if c.Resource == "" {
					mode = "project"
				}
				epoch := "-"
				if c.Epoch > 0 {
					epoch = strconv.FormatUint(c.Epoch, 10)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					c.Holder.Name, c.Kind, shown(c), c.Liveness(now), mode, epoch, c.Intent)
			}
			return w.Flush()
		},
	}
	list.Flags().Bool("json", false, "emit the claims")

	release := &cobra.Command{
		Use:   "release [[kind:]NAME]",
		Short: "drop this session's project claim or a held target",
		Long: `release drops a claim you hold. With no argument it drops this session's
project claim. --epoch N acts only if the claim is still at that epoch — if it
was taken over, or you released and re-acquired it since, release fails with a
fencing error rather than dropping a claim you no longer held; epoch 0 (the
default) means your current claim. Both forms honour it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			epoch, _ := cmd.Flags().GetUint64("epoch")
			var err error
			what := "project claim"
			if len(args) == 1 {
				ref := targetRef(cmd, args[0])
				what = ref.String()
				err = ReleaseRef(ctxOf(cmd), ref, Self(), epoch)
			} else {
				err = ReleaseEpoch(DefaultDir(), Self(), epoch)
			}
			if err != nil {
				return refuse(cmd, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "claim: released %s\n", what)
			return nil
		},
	}
	release.Flags().Uint64("epoch", 0, "release only if the claim is still at this epoch (0 = my current claim)")

	refresh := &cobra.Command{
		Use:   "refresh [kind:]NAME",
		Short: "heartbeat a claim you hold so it does not lapse",
		Long: `refresh is the explicit heartbeat: it extends a lease you already hold without
changing its epoch. It never takes a claim — a target you do not hold, or one
taken over since --epoch N, is refused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			epoch, _ := cmd.Flags().GetUint64("epoch")
			ref := targetRef(cmd, args[0])
			g, err := Refresh(ctxOf(cmd), ref, Self(), epoch)
			if err != nil {
				return refuse(cmd, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "claim: refreshed %s (epoch %d)\n", shown(g.Claim), g.Epoch)
			return nil
		},
	}
	refresh.Flags().Uint64("epoch", 0, "refresh only if the claim is still at this epoch (0 = my current claim)")

	var requestMessage string
	request := &cobra.Command{
		Use:   "request [[kind:]NAME]",
		Short: "Ask the live holder to review/merge or release a claim",
		Long: `request does not steal or bypass a claim. It finds the live holder of TARGET
(or, with no argument, the live owner whose path set conflicts with this project)
and sends that identity a durable, urgent coordination message through the Bashy
bus. The owner decides whether to merge, sequence, hand off, or release; until then
the claim keeps refusing.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			self := Self()
			from := strings.TrimSpace(self.Name)
			if from == "" {
				from = "agent"
			}
			var holder, body string
			if len(args) == 1 {
				ref := targetRef(cmd, args[0])
				var conflict *Conflict
				// ConflictFor, not Guard: the question is who the caller cannot
				// TAKE this from, and an acquisition refuses an announce-mode
				// claim that a Guard would wave through.
				if err := ConflictFor(ctxOf(cmd), self, Use{Kind: ref.Kind, Name: ref.Name}); !errors.As(err, &conflict) {
					if err != nil {
						return err
					}
					return fmt.Errorf("no live holder of %s other than you; take it with `bashy claim%s`", ref, shellWords(refArgs(ref)))
				}
				holder = conflict.holder()
				body = fmt.Sprintf("CLAIM REQUEST: %s asks you to release or sequence %s", from, conflict.Claim.Address())
			} else {
				claims, err := List(DefaultDir())
				if err != nil {
					return err
				}
				var conflict *Claim
				for _, c := range claims {
					if c.Conflicts(roots(), self, time.Now()) {
						conflict = c
						break
					}
				}
				if conflict == nil {
					return fmt.Errorf("no live conflicting project claim; take it with `bashy claim --intent <work>`")
				}
				holder = conflict.Holder.Name
				body = fmt.Sprintf("MERGE REQUEST: %s requests review/merge sequencing or release of the %s project claim", from, conflict.Project)
			}
			to := strings.TrimSpace(holder)
			if to == "" {
				return fmt.Errorf("conflicting claim has no addressable owner name")
			}
			if m := strings.TrimSpace(requestMessage); m != "" {
				body = m
			}
			if err := claimPublish(bus.Notification{Principal: from, To: to, Body: body, Priority: bus.DeliveryInterrupt}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "claim request: sent to %s; write lock remains enforced until that owner coordinates or releases\n", to)
			return nil
		},
	}
	request.Flags().StringVarP(&requestMessage, "message", "m", "", "merge/sequencing request (default names requester and target)")

	prune := &cobra.Command{
		Use:   "prune",
		Short: "remove lapsed claims no live holder holds",
		Long: `prune clears claims whose heartbeat is past the TTL — project claims
and named resource holds left behind by agents that stopped without
releasing. Live and attached (kernel-locked) claims are never touched, so
pruning while others work is safe.`,
		Example: `  bashy claim prune              # clear lapsed claims
  bashy claim prune --dry-run    # show what would go, remove nothing
  bashy claim prune --json         # machine-readable removals`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			asJSON, _ := cmd.Flags().GetBool("json")
			now := time.Now()
			pruned, err := Prune(DefaultDir(), now, dryRun)
			if err != nil {
				return err
			}
			if asJSON {
				if pruned == nil {
					pruned = []*Claim{}
				}
				b, _ := json.MarshalIndent(pruned, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			if len(pruned) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "claim: nothing lapsed to prune")
				return nil
			}
			verb := "pruned"
			if dryRun {
				verb = "would prune"
			}
			for _, c := range pruned {
				target, mode := c.Project, "project"
				if c.Resource != "" {
					target, mode = c.Resource, c.Mode
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %-18s %-22s %-10s age %s %s\n",
					verb, c.Holder.Name, target, mode,
					now.Sub(c.Heartbeat).Round(time.Second), c.Intent)
			}
			return nil
		},
	}
	prune.Flags().Bool("dry-run", false, "show what would be removed without removing it")
	prune.Flags().Bool("json", false, "emit the pruned claims")

	cmd.AddCommand(list, request, release, refresh, prune)
	return cmd
}

// Enforce is the decision the shell middleware asks for on every write.
//
// It returns nil when the write may proceed (the claim is taken silently), and a
// *Conflict when someone else holds the project. `force` comes from
// BASHY_CLAIM_FORCE, and it is recorded rather than hidden — an override nobody can
// see is an override nobody can audit.
func Enforce(roots []string, intent string) error {
	force := os.Getenv("BASHY_CLAIM_FORCE") != "" && os.Getenv("BASHY_CLAIM_FORCE") != "0"
	_, err := Acquire(DefaultDir(), roots, Self(), intent, force)
	return err
}
