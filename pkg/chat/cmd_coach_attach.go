package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/room"
)

// newCoachAttachCmd is the `coach attach` subcommand: point a coach at a session
// that is ALREADY RUNNING, instead of only at one the coach launched itself.
//
// It resolves the target through the SAME resolver `chat steer` uses (findMember
// → room.Find: full id or unambiguous prefix/nick), so an operator does not learn
// a second resolution rule. Then it runs the shared attachCoachAs, which tails the
// member's output (pty mode) or structured events (event mode) and steers through
// the member's control socket using the ONE trip implementation a launched coach
// uses.
//
// It defaults to --as supervisor: that is what `coach attach` has always meant,
// it is documented, and it is in use. The role ladder is available here too, but
// the default is the shipped behavior — a flag that appears must not change what
// existing invocations do.
//
// `--list` prints the attachable members (those with a control socket) so an
// operator can discover the id before attaching.
func newCoachAttachCmd() *cobra.Command {
	return newAttachCmd("attach <session>", RoleSupervisor,
		"coach a session that is ALREADY RUNNING (attach; never launches or kills it)",
		"coach attach")
}

// NewAttachCmd is the host-agnostic `attach` verb — `bashy attach <session> --as
// {observer|advisor|supervisor}` — the general form of what `coach attach`
// pioneered. Same machinery, same refusals, same single tail; the role is the
// only difference, and it is an enforced effect cap rather than a promise.
//
// It defaults to --as observer, the LEAST powerful role. `coach attach` keeps its
// supervisor default because that is what it already meant; a new front door has
// no such history, and the safe default for "let me join this session" is to
// watch. Choosing a power is one flag away; acquiring one by accident should not
// be possible at all.
func NewAttachCmd() *cobra.Command {
	return newAttachCmd("attach <session>", RoleObserver,
		"join a session that is ALREADY RUNNING as observer, advisor, or supervisor",
		"attach")
}

