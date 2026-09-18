package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newSubscribeCmd() *cobra.Command {
	var s Subscription
	var as string

	cmd := &cobra.Command{
		Use:   "subscribe [flags]",
		Short: "register a standing interest the sidecar holds on your behalf",
		Long: `subscribe records what an agent cares about, so the sidecar can decide —
off that agent's critical path — which notifications are its business.

  bashy bus subscribe --as dev-a --topic 'code.api.*' \
      --instance dev-a-session --interrupt-from steward

Topics are hierarchical and match by whole dotted segments: 'code.api.*' matches
code.api.Foo and code.api.Foo.renamed, but not code.apiary.

--interrupt-from is the governance boundary and DEFAULTS TO NOBODY. Anyone may
publish to you (it arrives queued, read at your next turn boundary); only the
principals you name may break into a running turn. Interrupts from anyone else
are demoted to queued, never dropped.

Subscriptions are meant to be scoped to a piece of work: set one up when an agent
starts an issue, remove it when the agent finishes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s.Subscriber = resolveSubscriber(as)
			if len(s.Topics) == 0 && s.To == "" && s.Room == "" {
				return fmt.Errorf("subscribe: nothing to subscribe to — pass --topic, --to, or --room")
			}
			if err := SaveSubscription(s); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "subscribed %s to %s\n",
				s.Subscriber, strings.Join(describeInterest(s), ", "))
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&as, "as", "", "subscriber name (default: your principal)")
	f.StringArrayVar(&s.Topics, "topic", nil, "topic or dotted prefix (repeatable): code.api.*")
	f.StringVar(&s.To, "to", "", "also accept notifications addressed to this id")
	f.StringVar(&s.Room, "room", "", "also accept notifications for this room")
	f.StringVar(&s.Instance, "instance", "", "room member id to steer when an interrupt is authorized")
	f.StringArrayVar(&s.InterruptFrom, "interrupt-from", nil,
		"principal allowed to interrupt a running turn (repeatable; default: nobody)")
	f.IntVar(&s.MaxPerMin, "max-per-min", 0,
		fmt.Sprintf("interrupts per minute before the surplus is demoted to queued (default %d)", DefaultMaxPerMin))
	return cmd
}

func describeInterest(s Subscription) []string {
	var out []string
	for _, t := range s.Topics {
		out = append(out, "#"+t)
	}
	if s.To != "" {
		out = append(out, "→"+s.To)
	}
	if s.Room != "" {
		out = append(out, "@"+s.Room)
	}
	return out
}

func newUnsubscribeCmd() *cobra.Command {
	var as string
	cmd := &cobra.Command{
		Use:   "unsubscribe [flags]",
		Short: "tear down a subscription (an agent that finished is no longer a target)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			who := resolveSubscriber(as)
			if err := RemoveSubscription(who); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "unsubscribed %s\n", who)
			return nil
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "subscriber name (default: your principal)")
	return cmd
}

func newSubscriptionsCmd() *cobra.Command {
	var jsonOut, reconcile bool
	cmd := &cobra.Command{
		Use:     "subscriptions",
		Aliases: []string{"subs"},
		Short:   "list standing subscriptions",
		Long: "List the standing subscriptions the sidecar holds.\n\n" +
			"--reconcile opens a default INBOX for every agent in the fleet that lacks\n" +
			"one. Addressing was opt-in, so on a real host there were none, and a\n" +
			"notification addressed to an agent matched nothing and reached nobody —\n" +
			"durable and undelivered. An address book whose entries have no address is\n" +
			"not an address book.\n\n" +
			"What it grants is deliberately narrow: the agent's own name as a DM target,\n" +
			"no topic interest, and NO interrupt rights. Auto-subscribe hands out an\n" +
			"inbox, never a doorbell — the power to break into a running turn stays\n" +
			"granted by name. An existing subscription is never overwritten, because an\n" +
			"operator who tuned it has expressed a policy.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			if reconcile {
				if FleetNames == nil {
					return fmt.Errorf("bus: no agent catalog is wired here, so the fleet cannot be reconciled")
				}
				made, err := ReconcileSubscriptions(FleetNames())
				if err != nil {
					return err
				}
				if len(made) == 0 {
					fmt.Fprintln(w, "every agent already has an inbox")
				} else {
					fmt.Fprintf(w, "opened %d inbox(es): %s\n", len(made), strings.Join(made, ", "))
				}
			}
			all, err := Subscriptions()
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"schema_version": SchemaVersion,
					"subscriptions":  all,
				})
			}
			if len(all) == 0 {
				fmt.Fprintln(w, "no subscriptions")
				return nil
			}
			fmt.Fprintf(w, "%-16s %-30s %-14s %s\n", "SUBSCRIBER", "INTEREST", "INSTANCE", "MAY INTERRUPT")
			for _, s := range all {
				who := "nobody"
				if len(s.InterruptFrom) > 0 {
					who = strings.Join(s.InterruptFrom, ",")
				}
				fmt.Fprintf(w, "%-16s %-30s %-14s %s\n",
					s.Subscriber, strings.Join(describeInterest(s), " "), orDash(s.Instance), who)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&reconcile, "reconcile", false, "open a default inbox for every fleet agent that lacks one")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newSidecarCmd() *cobra.Command {
	var poll time.Duration
	var once, jsonOut bool

	cmd := &cobra.Command{
		Use:   "sidecar [flags]",
		Short: "hold subscriptions on agents' behalf and pre-resolve their notifications",
		Long: `sidecar watches the bus continuously so the agents do not have to.

A running agent is always heads-down in a turn, blocked between turns, or idle —
and in none of those states can it reliably decide to go check a channel. Asking
it to remember is the worst option: it forgets, or it is mid-task. So the sidecar
does the watching, matches each notification against the standing subscriptions,
applies the governance and rate rules, and leaves each agent a pre-resolved
buffer to read at a turn boundary ('bashy bus pending').

Authorized, direction-changing notifications additionally break into a running
turn over the same control socket the coach uses. Everything else queues.

  bashy bus sidecar               # run continuously
  bashy bus sidecar --once        # one pass, for a cron or a test`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sc := NewSidecar(poll)
			if once {
				res, err := sc.Once()
				if err != nil {
					return err
				}
				if jsonOut {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(res)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "queued %d, interrupted %d, demoted %d\n",
					res.Queued, res.Interrupts, res.Demoted)
				return nil
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			fmt.Fprintf(cmd.ErrOrStderr(), "bus sidecar: watching (poll %s)\n", sc.Poll)
			return sc.Run(ctx)
		},
	}
	f := cmd.Flags()
	f.DurationVar(&poll, "poll", defaultPoll, "how often to re-read the timeline")
	f.BoolVar(&once, "once", false, "run a single pass and exit")
	f.BoolVar(&jsonOut, "json", false, "emit the pass result as JSON (--once)")
	return cmd
}

