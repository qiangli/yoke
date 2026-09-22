// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/spf13/cobra"
)

// NewClaimCmd builds `bashy claim` — ONE verb, three subcommands.
//
// One verb, not three (claim / claims / release), because the Command Atlas asks of
// every front-door verb: which stage do you serve that nothing else already does?
// "Take a claim", "list claims" and "release a claim" are one concern, and giving
// each its own top-level word is exactly the accretion the coherence pass exists to
// stop.
func NewClaimCmd(roots func() []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claim [name] [-- command...]",
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
their path sets INTERSECT.

A NAME claims one shared resource on this host. The detached form is a lease across
agent invocations; ` + "`bashy claim NAME -- COMMAND`" + ` is a kernel-backed hold for the
child lifetime. Both are advisory and HOST-LOCAL: they coordinate agents here and do
not prove that a remote machine is idle.

It refuses on CONFLICT, never on absence: the claim is taken silently on your first
write, and you are stopped only when someone else already holds one.`,
		Example: `  bashy claim                     # take/refresh the claim on this project
	  bashy claim do1 --intent "sprint 251 tests"
	  bashy claim do1 -- make test       # hold do1 for the child lifetime
	  bashy claim list                # who is working right now, and where?
	  bashy claim release do1         # let someone else have the resource`,
		Args: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash >= 0 {
				if dash != 1 || len(args) < 2 {
					return fmt.Errorf("usage: bashy claim NAME -- COMMAND [ARG...]")
				}
				return nil
			}
			if len(args) > 1 {
				return fmt.Errorf("claim accepts one resource name; use -- before a child command")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			intent, _ := cmd.Flags().GetString("intent")
			force, _ := cmd.Flags().GetBool("force")
			wait, _ := cmd.Flags().GetDuration("wait")
			if cmd.ArgsLenAtDash() >= 0 {
				if force {
					return fmt.Errorf("--force is not valid with a child-scoped kernel hold")
				}
				c, l, err := AcquireAttached(DefaultDir(), args[0], Self(), intent, wait)
				if err != nil {
					return err
				}
				child := exec.CommandContext(cmd.Context(), args[1], args[2:]...)
				child.Stdin = cmd.InOrStdin()
				child.Stdout = cmd.OutOrStdout()
				child.Stderr = cmd.ErrOrStderr()
				runErr := child.Run()
				releaseErr := ReleaseAttached(DefaultDir(), c, l)
				if runErr != nil {
					return runErr
				}
				return releaseErr
			}
			if len(args) == 1 {
				c, err := AcquireResourceWithin(DefaultDir(), args[0], Self(), intent, force, wait)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "claim: %s held on %s by %s (%s)\n",
					c.Resource, c.Holder.Host, c.Holder.Name, c.Mode)
				return nil
			}
			if wait > 0 {
				return fmt.Errorf("--wait requires a named resource")
			}
			c, err := Acquire(DefaultDir(), roots(), Self(), intent, force)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "claim: %s held by %s (%d root(s))\n",
				c.Project, c.Holder.Name, len(c.Roots))
			return nil
		},
	}
	cmd.Flags().String("intent", "", "what you are doing (shown to whoever collides with you)")
	cmd.Flags().Bool("force", false, "take it even if someone else holds it (recorded)")
	cmd.Flags().Duration("wait", 0, "wait up to this duration for a named resource")

	list := &cobra.Command{
		Use:   "list",
		Short: "who is working right now, and where?",
		Long: `list answers a question that nothing in this codebase could answer before it:
WHO IS WORKING RIGHT NOW, AND WHERE?

weave knew about its own issues. sprint knew about its own board. Nothing knew about
a plain agent session a human launched in a terminal — which is precisely how two
sessions became invisible to each other.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			claims, err := List(DefaultDir())
			if err != nil {
				return err
			}
			now := time.Now()
			if asJSON {
				b, _ := json.MarshalIndent(claims, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			if len(claims) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "claim: nobody is working on this host")
				return nil
			}
			for _, c := range claims {
				state := string(c.Liveness(now))
				target, mode := c.Project, "project"
				if c.Resource != "" {
					target, mode = c.Resource, c.Mode
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-18s %-22s %-10s %-10s %s\n",
					c.Holder.Name, target, state, mode, c.Intent)
			}
			return nil
		},
	}
	list.Flags().Bool("json", false, "emit the claims")

	release := &cobra.Command{
		Use:   "release [name]",
		Short: "drop this session's project claim or a named resource hold",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if len(args) == 1 {
				err = ReleaseResource(DefaultDir(), args[0], Self())
			} else {
				err = Release(DefaultDir(), Self())
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "claim: released")
			return nil
		},
	}

	var requestMessage string
	request := &cobra.Command{
		Use:   "request",
		Short: "Ask the live conflicting owner to review/merge or release the project claim",
		Long: `request does not steal or bypass a claim. It finds the live owner whose
path set conflicts with this project and sends that identity a durable, urgent
coordination message through the Bashy bus. The owner decides whether to merge,
sequence, hand off, or release; until then the git write guard continues to refuse.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			self := Self()
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
			to := strings.TrimSpace(conflict.Holder.Name)
			if to == "" {
				return fmt.Errorf("conflicting claim has no addressable owner name")
			}
			from := strings.TrimSpace(self.Name)
			if from == "" {
				from = "agent"
			}
			body := strings.TrimSpace(requestMessage)
			if body == "" {
				body = fmt.Sprintf("MERGE REQUEST: %s requests review/merge sequencing or release of the %s project claim", from, conflict.Project)
			}
			if err := bus.Publish(bus.Notification{Principal: from, To: to, Body: body, Priority: bus.DeliveryInterrupt}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "claim request: sent to %s; write lock remains enforced until that owner coordinates or releases\n", to)
			return nil
		},
	}
	request.Flags().StringVarP(&requestMessage, "message", "m", "", "merge/sequencing request (default names requester and project)")

	cmd.AddCommand(list, request, release)
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