// newAttachCmd builds the shared attach command. defaultRole is what --as means
// when the operator does not say — see the two callers for why they differ. name
// is the invocation an error message should quote back.
func newAttachCmd(use string, defaultRole AttachRole, short, name string) *cobra.Command {
	var (
		list      bool
		repeat    int
		ratio     float64
		maxSteers int
		readOnly  bool
		as        string
		logPath   string
		timeout   time.Duration
	)
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: "Attach joins a session already in flight — the member is resolved from the host " +
			"room, watched through its capture (pty mode: the generic loop-from-output signal, " +
			"works for any tool) or its structured event channel (event mode: precise tool.call " +
			"tracking, when the member reports one), and — for a role that permits it — steered " +
			"through its control socket. Detach (--timeout or Ctrl-C) leaves the coachee running.\n\n" +
			"--as picks the ROLE, and the role is an enforced cap on what the attachment can do:\n" +
			"  observer     watch and report; emits nothing at all\n" +
			"  advisor      may also say a sentence (read at the coachee's next turn boundary)\n" +
			"  supervisor   may also press ESC, the only thing that breaks into a running turn\n\n" +
			"Never an author: no role writes files, commits, or merges. When the LLM-free reflex " +
			"policy trips under a capped role the trip is still detected and reported — the effect " +
			"is demoted to what the role allows, never silently dropped.\n\n" +
			"Resolve the target the same way `chat steer` does: the full instance id or an " +
			"unambiguous prefix. `--list` shows the attachable members.",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			if list {
				return listAttachable(cmd)
			}
			role, err := resolveAttachRole(cmd, as, readOnly, defaultRole)
			if err != nil {
				return err
			}
			if len(args) < 1 {
				return fmt.Errorf("%s: name a session id — `%s --list` shows attachable members", name, name)
			}
			card, err := findMember(args[0])
			if err != nil {
				return err
			}
			pol := DefaultCoachPolicy()
			if repeat > 0 {
				pol.RepeatThreshold = repeat
			}
			if ratio > 0 {
				pol.RatioThreshold = ratio
			}
			if maxSteers > 0 {
				pol.MaxSteers = maxSteers
			}
			pol.LogPath = logPath

			// --read-only is the pre-roles spelling of observer, and it resolved to
			// RoleObserver above. Dropping the ESC at the policy level too keeps the
			// old invocation byte-for-byte what it was; the role cap is what actually
			// guarantees nothing crosses the socket.
			if readOnly {
				pol.Interrupt = false
			}

			base := cmd.Context()
			var ctx context.Context
			var cancel context.CancelFunc
			if timeout > 0 {
				ctx, cancel = context.WithTimeout(base, timeout)
			} else {
				ctx, cancel = context.WithCancel(base)
			}
			defer cancel()

			errOut := cmd.ErrOrStderr()
			fmt.Fprintf(errOut, "attach: joined %s (%s) as %s — Ctrl-C / --timeout detaches; the agent keeps running\n", card.ID, card.Binding, role)

			coach, steer, err := attachCoachAs(ctx, card, pol, role)
			if err != nil {
				return err
			}

			// Block until the watch ends. Waiting on the coach (not ctx.Done) means a
			// coachee that exits on its own — pid gone — ends the watch too, so an
			// attach with no --timeout returns when the agent does instead of hanging.
			// The watcher exits on --timeout, Ctrl-C (ctx cancelled), or the coachee's
			// pid going away; the coachee is never killed by this side either way.
			coach.Wait()
			rep := coach.Report()

			fmt.Fprintf(errOut, "\n── attach report (%s, %s, role %s) ──\n", card.ID, coach.Mode(), role)
			if coach.Mode() == "events" {
				fmt.Fprintf(errOut, "tool calls: %d total / %d distinct (repeat %.2f)\n", rep.Total, rep.Distinct, rep.Repeat)
			} else {
				fmt.Fprintf(errOut, "output lines: %d total / %d distinct (repeat %.2f)\n", rep.Total, rep.Distinct, rep.Repeat)
			}
			fmt.Fprintf(errOut, "steers: %d\n", len(rep.Steers))
			for i, s := range rep.Steers {
				fmt.Fprintf(errOut, "  %d. [%s] at repeat=%.2f: %q\n", i+1, s.Reason, s.Repeat, s.Steer)
			}
			// Report what the role prevented. A demoted intervention is a fact about
			// the session: the loop was detected and the attachment was not permitted
			// to act on it, which is the operator's cue to re-attach one rung up.
			if n := steer.Demotions(); n > 0 {
				fmt.Fprintf(errOut, "demoted by role %s: %d (detected and reported, not sent — re-attach with a higher --as to act)\n", role, n)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&as, "as", string(defaultRole),
		"participation role: "+strings.Join(AttachRoleNames(), " | ")+" (least to most power)")
	f.BoolVar(&list, "list", false, "list attachable sessions (room members with a control socket) and exit")
	f.IntVar(&repeat, "repeat", 0, "trip when one identical call/line is issued this many times (default 3)")
	f.Float64Var(&ratio, "ratio", 0, "trip when total/distinct reach this (default 3.0)")
	f.IntVar(&maxSteers, "max-steers", 0, "hard cap on interventions (default 3)")
	f.BoolVar(&readOnly, "read-only", false, "detect-only: watch and report, send nothing to the coachee (same as --as observer)")
	f.StringVar(&logPath, "log", "", "append one JSON line per steer here (the training record)")
	f.DurationVar(&timeout, "timeout", 0, "bound the watch; detaches when it elapses (the agent keeps running)")
	return cmd
}

// resolveAttachRole turns the flags into the one role the attachment runs under.
//
// --read-only predates --as and means observer. When both are given they must
// agree: silently capping an explicit `--as supervisor` down would hide a
// contradiction the operator wrote on purpose, and silently honouring it would
// defeat --read-only's whole promise. Say which two flags disagree instead.
func resolveAttachRole(cmd *cobra.Command, as string, readOnly bool, defaultRole AttachRole) (AttachRole, error) {
	asSet := cmd.Flags().Changed("as")
	if !asSet {
		if readOnly {
			return RoleObserver, nil
		}
		return defaultRole, nil
	}
	role, err := ParseAttachRole(as)
	if err != nil {
		return "", err
	}
	if readOnly && role != RoleObserver {
		return "", fmt.Errorf("attach: --read-only and --as %s contradict each other — --read-only is --as observer; drop one", role)
	}
	return role, nil
}

// listAttachable prints the room members a coach can attach to — the live,
// steerable ones (a non-empty control socket is what makes a member attachable:
// it is the only surface a coach steers through).
func listAttachable(cmd *cobra.Command) error {
	members, err := room.Members()
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	var attachable []room.Card
	for _, c := range members {
		if c.CtlSock != "" {
			attachable = append(attachable, c)
		}
	}
	if len(attachable) == 0 {
		fmt.Fprintln(w, "no attachable sessions — a member needs a control socket to be coached")
		return nil
	}
	fmt.Fprintf(w, "%-24s %-22s %-8s %-7s %s\n", "ID", "BINDING", "EVENTS", "PID", "TASK")
	for _, c := range attachable {
		ev := "pty"
		if c.Events {
			ev = "events"
		}
		fmt.Fprintf(w, "%-24s %-22s %-8s %-7d %s\n", c.ID, c.Binding, ev, c.PID, c.Task)
	}
	return nil
}