func newPendingCmd() *cobra.Command {
	var as string
	var jsonOut, peek, all bool

	cmd := &cobra.Command{
		Use:   "pending [flags]",
		Short: "read the notifications the sidecar has already resolved for you",
		Long: `pending prints an agent's pre-resolved notifications and clears them.

This is the turn-boundary inject point. The agent does not evaluate
subscriptions, match topics, check principals or apply rate limits — that is
resolved for it, and what arrives here is a short list of things already
determined to be its business.

It resolves on read, so it works with NO sidecar running: a message addressed to
this agent is delivered the next time it looks. A running sidecar only adds
interrupts, which are the one delivery that cannot wait for the agent to ask.

Use --peek to read without clearing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			who := resolveSubscriber(as)
			// Resolve before reading. The sidecar pre-resolves off the critical
			// path when one is running, and on most hosts none is — so without
			// this, a message addressed to this agent sits in the timeline
			// forever and `pending` truthfully reports an empty buffer while the
			// message exists. Queueing needs no control socket; only an interrupt
			// does. A resolution failure is not fatal: whatever the sidecar
			// already buffered is still worth printing.
			// Open this reader's inbox if it has none, so anyone can join the
			// board by reading it — no subscribe step, no catalog entry. It
			// opens at the head, so a first-time reader sees what arrives from
			// now on rather than the whole history.
			// A ROLE opens at 0, an agent at the head — see IsRoleName. Reading a
			// role's inbox must not skip the pings already published to it.
			if IsRoleName(who) {
				_, _ = EnsureRoleInbox(who)
			} else {
				_, _ = EnsureSubscription(who)
			}
			_, _ = ResolveFor(who)
			var items []Pending
			var err error
			if all {
				items, err = ReadPending(who)
			} else {
				items, err = UnreadPending(who)
			}
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()

			if jsonOut {
				enc := json.NewEncoder(w)
				for _, p := range items {
					if eerr := enc.Encode(p); eerr != nil {
						return eerr
					}
				}
			} else if len(items) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no notifications")
			} else if _, werr := fmt.Fprint(w, FormatPending(items)); werr != nil {
				return werr
			}

			// Clear only AFTER the buffer has been written out, and only up to what
			// was actually read: clearing first, or truncating wholesale, would
			// discard anything the sidecar appended in between — a notification the
			// agent never learns existed.
			if peek || all || len(items) == 0 {
				return nil
			}
			// MARK, never delete. The message stays in the buffer with a
			// read_at stamp, so `--all` can still answer "what was I told, and
			// when" long after the fact.
			return MarkRead(who, items[len(items)-1].Seq)
		},
	}
	f := cmd.Flags()
	f.StringVar(&as, "as", "", "subscriber name (default: your principal)")
	f.BoolVar(&jsonOut, "json", false, "emit one JSON object per line")
	f.BoolVar(&peek, "peek", false, "read without marking anything read")
	f.BoolVar(&all, "all", false, "show every message ever received, read or not (history)")
	return cmd
}
