package bus

// `bashy ping` — one front door to the board, and to the classic command.
//
//	bashy ping                      read the board
//	bashy ping <target> "message"   send  (a message means it is mail)
//	bashy ping --to <target> "message"
//	bashy ping <host>               ICMP  (no message means the classic command)
//
// ARITY DISAMBIGUATES, which is why this can share a name with a 40-year-old
// command without lying about either. A message operand makes it mail; its
// absence makes it the thing `ping` has always meant. So `bashy ping dragon` is
// unambiguously ICMP and `bashy ping steward "..."` is unambiguously a post,
// with no precedence table to get wrong and no guessing about identity — the
// defect that produced three separate bugs the day this was designed.
//
// The one genuinely ambiguous case is a bare target that names a role
// (`bashy ping steward`), and there the answer is a HINT, not a guess.
//
// # Why this does not shadow /sbin/ping
//
// `bashy ping` is front-door argv dispatch; bare `ping` inside a bashy shell
// goes through the ExecHandler, finds no registered tool, and resolves on PATH
// exactly as before. They collide only if `ping` is given a bare-name shim in
// the shell preamble — so it MUST NOT BE, joining the never-shimmed class with
// `time` and `kill`. A host target is handed to the real binary, so nothing a
// script does today changes.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// NewPingCmd is the front door. It owns no store and no identity path of its
// own: every send is a board post and every read is the board.
func NewPingCmd() *cobra.Command {
	var as, to string
	cmd := &cobra.Command{
		Use:   "ping [<target> [<message>...]]",
		Short: "read the board, message someone on it, or ICMP a host",
		Long: `ping is the front door to this host's message board, and to the classic command.

  bashy ping                          what is new for you (same as ` + "`bashy mb`" + `)
  bashy ping steward "..."            message whoever holds the steward seat
  bashy ping --to steward "..."       the same send, with an explicit addressee
  bashy ping conductor:22 "..."       message the conductor of sprint 22
  bashy ping codex-gpt5.6-sol "..."   message one agent
  bashy ping example.com              ICMP a host — the classic command, unchanged

A MESSAGE makes it mail; no message makes it ICMP. That is the whole rule, and
it is why this name is safe to share: ` + "`bashy ping dragon`" + ` still pings a host.

Addressing a ROLE reaches the seat rather than whoever holds it, so the message
survives a handover. Addressing an AGENT reaches that agent.

This is a front door, not a second system: everything here is ` + "`bashy mb`" + `, which
has more than this exposes — receipts, claims, and selectors like --band and
--tool. A messaging body is limited to 1024 UTF-8 bytes and is never truncated
or auto-split; prefer a stable reference or manually numbered correlated parts.
ICMP arguments are unchanged. Run ` + "`bashy mb --help`" + ` for details.`,
		SilenceUsage: true,
		// FLAGS BELONG TO THE SYSTEM PING, so this command parses none of its
		// own beyond --as, --to and --help.
		//
		// Letting cobra parse would make `bashy ping -c 1 localhost` fail with
		// "unknown shorthand flag: 'c'" — breaking the one promise this name
		// makes, that the classic command still works. ping's flag surface is
		// large, platform-specific and not ours to mirror; a whitelist would
		// rot the first time a platform added one.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			args, as, to, help := splitPingArgs(args)
			if help {
				return cmd.Help()
			}
			if to != "" {
				if len(args) == 0 {
					return fmt.Errorf("ping: --to requires a message")
				}
				return pingSend(cmd, as, to, strings.Join(args, " "))
			}
			switch {
			case len(args) == 0:
				return runBoardRead(cmd, as, DefaultBoardLimit, false, false)
			case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
				return pingBareTarget(cmd, args[0])
			case strings.HasPrefix(args[0], "-"):
				// A leading flag can only be for the system ping — mail is
				// addressed to a target, never to an option.
				return icmp(cmd, args...)
			default:
				return pingSend(cmd, as, args[0], strings.Join(args[1:], " "))
			}
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "sender/reader identity (default: resolved from your principal)")
	cmd.Flags().StringVar(&to, "to", "", "addressee for a message send")
	return cmd
}

// splitPingArgs pulls out the options this command owns and leaves
// everything else exactly as typed, for either the board or the system ping.
func splitPingArgs(in []string) (rest []string, as, to string, help bool) {
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] == "--as" && i+1 < len(in):
			as = in[i+1]
			i++
		case strings.HasPrefix(in[i], "--as="):
			as = strings.TrimPrefix(in[i], "--as=")
		case in[i] == "--to" && i+1 < len(in):
			to = in[i+1]
			i++
		case strings.HasPrefix(in[i], "--to="):
			to = strings.TrimPrefix(in[i], "--to=")
		case in[i] == "-h" || in[i] == "--help":
			help = true
		default:
			rest = append(rest, in[i])
		}
	}
	return rest, as, to, help
}

// pingBareTarget handles `bashy ping <target>` with no message.
//
// A role here is the one case arity cannot settle, and it gets a hint rather
// than a guess: silently posting an empty message would be worse, and silently
// ICMP-ing a name that is plainly a seat wastes the user's next minute.
func pingBareTarget(cmd *cobra.Command, target string) error {
	if _, ok := ResolveRole(target); ok {
		return fmt.Errorf("%q is a role on this host, not a hostname\n"+
			"  message it:   bashy ping %s \"...\"\n"+
			"  ICMP a host:  ping %s", target, target, target)
	}
	return icmp(cmd, target)
}

// pingSend posts to the board. The target is resolved AT SEND TIME through
// the one resolution ladder ResolveSendTarget owns: a role to its seat's
// stable address, an agent to its roster name, an existing reader to itself,
// and past those the host resolver — the same authority `whois` answers from
// — so a person or observed seat whois knows is sendable here too. A target
// matching nothing anywhere fails with choices, writing nothing: a
// confirmation that named a recipient the board has never heard of was
// indistinguishable from a real delivery, which is the defect this closes.
func pingSend(cmd *cobra.Command, as, target, body string) error {
	if err := ValidateCoordinationBody(body); err != nil {
		return fmt.Errorf("ping message: %w", err)
	}
	from, err := ResolveAuthoredActor(as)
	if err != nil {
		return err
	}
	addr, kind, ok := ResolveSendTarget(target)
	if !ok {
		return unresolvedTargetError(target)
	}
	// Board FIRST, steer second — the durable copy must not be the optional one.
	seq, err := PostMessageSeq(Post{From: from, To: addr, Topic: "mb", Body: body})
	if err != nil {
		return err
	}
	d := SteerLive(addr, steerNotice(from, body))
	d.State = deliveryState(addr, seq, d.Steered, kind != TargetRole)
	d.To = RoleLabelFor(d.To)
	reportDelivery(cmd, []Delivery{d})
	return nil
}

// icmp hands a host target to the real ping.
//
// EXEC, never reimplement: ping needs raw sockets or a setuid binary, and the
// system one is already installed, already permitted and already correct on
// every platform. Reimplementing it would mean shipping a worse copy of a
// command that ships with the OS — precisely the case coreutils' NO-list files
// under "system administration, out of scope".
func icmp(cmd *cobra.Command, args ...string) error {
	bin, err := exec.LookPath("ping")
	if err != nil {
		return fmt.Errorf("no system ping on PATH: %w", err)
	}
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
	return c.Run()
}
