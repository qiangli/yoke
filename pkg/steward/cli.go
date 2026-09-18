// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/role"
)

// opts is the state every subcommand shares: which store, and whether the caller
// wants prose or JSON.
type opts struct {
	dir    string
	asJSON bool

	// base is what the EMBEDDER passed to NewStewardCmd — the store options this command
	// tree was mounted with. It is deliberately NOT reachable from a flag: it carries the
	// roots of trust (a real Verifier, a VerificationVerifier) and the canonical registry
	// root, and every one of those would be worthless if the agent running the command
	// could redirect it from the command line.
	base []Option
}

// store opens the store for everything that is NOT an authority transition: status,
// board, log, record, verify, reconcile, repair. It injects no verifier — and therefore,
// by construction, cannot claim or take over the seat. That is not an oversight; it is
// the fail-closed default doing its job, and it means a bug in a read path cannot become
// an authority bug.
func (o *opts) store() (*Store, error) { return Open(o.dir, o.base...) }

// authStore opens the store for an AUTHORITY TRANSITION — authorize, claim, takeover —
// with the root of trust the CLI can actually offer: a typed confirmation at a real
// terminal.
//
// BE CLEAR ABOUT WHAT THAT IS WORTH. It is AUDIT-grade, never security-grade (see
// GradeAudit). The process cannot distinguish a human's keystrokes from a pty an agent
// allocated and wrote into — both are terminals, both produce the same bytes. What it
// delivers is that the transition is deliberate, attended, and permanently recorded; what
// it does NOT deliver is proof that a human was in the room.
//
// The consequence is enforced rather than merely documented: an audit-grade attestation
// authorizes only an ATTENDED transition. With no terminal — a cron job, a CI runner, a
// headless agent loop — this verifier refuses, and there is nothing else to fall back on,
// so the unattended path fails closed until a host injects a real verifier
// (steward.WithVerifier). That is the integration hook for bashy meet or a host approval
// UI, and nothing here has to change to take it: the enforcement point already exists.
// The pty verifier is appended AFTER the embedder's options, so a host that injected a real
// Verifier keeps it: later options win, and the audit-grade terminal is the FLOOR, not a
// downgrade applied on top of something better.
func (o *opts) authStore(cmd *cobra.Command) (*Store, error) {
	base := append([]Option{WithVerifier(&ptyVerifier{in: cmd.InOrStdin(), out: cmd.ErrOrStderr()})}, o.base...)
	return Open(o.dir, base...)
}

// NewStewardCmd builds `bashy steward`: the host's single seat of authority and its
// permanent record.
//
// Mounted by the host (bashy) rather than exported as a userland tool, because a
// steward is not a utility — it is the thing the human talks TO about everything the
// agents on this machine did.
//
// The options are the HOST's, not the user's: the roots of trust (WithVerifier,
// WithVerificationVerifier) and the canonical registry root (WithRegistryRoot). They are
// passed in code, at mount time, precisely because none of them would be worth anything if
// the agent typing the command could point them somewhere else. With none of them, the CLI
// is what it has always been: it can read everything, it can journal, it can refute — and
// it can neither hand out the seat unattended nor promote a claim to verified.
func NewStewardCmd(base ...Option) *cobra.Command {
	o := &opts{base: base}
	cmd := &cobra.Command{
		Use:   "steward",
		Short: "the host's one seat of authority, and the journal that outlives whoever holds it",
		Long: `steward is the ONE agent per host/user that answers for what happened here.

Not one per repo, not one per terminal — one per machine-and-account, held under a
heartbeat lease, and recoverable by the next agent WITHOUT the last one's
cooperation. That last part is the whole design: a steward that crashed, was
rate-limited, or simply vanished leaves no goodbye note, and continuity must survive
it anyway.

  THE JOURNAL IS THE ONLY TRUTH.
  Board, status, log, conversation, history and checkpoints are read-only PROJECTIONS
  of it. None of them is a second place where state lives, so none of them can drift
  from it or quietly become the real one.

  A REFERENCE IS NOT A VERIFICATION.
  A claim of success with nothing to point at projects as UNKNOWN. A claim with
  references nobody checked projects as ASSERTED — never as verified. Only a
  verification record (` + "`steward verify`" + `), where somebody went and looked, promotes a
  claim to verified. An agent can attach a plausible command string to work it never
  did exactly as easily as to work it did.

  EVERY WRITE PRESENTS A FENCING EPOCH.
  There is no "use whatever is current". A steward that lapsed and was taken over
  comes back holding a stale token and is REJECTED, loudly, instead of interleaving
  its writes with its successor's.

  ACQUIRING THE SEAT IS AUTHORIZED, AND THE AUTHORIZATION IS DURABLE.
  ` + "`steward claim`" + ` takes a VACANT or LAPSED seat. Anything else — a live seat, or one
  whose liveness record cannot be trusted — is a TAKEOVER. BOTH are acquisitions of
  authority and BOTH spend a capability minted by ` + "`steward authorize`" + `: single-use,
  expiring, bound to one epoch and one agent, and recorded in the journal forever.
  There is no unauthorized way onto this seat — a claim without a grant is refused.

steward is NOT handoff. ` + "`bashy handoff`" + ` moves WORK — a diff, a working tree, a task.
steward moves a MANDATE. Claiming the seat touches no repository, restores no working
tree, and captures no diff.`,
		Example: `  bashy steward status                     # who holds the seat, and is the record sound?

  # Taking the seat is an ACQUISITION, so it is authorized: mint the single-use grant,
  # then spend it. Both halves are ATTENDED — each asks you to type the epoch back.
  grant=$(bashy steward authorize --action claim --actor "$USER" --reason "on call" --json | jq -r .id)
  eval "$(bashy steward claim --grant "$grant" --intent 'on call' --export)"   # take the seat, export the epoch

  bashy steward board                      # the Kanban: lanes, priorities, blockers, next actions
  bashy steward record --workstream api -m "migrated the schema" \
        --outcome success -e "command:go test ./..." -e "commit:de6485c"
  bashy steward verify --seq 7 --result success --method "re-ran the suite on a clean checkout"
  bashy steward log --degraded             # what do we NOT actually know?
  bashy steward reconcile                  # the verb a successor runs FIRST
  bashy steward authorize --actor qiangli --reason "incumbent wedged"
  bashy steward takeover --grant g-…`,
		SilenceUsage: true,
	}
	// See the note in pkg/board's newRoleCommand: this tree is mounted under
	// `bashy`, so cobra's generated `completion` verb documents a binary that
	// does not exist.
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.PersistentFlags().StringVar(&o.dir, "dir", "",
		"steward store (default $BASHY_STEWARD_DIR, else ~/.bashy/steward/<host>-<user>-<id>)")
	cmd.PersistentFlags().BoolVar(&o.asJSON, "json", false, "emit machine-readable JSON")

	cmd.AddCommand(
		newStatusCmd(o),
		newScopeCmd(o),
		newBoardCmd(o),
		newLogCmd(o),
		newConversationCmd(o),
		newHistoryCmd(o),
		newCheckpointCmd(o),
		newReconcileCmd(o),
		newRepairCmd(o),
		newClaimCmd(o),
		newAuthorizeCmd(o),
		newGrantsCmd(o),
		newTakeoverCmd(o),
		newReleaseCmd(o),
		newHeartbeatCmd(o),
		newRecordCmd(o),
		newDecideCmd(o),
		newVerifyCmd(o),
		newPingCmd(o),
		newInboxCmd(o),
		newTranscriptCmd(o),
		newWorkstreamCmd(o),
	)
	return cmd
}

// SeatSummary prints who holds this host and what to do about it.
//
// It is exported because the front door for the steward ROLE is `bashy
// steward`, which renders the role's operating skill — and the first question
// an arriving agent has is not "how do I steward" but "is anybody already
// stewarding". Printing the manual without the seat answers the second question
// while burying the first, so the summary goes above it.
//
// AUTHORITY and LIVENESS are stated separately, because a held-but-dead seat is
// not the same situation as a held one and the action differs: the first is a
// takeover, the second is a conversation.
func SeatSummary(w io.Writer, dir string, base ...Option) error {
	st, err := Open(dir, base...)
	if err != nil {
		return err
	}
	view, err := st.Status(time.Now())
	if err != nil {
		return err
	}
	// THE SEAT IS PER (HOST, ACCOUNT), NOT PER HOST — say so.
	//
	// The store key is <host>-<account>-<digest>, so two login users on one
	// machine hold two independent seats. Printing only the hostname implies a
	// machine-wide seat, which would let a second user read "no steward" and
	// believe the machine is unattended while somebody else is stewarding their
	// own account. The distinction only matters on shared machines, which is
	// exactly where getting it wrong costs the most.
	seat := seatLabel()
	if view.Authority.Vacant {
		fmt.Fprintf(w, "%s has NO steward — the seat is open.\n", seat)
		// The real flow is TWO steps, and saying otherwise sends someone into a
		// refusal. Taking the seat needs a capability first, deliberately:
		// an agent that could decide to become the steward would eventually
		// decide to do it to a healthy one.
		fmt.Fprintf(w, "  claim it:  bashy steward authorize --action claim --actor <who> --reason <why>\n")
		fmt.Fprintf(w, "             bashy steward claim --grant <id> --intent \"<what you are here to do>\"\n\n")
		return nil
	}
	fmt.Fprintf(w, "%s steward: %s (epoch %d)\n", seat, view.Authority.Holder, view.Authority.Epoch)
	switch view.Liveness {
	case LivenessLive:
		fmt.Fprintf(w, "  alive — reach them on bus %s before taking anything over\n\n",
			stewardAssignment().Topic())
	case LivenessVacant:
		fmt.Fprintf(w, "  the journal says vacant — bashy steward claim\n\n")
	default:
		// Held on paper, nobody breathing. The actionable case, and the one
		// worth surfacing first.
		fmt.Fprintf(w, "  NOT ALIVE (%s) — held on paper only\n", view.Liveness)
		fmt.Fprintf(w, "  take it:   bashy steward authorize --action takeover --actor <who> --reason <why>\n")
		fmt.Fprintf(w, "             bashy steward takeover --grant <id> --intent \"<what you are here to do>\"\n\n")
	}
	return nil
}

// runSeatSummary prints who holds the host, and how to take it if nobody does.
//
// It states AUTHORITY and LIVENESS separately, the same way `status` does,
// because a held-but-dead seat is not the same situation as a held one and the
// action differs: the first is a takeover, the second is a conversation.

// epochFlag wires the fencing token onto a command. Every authoritative mutation has
// one, and every one of them REFUSES to proceed without a value — from the flag, or
// from $BASHY_STEWARD_EPOCH exported at claim time.
func epochFlag(c *cobra.Command, into *uint64) {
	c.Flags().Uint64Var(into, "epoch", 0,
		"the fencing epoch you hold (default $"+EpochEnv+", exported by `steward claim --export`). "+
			"The write is REJECTED if the seat has moved on")
}

// interactive reports whether a terminal is attached to stdin.
//
// It is used for one thing only: minting an operator-assertion authorization, and
// refusing an unattended takeover that leans on one. It is an OBSERVATION about the
// process, not a security boundary — a caller that wants to lie about it can, and the
// journal records the assertion as an assertion precisely because of that. See
// Provenance.
// It asks the OS whether the fd is a TERMINAL, and the distinction from the usual
// os.ModeCharDevice shortcut is not pedantry — that shortcut is TRUE FOR /dev/null,
// which is precisely how a daemon, a cron job, a CI runner, and a headless agent get
// their stdin. Under it, the most unattended process on the machine would be judged
// attended, and could spend an operator ASSERTION on an unattended takeover: the exact
// case the external-receipt rule exists to stop. The weaker check does not merely
// mislabel the situation, it hands back the control.
func interactive(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// ─── seat lifecycle ───────────────────────────────────────────────────────────

func newScopeCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "scope",
		Short: "print this seat's identity: which machine, which OS account",
		Long: `scope prints the identity of THIS seat — one stable machine, one stable OS account.

The store is keyed by it, and NEITHER HALF COMES FROM THE ENVIRONMENT.

  machine  a stable machine id from the OS: /etc/machine-id (linux), kern.uuid (darwin),
           the MachineGuid registry value (windows) — or $BASHY_HOST_ID where the OS has
           none. NOT the hostname, which is a label a machine wears, not a machine: two
           laptops both called "localhost" sharing a synced home would otherwise merge
           their journals, their epoch ladders, and their stewards.
  account  the OS account this process actually runs as: the numeric UID, or the account
           SID on windows. NOT $USER/$LOGNAME/$USERNAME, which are strings a process
           inherits and can overwrite — so ` + "`USER=someone-else bashy steward claim`" + ` used to
           be a DIFFERENT SEAT, and an agent could sidestep the singleton by exporting a
           variable instead of taking anything.

The store carries a BINDING to this identity and refuses to be opened from another
machine or account, because a store directory is a path, and a path can be pointed at
deliberately, carried by a synced home, or restored from a backup onto the wrong box.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			sc := s.ScopeInfo()
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), struct {
					Scope    string `json:"scope"`
					Dir      string `json:"dir"`
					Registry string `json:"registry"`
					Digest   string `json:"digest"`
					Source   string `json:"machine_id_source"`
					Host     string `json:"host_label"`
				}{sc.ID, s.Dir(), s.RegistryPath(), sc.Digest(), sc.Source, sc.Host})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, sc.ID)
			fmt.Fprintf(out, "store:      %s\n", s.Dir())
			// Where the seat's ONE store is recorded. Printed because it is no longer guessable:
			// the root comes from the OS account's home, not from $HOME, so an operator who has to
			// go and look at the binding (or deliberately remove it after a real move) cannot
			// derive the path from anything they can echo.
			fmt.Fprintf(out, "registry:   %s\n", s.RegistryPath())
			fmt.Fprintf(out, "machine id: %s (a LABEL, not identity: %s)\n", sc.Source, sc.Host)
			fmt.Fprintf(out, "binding:    %s\n", short8(sc.Digest()))
			return nil
		},
	}
}

func newClaimCmd(o *opts) *cobra.Command {
	var intent, grant, grantFile string
	var export bool
	cmd := &cobra.Command{
		Use:     "claim",
		Aliases: []string{"take"},
		Short:   "acquire a VACANT or LAPSED seat, under an authorization (atomically; never asks the incumbent)",
		Long: `claim takes the seat in exactly two situations, and no others:

  VACANT  nobody holds it.
  LAPSED  a heartbeat record that AGREES with the journal — right holder, right epoch,
          sane timestamps — says the holder is past the TTL.

Everything else is refused. A LIVE seat is held — including one you hold yourself (see
below). A seat whose liveness record is MISSING, unreadable, wrong-schema, wrong-holder,
wrong-epoch, or dated into the future is NOT claimable: that is a fact about the RECORD,
not about the holder, every way of producing it is also a way of producing it
deliberately, and deleting one file must not be enough to take a healthy steward's seat.
Recovering from either is a takeover.

IT IS AUTHORIZED, like a takeover. Claiming used to be free, on the theory that an empty
chair belongs to whoever sits in it. But a LAPSED seat is not an empty chair — it has an
incumbent, and "lapsed" proves a heartbeat gap and nothing more: they may be mid-thought,
rate-limited, or paused at a prompt, and the claim FENCES them. An unattended agent that
could claim a lapsed seat could simply wait out the TTL and depose a working steward —
the takeover it was forbidden to perform, spelled differently. And a vacant seat is still
the seat of authority for the whole machine; "whoever gets there first" is a race, not a
policy. So a claim SPENDS a capability, exactly as a takeover does — mint it first, and
pass its id:

  steward authorize --action claim --actor <who> --reason <why>   # prints the grant id
  steward claim --grant <id> --intent <what for>

Both halves are attended: each asks you to type the current epoch back at the terminal.
The grant is single-use, so the id above buys exactly one claim.

RENEWING IS NOT CLAIMING. Re-claiming a seat you already hold used to be quietly treated
as a heartbeat. It isn't one any more, because it was a way to refresh a held tenure
WITHOUT presenting the epoch — and the epoch is the only thing that can tell a steward
its tenure ended while it was away. Renew with ` + "`steward heartbeat --epoch N`" + `, which
presents it.

Claiming a lapsed seat BUMPS THE FENCING EPOCH, so the prior holder is fenced rather than
buried: their next write is rejected loudly instead of interleaving with yours.

Claiming captures NO repository state. It is a mandate, not a checkout.`,
		Example: `  # 1. mint the single-use grant. It prints its id (and --json puts it on stdout alone).
  bashy steward authorize --action claim --actor "$USER" --reason "taking the seat for the day"

  # 2. spend it on the seat.
  bashy steward claim --grant g-1f4c9a2b --intent "on call"

  # Or both, capturing the id — and exporting the fencing epoch every later write presents:
  grant=$(bashy steward authorize --action claim --actor "$USER" --reason "on call" --json | jq -r .id)
  eval "$(bashy steward claim --grant "$grant" --intent 'on call' --export)"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.authStore(cmd)
			if err != nil {
				return err
			}
			v, err := s.Claim(cmd.Context(), Self(), SeatRequest{
				GrantID:   grant,
				GrantPath: grantFile,
				Attended:  interactive(cmd.InOrStdin()),
				Intent:    intent,
			}, time.Now())
			if err != nil {
				return err
			}
			// The seat is held; open the room that makes its holder reachable.
			// Failure is reported, never fatal — a host with an accountable
			// steward and no intercom is strictly better than one with neither.
			if line := assumeSeatRoom(seatHolderName()); line != "" && !o.asJSON {
				defer fmt.Fprint(cmd.OutOrStdout(), line)
			}
			if err := ExportEpoch(v.Authority.Epoch); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, v)
			}
			if export {
				// The one line a shell can eval. Everything else goes to stderr so it does
				// not end up inside the eval.
				fmt.Fprintf(out, "export %s=%d\n", EpochEnv, v.Authority.Epoch)
				out = cmd.ErrOrStderr()
			}
			fmt.Fprintf(out, "claimed the steward seat: %s at epoch %d\n",
				holderName(v.Authority.Holder), v.Authority.Epoch)
			if v.Authority.TakenOverFrom != nil {
				fmt.Fprintf(out, "  the lapsed seat was held by %s — they are now FENCED at the old epoch\n",
					holderName(*v.Authority.TakenOverFrom))
			}
			fmt.Fprintf(out, "  store: %s\n", s.Dir())
			if !export {
				// NOT "re-run claim with --export": the grant that bought this seat is spent, and
				// a seat you already hold is not claimable anyway. --export is a flag on the claim
				// you are making, not a second, grantless way back to the epoch.
				fmt.Fprintf(out, "\nEvery write presents this epoch. Export it to your children:\n"+
					"  export %s=%d          (--export prints exactly that line, for eval)\n", EpochEnv, v.Authority.Epoch)
			}
			fmt.Fprintln(out, "\nRun `steward reconcile` before acting: it reports what the record can and cannot establish.")
			return nil
		},
	}
	cmd.Flags().StringVar(&intent, "intent", "", "what you hold the seat to do")
	cmd.Flags().StringVar(&grant, "grant", "", "the authorization id to consume (from `steward authorize --action claim`)")
	cmd.Flags().StringVar(&grantFile, "grant-file", "", "read the authorization from a file instead")
	cmd.Flags().BoolVar(&export, "export", false, "print only `export "+EpochEnv+"=N` on stdout, for eval")
	return cmd
}

func newAuthorizeCmd(o *opts) *cobra.Command {
	var (
		action, actor, reason   string
		ttl                     time.Duration
		receipt, issuer, rcptID string
	)
	cmd := &cobra.Command{
		Use:   "authorize",
		Short: "mint a single-use, expiring capability to ACQUIRE the seat (claim or takeover)",
		Long: `authorize mints the capability that ` + "`steward claim`" + ` and ` + "`steward takeover`" + ` consume.

Both are acquisitions of authority and both are authorized. A takeover obviously so. A
CLAIM too, and that is the less obvious half: a LAPSED seat has an incumbent — "lapsed"
proves a heartbeat gap and nothing more, and they may be mid-thought — so an unattended
agent that could claim one could just wait out the TTL and depose a working steward. Same
act, different spelling.

The capability is bound, on purpose, to everything that could otherwise be abused:

  single-use   the seat event that spends it names its nonce IN THE JOURNAL, so replay
               refuses a second use — even if the grant file is restored from a backup.
  one action   a capability minted to CLAIM an empty seat does not authorize SEIZING an
               occupied one.
  expiring     15 minutes by default (--ttl), 24h maximum.
  one agent    it names its grantee. It is not a coupon somebody else can pick up.
  one epoch    it authorizes acting on the seat AS IT STANDS NOW. If the seat moves on,
               the grant is dead: it authorized a situation, not the seat in general.

AND THE BOUNDS ARE NOT THE AUTHORITY. This is the thing to understand about this command.
Every bound above is a check on bytes in a file — and the agent this is meant to restrain
can write that file, because it runs as you. A capability rooted in the store can never be
worth more than the store.

So what actually authorizes an acquisition is a VERIFIER, injected by the host, which the
transition asks — and asks AGAIN when the grant is spent. The CLI's verifier is a typed
confirmation at a real terminal. It is AUDIT-grade: deliberate, attended, permanently
recorded, and NOT proof that a human was in the room, because a pty an agent allocated
produces the same bytes a keyboard does. There is deliberately no --yes to skip it: a flag
that skipped the confirmation would hand every unattended process on the machine exactly
the capability the confirmation exists to withhold.

The consequence is enforced, not merely documented: an UNATTENDED acquisition cannot be
authorized by an audit-grade attestation at all. With no terminal, this command refuses,
and so does the acquisition — until a host wires a verifier that can establish authority
outside this store (bashy meet, an approval service, a signature it can check).

  --receipt <file> --receipt-issuer <src>  attaches an out-of-band approval artifact and
                                           pins its bytes by digest. Note what that is: a
                                           digest is INTEGRITY, never AUTHENTICITY. It
                                           proves the bytes did not change; it says
                                           nothing about who wrote them, and an agent can
                                           write a file and hash it as easily as a human.
                                           It is evidence for a human or a verifier to
                                           weigh — never an authorization on its own.`,
		Example: `  bashy steward authorize --action claim    --actor qiangli --reason "taking the seat for the day"
  bashy steward authorize --action takeover --actor qiangli --reason "incumbent wedged on a rate limit"
  bashy steward authorize --action takeover --actor oncall  --receipt ./approval.json --receipt-issuer github:pr-412`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.authStore(cmd)
			if err != nil {
				return err
			}
			g, err := s.Authorize(cmd.Context(), GrantRequest{
				Action:        action,
				Grantee:       Self(),
				Actor:         actor,
				Reason:        reason,
				TTL:           ttl,
				Attended:      interactive(cmd.InOrStdin()),
				ReceiptPath:   receipt,
				ReceiptIssuer: issuer,
				ReceiptID:     rcptID,
			}, time.Now())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, g)
			}
			fmt.Fprintf(out, "authorization %s minted\n", g.ID)
			fmt.Fprintf(out, "  action:     %s\n", g.Action)
			fmt.Fprintf(out, "  provenance: %s\n", g.Provenance)
			fmt.Fprintf(out, "  actor:      %s  (an assertion, not a credential — see `steward authorize --help`)\n", g.Actor)
			fmt.Fprintf(out, "  grantee:    %s\n", holderName(g.Grantee))
			fmt.Fprintf(out, "  acts on:    epoch %d (and only epoch %d — if the seat moves, this dies)\n", g.FromEpoch, g.FromEpoch)
			fmt.Fprintf(out, "  expires:    %s\n", g.ExpiresAt.Local().Format(time.RFC3339))
			if a := g.Attestation; a != nil {
				fmt.Fprintf(out, "  attested:   %s via %s — %s-grade\n", a.Verifier, a.Channel, a.Grade)
				if a.Grade == GradeAudit {
					fmt.Fprintln(out, "              AUDIT-grade: deliberate and attended, NOT proof a human was present.")
					fmt.Fprintln(out, "              It will NOT authorize an unattended acquisition.")
				}
			}
			if g.Receipt != nil {
				fmt.Fprintf(out, "  receipt:    %s from %s (%s) — bytes pinned, author NOT verified\n",
					g.Receipt.Path, g.Receipt.Issuer, short8(g.Receipt.Digest))
			}
			fmt.Fprintf(out, "\nSpend it once: `steward %s --grant %s`\n", g.Action, g.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&action, "action", ActionTakeover, "what to authorize: claim | takeover")
	cmd.Flags().StringVar(&actor, "actor", "", "the operator asserting this authorization (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why the acquisition is necessary")
	cmd.Flags().DurationVar(&ttl, "ttl", DefaultGrantTTL, "how long the capability stays usable")
	cmd.Flags().StringVar(&receipt, "receipt", "", "an out-of-band approval artifact (bytes pinned by digest; author NOT authenticated)")
	cmd.Flags().StringVar(&issuer, "receipt-issuer", "", "where the receipt came from (required with --receipt)")
	cmd.Flags().StringVar(&rcptID, "receipt-id", "", "the receipt's identifier over there (a PR number, a ticket)")
	return cmd
}

// ptyVerifier is the root of trust the CLI can honestly offer: a human, at a real
// terminal, typing the epoch back.
//
// IT IS AUDIT-GRADE (GradeAudit), AND THE LABEL IS NOT MODESTY — it is the accurate
// description of what a process running as the user can establish about the user. There
// is no signature to check, no second party to ask, and no secret the agent does not also
// have; a pty an agent allocated delivers the same bytes a keyboard does. So this
// attests that the act was DELIBERATE and ATTENDED, which is real and worth having, and
// it attests to nothing about WHO. The typed epoch is what makes "deliberate" mean
// something: it cannot be answered by a reflexive "y", and it forces the person to look
// at who currently holds the seat before taking it from them.
//
// The two refusals below are the enforcement, and neither is bypassable from the CLI —
// note in particular that there is NO --yes. A flag that skipped the confirmation would
// hand every unattended agent on the host exactly the capability the confirmation exists
// to withhold, and it would do it in a single word that looks like a convenience.
type ptyVerifier struct {
	in  io.Reader
	out io.Writer
}

func (*ptyVerifier) Name() string { return "cli-pty" }

func (p *ptyVerifier) VerifyCapability(_ context.Context, c Capability) (Attestation, error) {
	if !interactive(p.in) {
		return Attestation{}, fmt.Errorf("no terminal is attached, so there is nobody here to confirm this %s. "+
			"The CLI's only root of trust is a typed confirmation at a real terminal, and an unattended process cannot "+
			"produce one — nor may it, since a confirmation with no human present attests to nothing. An unattended %s "+
			"needs a host verifier that can establish authority outside this store (steward.WithVerifier)", c.Action, c.Action)
	}

	verb := "CLAIM"
	if c.Action == ActionTakeover {
		verb = "SEIZE"
	}
	fmt.Fprintf(p.out, "\nYou are authorizing %s to %s the steward seat on this machine.\n\n", c.Actor, verb)
	if c.Phase == PhaseConsume {
		fmt.Fprintf(p.out, "  This is the moment authority actually moves — authorization %s is being SPENT.\n\n", c.Nonce)
	}
	switch c.Seat.Liveness {
	case LivenessVacant:
		fmt.Fprintln(p.out, "  the seat is currently VACANT.")
	default:
		fmt.Fprintf(p.out, "  current holder: %s (epoch %d, liveness %s)\n",
			holderName(c.Seat.Authority.Holder), c.Seat.Authority.Epoch, c.Seat.Liveness)
		if c.Seat.Liveness == LivenessLive {
			fmt.Fprintln(p.out, "  THIS STEWARD IS ALIVE. It may be mid-thought. Its writes will be rejected from the")
			fmt.Fprintln(p.out, "  instant this lands.")
		}
		if c.Seat.Liveness == LivenessLapsed {
			fmt.Fprintln(p.out, "  A LAPSE PROVES A HEARTBEAT GAP AND NOTHING MORE. They may be mid-thought,")
			fmt.Fprintln(p.out, "  rate-limited, or paused at a prompt, and they will be FENCED the instant this lands.")
		}
	}
	if c.Receipt != nil {
		fmt.Fprintf(p.out, "\n  a receipt from %q is attached (%s). Its bytes are pinned; its AUTHOR is not\n",
			c.Receipt.Issuer, short8(c.Receipt.Digest))
		fmt.Fprintln(p.out, "  verified by anything — a digest is integrity, never authenticity. You are the check.")
	}
	fmt.Fprintf(p.out, "\nType the epoch to confirm (%d): ", c.FromEpoch)

	line, err := bufio.NewReader(p.in).ReadString('\n')
	if err != nil {
		return Attestation{}, fmt.Errorf("the confirmation could not be read: %w", err)
	}
	if strings.TrimSpace(line) != fmt.Sprint(c.FromEpoch) {
		return Attestation{Approved: false, Grade: GradeAudit, Channel: "pty",
			Why: "the typed confirmation did not match the epoch"}, nil
	}
	return Attestation{
		Channel:  "pty",
		Grade:    GradeAudit,
		Approved: true,
		Why:      "a typed confirmation at a terminal — deliberate and attended, NOT proof a human was present",
	}, nil
}

func newGrantsCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "grants",
		Short: "list authorizations, and whether they can still be used",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			gs, err := s.ListGrants(time.Now())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, gs)
			}
			if len(gs) == 0 {
				fmt.Fprintln(out, "no authorizations")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "  ID\tPROVENANCE\tACTOR\tSEIZES EPOCH\tEXPIRES\tUSABLE")
			for _, g := range gs {
				usable := "no — " + g.Reason
				if g.Usable {
					usable = "yes"
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%s\n",
					g.Grant.ID, g.Grant.Provenance, g.Grant.Actor, g.Grant.FromEpoch,
					g.Grant.ExpiresAt.Local().Format(time.RFC3339), usable)
			}
			return tw.Flush()
		},
	}
}

func newTakeoverCmd(o *opts) *cobra.Command {
	var grant, grantFile string
	cmd := &cobra.Command{
		Use:   "takeover",
		Short: "seize the seat with a capability from `steward authorize`, fencing the prior holder",
		Long: `takeover is the RECOVERY path, and it is deliberately the loud one.

It seizes the seat whether or not the incumbent is live — including the case ` + "`claim`" + `
refuses, where the liveness record is missing or cannot be trusted — bumps the fencing
epoch, and records the capability it was performed under in the journal forever.

It CONSUMES a grant (` + "`steward authorize`" + `). The grant is single-use, expiring, bound to
one agent and to the exact epoch it was minted against; the takeover entry names its
nonce, so replay refuses any second use of it. An agent cannot decide on its own to
take over, because an agent that could would eventually do it to a healthy steward.

An UNATTENDED takeover (no terminal) requires a grant carrying an EXTERNAL RECEIPT.
With nobody present, "a human authorized this" is a sentence with no author; a receipt
is an artifact somebody can go and audit.

It never asks the incumbent — an incumbent that could be asked would not need taking
over. From the instant the epoch bumps, the prior holder's writes are REJECTED rather
than interleaved, so a steward that comes back from a network partition mid-sentence
cannot corrupt the record.`,
		Example: `  bashy steward authorize --actor qiangli --reason "incumbent wedged"
  bashy steward takeover --grant g-9f2c…`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.authStore(cmd)
			if err != nil {
				return err
			}
			v, err := s.Takeover(cmd.Context(), Self(), SeatRequest{
				GrantID:   grant,
				GrantPath: grantFile,
				Attended:  interactive(cmd.InOrStdin()),
			}, time.Now())
			if err != nil {
				return err
			}
			// The seat is held; open the room that makes its holder reachable.
			// Failure is reported, never fatal — a host with an accountable
			// steward and no intercom is strictly better than one with neither.
			if line := assumeSeatRoom(seatHolderName()); line != "" && !o.asJSON {
				defer fmt.Fprint(cmd.OutOrStdout(), line)
			}
			if err := ExportEpoch(v.Authority.Epoch); err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), v)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "TOOK OVER the steward seat: %s at epoch %d\n",
				holderName(v.Authority.Holder), v.Authority.Epoch)
			if a := v.Authority.Authz; a != nil {
				fmt.Fprintf(out, "  under:  grant %s (%s), actor %q, attended=%v\n", a.GrantID, a.Provenance, a.Actor, a.Attended)
				if a.Provenance == ProvenanceOperatorAssertion {
					fmt.Fprintln(out, "          recorded as an operator ASSERTION — not proof a human was present")
				}
				if a.Receipt != nil {
					fmt.Fprintf(out, "          receipt %s from %s\n", short8(a.Receipt.Digest), a.Receipt.Issuer)
				}
			}
			if v.Authority.TakenOverFrom != nil {
				fmt.Fprintf(out, "  fenced: %s — any write it attempts at its old epoch is now rejected\n",
					holderName(*v.Authority.TakenOverFrom))
			}
			fmt.Fprintf(out, "\n  export %s=%d\n", EpochEnv, v.Authority.Epoch)
			fmt.Fprintln(out, "\nRun `steward reconcile` now: the prior steward may have left claims nobody has checked.")
			return nil
		},
	}
	cmd.Flags().StringVar(&grant, "grant", "", "the authorization id to consume (from `steward authorize`)")
	cmd.Flags().StringVar(&grantFile, "grant-file", "", "read the authorization from a file instead")
	return cmd
}

func newReleaseCmd(o *opts) *cobra.Command {
	var note string
	var epoch uint64
	cmd := &cobra.Command{
		Use:   "release",
		Short: "vacate the seat cleanly (captures no repository state)",
		Long: `release vacates the seat so the next steward can claim it without waiting for the
lease to lapse.

It is a COURTESY, not a correctness requirement — an unreleased seat still lapses, and
the epoch still fences whoever comes back. The system is designed for the steward that
never gets to say goodbye; releasing merely saves the next one a wait.

It is FENCED like every other mutation, and this is the most dangerous place not to be:
a fenced steward "tidying up" on its way out would otherwise vacate the seat of the
steward that replaced it.

It captures NO repository state: no diff, no branch, no working tree. A steward hands
over a MANDATE. Work in flight travels by ` + "`bashy handoff`" + `, which is a different verb
because it is a different thing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			if err := s.Release(Self(), ep, note, time.Now()); err != nil {
				return err
			}
			// The seat is vacated; close its room. A released seat whose room
			// stays open leaves a channel addressed to somebody who has
			// explicitly stepped away — worse than no channel, because it
			// still looks live.
			roomLine := releaseSeatRoom(seatHolderName())
			if roomLine != "" && !o.asJSON {
				defer fmt.Fprint(cmd.OutOrStdout(), roomLine)
			}
			if o.asJSON {
				v, err := s.Status(time.Now())
				if err != nil {
					return err
				}
				return emitJSON(cmd.OutOrStdout(), v)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "released the steward seat. The journal keeps everything; only the seat is empty.")
			fmt.Fprintln(cmd.OutOrStdout(), "No repository state was captured — `bashy handoff` is the verb that moves WORK.")
			return nil
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "why you are standing down")
	epochFlag(cmd, &epoch)
	return cmd
}

func newHeartbeatCmd(o *opts) *cobra.Command {
	var epoch uint64
	cmd := &cobra.Command{
		Use:   "heartbeat",
		Short: "refresh the holder's liveness (writes no journal entry)",
		Long: `heartbeat refreshes the seat's liveness so it does not lapse.

It writes NO journal entry. A heartbeat is a pulse, not history, and a journal that
recorded every pulse would bury the events that matter underneath them.

It is FENCED. A heartbeat is a claim to be the live holder — the most consequential
claim in the system, since it is what keeps everyone else out — so a zombie cannot
refresh a tenure that ended.

It is also the holder's way OUT of an unknown liveness. If the heartbeat file was
deleted or corrupted, the journal still knows you hold the seat: heartbeating rebuilds
the record from it, and status goes back to live.

Recording to the journal already heartbeats — a steward that is actively writing is
self-evidently alive — so this is only needed by a steward that is thinking for a long
time without producing any record.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			if err := s.Heartbeat(Self(), ep, time.Now()); err != nil {
				return err
			}
			if o.asJSON {
				v, err := s.Status(time.Now())
				if err != nil {
					return err
				}
				return emitJSON(cmd.OutOrStdout(), v)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "heartbeat recorded.")
			return nil
		},
	}
	epochFlag(cmd, &epoch)
	return cmd
}

// ─── status ───────────────────────────────────────────────────────────────────

// statusEnvelope is the stable --json shape for `steward status`.
type statusEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	Seat          View   `json:"seat"`
	Board         Board  `json:"board"`
	Journal       struct {
		Entries int    `json:"entries"`
		Head    string `json:"head"`
		Intact  bool   `json:"intact"`
		Corrupt string `json:"corrupt,omitempty"`
	} `json:"journal"`
	Scope string `json:"scope"`
	Dir   string `json:"dir"`
}

func newStatusCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "who holds the seat, are they alive, and what does the board say",
		Long: `status answers the two questions a successor asks first, and keeps them SEPARATE:

  AUTHORITY — who holds the seat, at which epoch. Replayed from the journal, so it
              survives losing everything else. Delete the heartbeat file entirely and
              the holder and epoch are still known.
  LIVENESS  — is that holder still breathing. Read from the heartbeat record, and only
              believed if that record AGREES with the journal. A heartbeat naming the
              wrong holder, the wrong epoch, or a time in the future is not a weaker
              signal — it is no signal, and it reports as unknown.

Then the board: what is in flight, and — the part that matters — what is claimed but
never checked.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			now := time.Now()
			rep, err := s.Replay()
			if err != nil {
				return err
			}
			v := s.viewFrom(rep, now)
			board := ProjectBoard(rep.Entries, s.sealChecker())

			if o.asJSON {
				env := statusEnvelope{SchemaVersion: SchemaVersion, Seat: v, Board: board, Scope: s.Scope(), Dir: s.Dir()}
				env.Journal.Entries = len(rep.Entries)
				env.Journal.Head = rep.Head
				env.Journal.Intact = rep.Intact()
				if rep.Corrupt {
					env.Journal.Corrupt = fmt.Sprintf("line %d: %s", rep.CorruptLine, rep.CorruptReason)
				}
				return emitJSON(cmd.OutOrStdout(), env)
			}

			out := cmd.OutOrStdout()
			switch v.Liveness {
			case LivenessVacant:
				fmt.Fprintf(out, "seat: VACANT — no steward on this host/user\n")
				if v.Authority.Epoch > 0 {
					fmt.Fprintf(out, "  epoch: %d (the ladder never resets; the next claim takes %d)\n",
						v.Authority.Epoch, v.Authority.Epoch+1)
				}
				fmt.Fprintln(out, "  claim it: `steward claim`")
			default:
				fmt.Fprintf(out, "seat: %s (epoch %d) — %s\n", holderName(v.Authority.Holder), v.Authority.Epoch, v.Liveness)
				if !v.Since.IsZero() {
					fmt.Fprintf(out, "  since:     %s\n", v.Since.Format(time.RFC3339))
				}
				switch v.Liveness {
				case LivenessLive:
					fmt.Fprintf(out, "  heartbeat: %s (%s ago)\n", v.Heartbeat.Format(time.RFC3339), short(now.UTC().Sub(v.Heartbeat)))
				case LivenessLapsed:
					fmt.Fprintf(out, "  heartbeat: %s (%s ago — LAPSED, which proves a lapse and nothing more:\n"+
						"             they may be mid-thought, throttled, or coming back. Claiming FENCES them, safely.)\n",
						v.Heartbeat.Format(time.RFC3339), short(now.UTC().Sub(v.Heartbeat)))
				case LivenessUnknown:
					fmt.Fprintf(out, "  heartbeat: UNKNOWN — %s\n", v.LivenessReason)
					fmt.Fprintln(out, "             Authority above is still replayed from the journal, and the seat is NOT")
					fmt.Fprintln(out, "             claimable: an unreadable liveness record says nothing about the holder.")
					fmt.Fprintln(out, "             The holder can restore it with `steward heartbeat`; anyone else needs a takeover.")
				}
				if v.Intent != "" {
					fmt.Fprintf(out, "  intent:    %s\n", v.Intent)
				}
				if a := v.Authority.Authz; a != nil {
					fmt.Fprintf(out, "  took over: grant %s (%s), actor %q\n", a.GrantID, a.Provenance, a.Actor)
				}
			}

			fmt.Fprintf(out, "  journal:   %d entries, ", len(rep.Entries))
			if rep.Intact() {
				fmt.Fprintln(out, "intact")
			} else {
				fmt.Fprintf(out, "UNREADABLE TAIL at line %d (%s)\n", rep.CorruptLine, rep.CorruptReason)
				fmt.Fprintf(out, "             the %d entries above are valid and unaffected; `steward repair --plan` says what can be done\n", len(rep.Entries))
			}
			fmt.Fprintf(out, "  store:     %s\n", s.Dir())

			// UNREAD MAIL, on the one screen a steward already looks at.
			//
			// The seat inbox is worthless if its holder has to know it exists.
			// A count here is what turns `steward inbox` from a verb somebody
			// has to be told about into one they discover the moment there is a
			// reason to. Peeked, so looking at status never marks mail read.
			if n, err := bus.SeatPending(stewardAssignment().Topic(), true, false); err == nil && len(n) > 0 {
				fmt.Fprintf(out, "  inbox:     %d UNREAD — `bashy steward inbox`\n", len(n))
			}

			fmt.Fprintln(out)
			writeBoard(out, board)
			return nil
		},
	}
}

// ─── board ────────────────────────────────────────────────────────────────────

func newBoardCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "board [workstream]",
		Short: "the host Kanban: lanes, priorities, owners, blockers, next actions — and what is unproven",
		Long: `board is the host-level Kanban ABOVE the per-project issue queues: every strand of
work on this machine, whoever owns it, whatever repo it lives in.

  LANE        backlog → ready → in-progress → blocked → review → done. A strand with
              live blockers is shown as BLOCKED no matter what lane anyone typed: a
              board that let you park a blocked item in "in-progress" would be hiding
              the only thing worth looking at.
  PRIORITY    p0..p3. Untriaged sorts LAST, not in the middle — it has not earned a
              place ahead of something a human actually called p2.
  BLOCKERS    what is in the way. NEXT is what happens next, and when.
  LINKS       out to where the work really lives: issue:88, github:pr-412, weave:run-88.

And it is STILL a read-only PROJECTION of the journal. Every field was recorded by
somebody, at a time, under an epoch (` + "`steward workstream set`" + `) — nothing here is a cell
that got overwritten, so the board cannot drift from the record, and "who moved this to
p0, and when" is answerable forever.

Three columns are kept deliberately apart:

  STATE       open or closed — where the work is in its lifecycle.
  OUTCOME     what was claimed.
  CONFIDENCE  verified (somebody CHECKED, and their attestation is in the journal),
              asserted (references were supplied, nobody checked them),
              unknown, degraded, or refuted.

"Closed", "claimed done", and "verified done" are three different facts. Collapsing
them into one green row is exactly how a status board starts reporting wishes.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			board, _, err := s.Board()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			if len(args) == 1 {
				for _, ws := range board.Workstreams {
					if ws.Name != args[0] {
						continue
					}
					if o.asJSON {
						return emitJSON(out, ws)
					}
					writeCard(out, ws)
					return nil
				}
				return fmt.Errorf("steward: no workstream %q on the board", args[0])
			}

			if o.asJSON {
				return emitJSON(out, board)
			}
			writeBoard(out, board)
			return nil
		},
	}
}

func writeCard(out io.Writer, ws Workstream) {
	fmt.Fprintf(out, "workstream: %s\n", ws.Name)
	if ws.Title != "" {
		fmt.Fprintf(out, "  title:      %s\n", ws.Title)
	}
	fmt.Fprintf(out, "  lane:       %s\n", ws.Lane)
	fmt.Fprintf(out, "  state:      %s\n", ws.State)
	fmt.Fprintf(out, "  priority:   %s\n", orDash(string(ws.Priority)))
	fmt.Fprintf(out, "  owner:      %s\n", orDash(ws.Owner))
	if len(ws.Agents) > 0 {
		fmt.Fprintf(out, "  agents:     %s\n", strings.Join(ws.Agents, ", "))
	}
	for _, b := range ws.Blockers {
		fmt.Fprintf(out, "  BLOCKED BY: %s\n", b)
	}
	if ws.NextAction != "" {
		fmt.Fprintf(out, "  next:       %s\n", ws.NextAction)
	}
	if !ws.NextAt.IsZero() {
		fmt.Fprintf(out, "  next at:    %s\n", ws.NextAt.Format(time.RFC3339))
	}
	for _, l := range ws.Links {
		fmt.Fprintf(out, "  link:       %s:%s\n", l.Kind, l.Ref)
	}
	fmt.Fprintf(out, "  outcome:    %s (confidence: %s)\n", orDash(string(ws.Outcome)), ws.Confidence)
	if ws.Confidence == ConfidenceAsserted {
		fmt.Fprintln(out, "              ASSERTED — references were supplied and NOBODY CHECKED THEM.")
		fmt.Fprintln(out, "              `steward verify --seq N` is what makes this verified.")
	}
	fmt.Fprintf(out, "  entries:    %d (%d evidence, %d decisions, %d verifications)\n",
		ws.Entries, ws.EvidenceCount, ws.Decisions, ws.Verifications)
	if !ws.OpenedAt.IsZero() {
		fmt.Fprintf(out, "  opened:     %s\n", ws.OpenedAt.Format(time.RFC3339))
	}
	if ws.LastSummary != "" {
		fmt.Fprintf(out, "  last:       %s\n", ws.LastSummary)
	}
	for _, d := range ws.Unproven {
		fmt.Fprintf(out, "  UNPROVEN:   %s\n", d)
	}
}

// writeBoard renders the Kanban, leading with the honest headline.
func writeBoard(out io.Writer, b Board) {
	if len(b.Workstreams) == 0 {
		fmt.Fprintln(out, "board: empty — no workstreams recorded")
		return
	}
	unproven := 0
	for _, ws := range b.Workstreams {
		if ws.Outcome == OutcomeUnknown || ws.Outcome == OutcomeDegraded {
			unproven++
		}
	}
	fmt.Fprintf(out, "board: %d workstream(s)  (watermark %d)\n", len(b.Workstreams), b.Watermark)
	if b.Blocked > 0 {
		fmt.Fprintf(out, "  %d BLOCKED\n", b.Blocked)
	}
	if unproven > 0 {
		fmt.Fprintf(out, "  %d with an outcome NOBODY ESTABLISHED\n", unproven)
	}
	if b.Asserted > 0 {
		fmt.Fprintf(out, "  %d ASSERTED but never checked — a reference is a pointer, not a verification\n", b.Asserted)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  LANE\tPRI\tWORKSTREAM\tOWNER\tOUTCOME\tCONFIDENCE\tNEXT / BLOCKED BY")
	for _, ws := range b.Workstreams {
		next := ws.NextAction
		if len(ws.Blockers) > 0 {
			next = "⛔ " + strings.Join(ws.Blockers, "; ")
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			ws.Lane, orDash(string(ws.Priority)), ws.Name, orDash(ws.Owner),
			orDash(string(ws.Outcome)), ws.Confidence, truncate(next, 40))
	}
	tw.Flush()

	if unproven > 0 {
		fmt.Fprintln(out, "\nUnproven — a claim nobody can check is not a fact:")
		for _, ws := range b.Workstreams {
			if ws.Outcome != OutcomeUnknown && ws.Outcome != OutcomeDegraded {
				continue
			}
			for _, d := range ws.Unproven {
				fmt.Fprintf(out, "  %s: %s\n", ws.Name, d)
			}
		}
	}
}

// ─── log / conversation / history ─────────────────────────────────────────────

func newLogCmd(o *opts) *cobra.Command {
	var (
		f        Filter
		kinds    []string
		since    string
		follow   bool
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "log",
		Short: "the journal, chronologically — the authoritative record itself",
		Long: `log prints the journal: the one authoritative record, in the order it was written.

Chronological because the journal is append-only — entry order IS time order, and a log
that reordered them would be inventing a history the hash chain does not attest to.

  --degraded   the query a successor needs FIRST: only the entries whose claims were
               never established. "What do I not actually know?"
  --follow     stream new entries as they land (polling; an unreadable tail does not
               stop the stream, it just does not advance past it)`,
		Example: `  bashy steward log --limit 20
  bashy steward log --degraded          # what is claimed but unproven
  bashy steward log --kind decision --workstream api
  bashy steward log --follow`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			for _, k := range kinds {
				f.Kinds = append(f.Kinds, Kind(k))
			}
			if since != "" {
				t, err := parseSince(since)
				if err != nil {
					return err
				}
				f.Since = t
			}

			entries, rep, err := s.Log(f)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			if o.asJSON {
				if err := emitJSON(out, entries); err != nil {
					return err
				}
			} else {
				if len(entries) == 0 {
					fmt.Fprintln(out, "log: no entries match")
				}
				for _, e := range entries {
					writeEntry(out, e)
				}
				warnCorrupt(out, rep)
			}
			if !follow {
				return nil
			}

			// Follow until the caller interrupts. Ctrl-C is how this ENDS, not how it
			// fails, so a cancelled follow exits 0.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return s.Follow(ctx, f, interval, func(e Entry) error {
				if o.asJSON {
					return emitJSONLine(out, e)
				}
				writeEntry(out, e)
				return nil
			})
		},
	}
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "only these kinds (effect, observation, decision, verification, transcript, reconcile, repair, checkpoint, seat.*, workstream.*)")
	cmd.Flags().StringVar(&f.Workstream, "workstream", "", "only this workstream")
	cmd.Flags().StringVar(&f.Actor, "actor", "", "only this actor")
	cmd.Flags().BoolVar(&f.DegradedOnly, "degraded", false, "only entries whose claim was never established — the 'what do I not know?' query")
	cmd.Flags().IntVar(&f.Limit, "limit", 0, "keep only the last N matches (0 = all)")
	cmd.Flags().StringVar(&since, "since", "", "only entries after this time (RFC3339, or a duration like 2h)")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream new entries as they are appended")
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "poll interval for --follow")
	return cmd
}

func newConversationCmd(o *opts) *cobra.Command {
	var f Filter
	cmd := &cobra.Command{
		Use:     "conversation",
		Aliases: []string{"conv"},
		Short:   "the decisions — and, where a transcript survives, how the room got there",
		Long: `conversation shows what was DECIDED and why.

Decisions are AUTHORITATIVE: an explicit, durable record of intent with a rationale.
They are what a successor reads to learn not just what happened on this host, but what
the previous steward had concluded and was steering toward — which no amount of
replaying effects would ever recover.

Transcripts are NOT authoritative and are shown only as a courtesy. They are hash-linked
artifacts; nothing derives from them; deleting every one of them changes no board, no
status, and no checkpoint. The decision record is what binds.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			entries, rep, err := s.Conversation(f)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, entries)
			}
			if len(entries) == 0 {
				fmt.Fprintln(out, "conversation: nothing decided yet")
				return nil
			}
			for _, e := range entries {
				switch e.Kind {
				case KindDecision:
					fmt.Fprintf(out, "── DECISION  seq %d  %s  %s\n", e.Seq, e.Time, holderName(e.Actor))
					if e.Workstream != "" {
						fmt.Fprintf(out, "   workstream: %s\n", e.Workstream)
					}
					fmt.Fprintf(out, "   %s\n", e.Summary)
					if e.Rationale != "" {
						fmt.Fprintf(out, "   because: %s\n", e.Rationale)
					}
					for _, ev := range e.Evidence {
						fmt.Fprintf(out, "   evidence: %s:%s\n", ev.Kind, ev.Ref)
					}
				case KindTranscript:
					fmt.Fprintf(out, "── transcript (NON-AUTHORITATIVE)  seq %d  %s\n", e.Seq, e.Time)
					fmt.Fprintf(out, "   %s\n", e.Summary)
					if e.Artifact != nil {
						fmt.Fprintf(out, "   artifact: %s (%s)\n", e.Artifact.Path, e.Artifact.Digest)
					}
				}
				fmt.Fprintln(out)
			}
			warnCorrupt(out, rep)
			return nil
		},
	}
	cmd.Flags().StringVar(&f.Workstream, "workstream", "", "only this workstream")
	cmd.Flags().IntVar(&f.Limit, "limit", 0, "keep only the last N (0 = all)")
	return cmd
}

// historyEnvelope is the stable --json shape for `steward history`.
type historyEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	Seat          []StateChange   `json:"seat"`
	Checkpoints   []CheckpointRef `json:"checkpoints"`
}

func newHistoryCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "history",
		Short: "how the seat changed hands, and under what authority",
		Long: `history is the seat's authority ladder, reconstructed ENTIRELY by replay.

Every claim, every takeover, every release — who, when, at which epoch, and (for a
takeover) the exact capability it was performed under: the grant, its provenance, the
operator it named, whether anyone was actually at a terminal, and whether a receipt
backed it.

It shows the PROVENANCE, not a comforting summary. "authorized by qiangli" and "an
operator ASSERTION naming qiangli, unattended, with no receipt" are different facts,
and only the second one is true.

There is nowhere else this is stored: delete the heartbeat file, delete every
checkpoint, and this history is unchanged, because it lives in the journal like
everything else that matters.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			changes, cks, rep, err := s.History()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, historyEnvelope{SchemaVersion: SchemaVersion, Seat: changes, Checkpoints: cks})
			}

			if len(changes) == 0 {
				fmt.Fprintln(out, "history: the seat has never been held")
			} else {
				fmt.Fprintln(out, "seat history:")
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "  SEQ\tWHEN\tEVENT\tEPOCH\tACTOR\tAUTHORIZATION")
				for _, c := range changes {
					fmt.Fprintf(tw, "  %d\t%s\t%s\t%d\t%s\t%s\n",
						c.Seq, c.At.Format(time.RFC3339), c.Kind, c.Epoch, c.Actor, describeAuthz(c.Authz))
				}
				tw.Flush()
			}

			if len(cks) > 0 {
				fmt.Fprintln(out, "\ncheckpoints (as the journal remembers them — the files are only a cache):")
				for _, c := range cks {
					fmt.Fprintf(out, "  seq %-4d %s  %s\n", c.Seq, c.At.Format(time.RFC3339), c.ID)
				}
			}
			warnCorrupt(out, rep)
			return nil
		},
	}
}

func describeAuthz(a *AuthzRef) string {
	if a == nil {
		return "—"
	}
	s := fmt.Sprintf("%s %q", a.Provenance, a.Actor)
	if a.Receipt != nil {
		s += " +receipt(" + a.Receipt.Issuer + ")"
	}
	if !a.Attended {
		s += " unattended"
	}
	return s
}

// ─── checkpoint ───────────────────────────────────────────────────────────────

func newCheckpointCmd(o *opts) *cobra.Command {
	var (
		note   string
		list   bool
		verify string
		epoch  uint64
	)
	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "materialize a verified, reproducible projection of the journal",
		Long: `checkpoint materializes the board at the journal's current head, and records that it
did so IN the journal.

It is an AUTHORITATIVE act — the holder, at the current epoch, or nothing. It appends
to the journal, and the file it drops in the store is exactly the artifact a later
reader trusts to summarize what happened here; neither is a thing a bystander gets to
write.

A checkpoint is a CACHE WITH A RECEIPT, never a competing truth. It carries the
watermark it projects and the chain digest at that watermark, so it can be VERIFIED
rather than trusted: re-project the journal at the same watermark and compare. Delete
every checkpoint on the host and you have lost nothing but the time to recompute them.

It carries its unknowns forward BY NAME — both the unestablished outcomes and the
merely-asserted ones. A checkpoint that quietly dropped them would look like a clean
bill of health, which is the one thing it must never be able to fake.

  --verify <id>   re-derive a stored checkpoint and report whether it still holds. A
                  mismatch means the journal beneath it changed, which — given the hash
                  chain — means someone rewrote history.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			switch {
			case verify != "":
				ck, err := s.LoadCheckpoint(verify)
				if err != nil {
					return err
				}
				v, err := s.VerifyCheckpoint(ck)
				if err != nil {
					return err
				}
				if o.asJSON {
					return emitJSON(out, v)
				}
				if v.Reproducible {
					fmt.Fprintf(out, "%s: REPRODUCIBLE — re-derived from the journal at watermark %d, digests match\n", v.ID, v.Watermark)
					return nil
				}
				fmt.Fprintf(out, "%s: NOT REPRODUCIBLE — %s\n", v.ID, v.Reason)
				fmt.Fprintf(out, "  stored board:    %s\n  derived board:   %s\n", v.StoredDigest, v.DerivedDigest)
				fmt.Fprintf(out, "  stored journal:  %s\n  derived journal: %s\n", v.StoredHead, v.DerivedHead)
				return fmt.Errorf("checkpoint %s no longer re-derives from the journal — the history beneath it changed", v.ID)

			case list:
				cks, err := s.ListCheckpoints()
				if err != nil {
					return err
				}
				if o.asJSON {
					return emitJSON(out, cks)
				}
				if len(cks) == 0 {
					fmt.Fprintln(out, "no checkpoints stored (the journal may still remember ones whose files are gone — see `steward history`)")
					return nil
				}
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "  ID\tCREATED\tWATERMARK\tWORKSTREAMS\tUNRESOLVED\tASSERTED")
				for _, ck := range cks {
					fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%d\t%d\n",
						ck.ID, ck.CreatedAt.Format(time.RFC3339), ck.Watermark,
						len(ck.Board.Workstreams), len(ck.Unresolved), len(ck.Asserted))
				}
				return tw.Flush()
			}

			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			ck, err := s.Checkpoint(Self(), ep, note, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(out, ck)
			}
			fmt.Fprintf(out, "checkpoint %s\n", ck.ID)
			fmt.Fprintf(out, "  watermark:      %d\n", ck.Watermark)
			fmt.Fprintf(out, "  journal digest: %s\n", ck.JournalDigest)
			fmt.Fprintf(out, "  board digest:   %s\n", ck.Board.Digest)
			fmt.Fprintf(out, "  workstreams:    %d\n", len(ck.Board.Workstreams))
			if len(ck.Unresolved) > 0 {
				fmt.Fprintf(out, "  CARRIED FORWARD — %d unresolved claim(s):\n", len(ck.Unresolved))
				for _, d := range ck.Unresolved {
					fmt.Fprintf(out, "    %s\n", d)
				}
			}
			if len(ck.Asserted) > 0 {
				fmt.Fprintf(out, "  CARRIED FORWARD — %d claim(s) asserted but NEVER CHECKED:\n", len(ck.Asserted))
				for _, d := range ck.Asserted {
					fmt.Fprintf(out, "    %s\n", d)
				}
			}
			fmt.Fprintf(out, "\nVerify it any time: `steward checkpoint --verify %s`\n", ck.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "why this checkpoint is being taken")
	cmd.Flags().BoolVar(&list, "list", false, "list stored checkpoints")
	cmd.Flags().StringVar(&verify, "verify", "", "re-derive a stored checkpoint and report whether it still holds")
	epochFlag(cmd, &epoch)
	return cmd
}

// ─── reconcile / repair ───────────────────────────────────────────────────────

func newReconcileCmd(o *opts) *cobra.Command {
	var (
		record bool
		epoch  uint64
	)
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "what can and cannot be established — the verb a successor runs FIRST",
		Long: `reconcile reports what the record can and cannot establish.

It is the verb a successor runs BEFORE touching anything: who holds the seat, whether
the journal is intact, which claims are unproven, which rest on references nobody
checked, and which artifacts have gone missing. That is the difference between
inheriting a SYSTEM and inheriting a STORY about a system.

IT DOES NOT COMPARE THE JOURNAL AGAINST REALITY BY ITSELF, AND IT SAYS SO.

This package is host-scoped and generic: its journal spans every project on the
machine, and it knows nothing about git, CI, GitHub, or your services. Comparing a
claim against the world needs an ADAPTER that knows how to go and look (steward.Observer
— a host wires them in). With no adapter, this report tells you exactly what it did:
it re-read the journal, and it checked NOTHING against reality. That is a spellcheck,
not a reality check, and calling it one would be the most dangerous lie in the system.

  ok        the journal is intact and every claim in it has been CHECKED. Deliberately
            hard to reach: it needs verification records, not references.
  degraded  the record is readable, but something in it could not be established — or
            rests on references nobody has checked.
  unknown   the record ITSELF is damaged. What survives is still valid; what came after
            it cannot be spoken for.

There is deliberately no "failed". This subsystem never reports success in the face of
missing evidence — and it never invents a failure it cannot prove either.

  --record  append the reconciliation to the journal (requires holding the seat), so
            "we checked, and here is what we could not establish" becomes permanent.
            Its outcome mirrors the verdict, and its summary states whether reality was
            actually compared.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			now := time.Now()

			// No observers here: the CLI is the generic front door, and it has none to
			// give. A host that has adapters calls Store.Reconcile directly.
			r, err := s.Reconcile(cmd.Context(), now)
			if err != nil {
				return err
			}
			if record {
				// EVERY way this can fail must leave the REPORT standing: no seat, no
				// fencing token to present, a superseded epoch. Reconcile is the verb a cold
				// successor runs BEFORE it holds anything, so refusing to print the truth on
				// the grounds that we could not also write it down would break the one
				// command that matters most in exactly the situation it exists for. The
				// reader gets the report, and is told plainly why it is not in the journal.
				ep, err := ResolveEpoch(epoch)
				if err == nil {
					_, err = s.RecordReconciliation(Self(), ep, r, now)
				}
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "note: the report below was NOT written to the journal: %v\n\n", err)
				} else if r2, err := s.Reconcile(cmd.Context(), now); err == nil {
					r = r2 // the reconcile entry we just wrote is itself history now
				}
			}

			if o.asJSON {
				return emitJSON(out, r)
			}

			fmt.Fprintf(out, "reconciliation: %s\n\n", strings.ToUpper(string(r.Health)))

			switch r.Seat.Liveness {
			case LivenessVacant:
				fmt.Fprintln(out, "seat:     VACANT")
			default:
				fmt.Fprintf(out, "seat:     %s (epoch %d) — %s\n",
					holderName(r.Seat.Authority.Holder), r.Seat.Authority.Epoch, r.Seat.Liveness)
				if r.Seat.LivenessReason != "" {
					fmt.Fprintf(out, "          %s\n", r.Seat.LivenessReason)
				}
			}

			fmt.Fprintf(out, "journal:  %d entries, ", r.JournalEntries)
			if r.JournalIntact {
				fmt.Fprintf(out, "intact (head %s)\n", short8(r.JournalHead))
			} else {
				fmt.Fprintf(out, "DAMAGED — %s\n", r.CorruptTail)
				fmt.Fprintln(out, "          `steward repair --plan` says whether this is a torn append (repairable) or something worse")
			}
			fmt.Fprintf(out, "board:    %d workstream(s)\n", len(r.Board.Workstreams))
			fmt.Fprintf(out, "reality:  %s\n", r.RealityNote)

			if len(r.Unproven) > 0 {
				fmt.Fprintf(out, "\nUNPROVEN — %d claim(s) you must not take on faith:\n", len(r.Unproven))
				for _, u := range r.Unproven {
					fmt.Fprintf(out, "  seq %-4d [%s] claimed %s, effective %s\n", u.Seq, orDash(u.Workstream), u.Claimed, u.Effective)
					fmt.Fprintf(out, "           %s\n           why: %s\n", u.Summary, u.Why)
				}
			}
			if len(r.Asserted) > 0 {
				fmt.Fprintf(out, "\nASSERTED, NEVER CHECKED — %d claim(s) resting on references nobody verified:\n", len(r.Asserted))
				for _, u := range r.Asserted {
					fmt.Fprintf(out, "  seq %-4d [%s] %s\n", u.Seq, orDash(u.Workstream), u.Summary)
					fmt.Fprintf(out, "           verify it: `steward verify --seq %d --result <success|failed> --method <how>`\n", u.Seq)
				}
			}
			if len(r.Observations) > 0 {
				fmt.Fprintf(out, "\nobservations (%d) — what the adapters actually FOUND:\n", len(r.Observations))
				for _, ob := range r.Observations {
					fmt.Fprintf(out, "  seq %-4d %s: %s — %s\n", ob.Seq, ob.Observer, ob.Result, ob.Detail)
				}
			}
			for _, e := range r.ObserverErrors {
				fmt.Fprintf(out, "  adapter FAILED: %s\n", e)
			}
			if len(r.MissingArtifacts) > 0 {
				fmt.Fprintf(out, "\nmissing artifacts (%d) — OPTIONAL by contract; no projection depends on them,\n"+
					"and the board is bit-identical without them:\n", len(r.MissingArtifacts))
				for _, m := range r.MissingArtifacts {
					fmt.Fprintf(out, "  %s\n", m)
				}
			}
			if len(r.TamperedArtifacts) > 0 {
				fmt.Fprintf(out, "\nTAMPERED ARTIFACTS (%d) — an absent artifact is a gap; an ALTERED one is a lie:\n", len(r.TamperedArtifacts))
				for _, t := range r.TamperedArtifacts {
					fmt.Fprintf(out, "  %s\n", t)
				}
			}
			if len(r.CheckpointsVerified) > 0 {
				fmt.Fprintln(out, "\ncheckpoints:")
				for _, v := range r.CheckpointsVerified {
					if v.Reproducible {
						fmt.Fprintf(out, "  %s: reproducible\n", v.ID)
					} else {
						fmt.Fprintf(out, "  %s: NOT REPRODUCIBLE — %s\n", v.ID, v.Reason)
					}
				}
			}

			switch r.Health {
			case HealthOK:
				fmt.Fprintln(out, "\nEverything in the record has been checked. Safe to proceed.")
			case HealthDegraded:
				fmt.Fprintln(out, "\nThe record is readable but INCOMPLETE. Treat the claims above as open questions —")
				fmt.Fprintln(out, "they are exactly the things a previous agent said were done and nobody confirmed.")
			case HealthUnknown:
				fmt.Fprintln(out, "\nThe RECORD ITSELF is damaged. What survives above is valid; what came after it cannot be")
				fmt.Fprintln(out, "spoken for. Do not treat the absence of an entry as proof that nothing happened.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&record, "record", false, "append the reconciliation to the journal (requires holding the seat)")
	epochFlag(cmd, &epoch)
	return cmd
}

func newRepairCmd(o *opts) *cobra.Command {
	var (
		plan  bool
		epoch uint64
	)
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "truncate a TORN FINAL APPEND — and nothing else — quarantining what it discards",
		Long: `repair fixes exactly one kind of damage: a torn final append.

That is what a crash leaves behind — the process died partway through writing the last
line, so the file ends with an incomplete fragment and no terminating newline. Nothing
that was ever completed is in those bytes, by definition, since a completed append is
fsynced with its newline.

EVERYTHING ELSE IS REFUSED, and the two refusals are the point:

  MID-LOG DAMAGE — if complete lines FOLLOW the unreadable region, then whatever is
  after it was fully written. Truncating from the damage point would destroy completed
  records, so it will not happen.

  A COMPLETE RECORD THAT DOES NOT CHAIN — a parseable entry whose hash, prev_hash, seq,
  or epoch is wrong is not a torn write. It is a record that was ALTERED, or one written
  around a record that was REMOVED. That is the signature of tampering, and a tool that
  silently truncated it away would be the attacker's best friend: it would delete the
  evidence and call it a repair.

A repair that can only ever remove garbage is a repair. A repair that can remove data is
a data-loss tool with a reassuring name.

What a repair does, in order:

  1. AUTHORIZES — the holder, at the current epoch. A damaged journal is not a licence
     for a stranger to truncate the host's record.
  2. QUARANTINES — the exact discarded bytes are copied out, by digest, BEFORE the
     truncation. "The tool ate it" is not an answer to "what was in those bytes?".
  3. TRUNCATES — at the last byte that verified. A valid entry can never be removed.
  4. RECEIPTS — a durable, degraded entry under the holder's epoch saying what was
     discarded and where it went. If the receipt cannot be written, that is an ERROR,
     loudly: a log that quietly healed itself is indistinguishable from a log somebody
     edited.

  --plan   say what would be done, and change nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			p, err := s.PlanRepair()
			if err != nil {
				return err
			}
			if plan {
				if o.asJSON {
					return emitJSON(out, p)
				}
				if !p.Corrupt {
					fmt.Fprintln(out, "journal is intact — nothing to repair")
					return nil
				}
				fmt.Fprintf(out, "journal is DAMAGED (%s)\n", p.Kind)
				fmt.Fprintf(out, "  valid entries:  %d (%d bytes)\n", p.ValidEntries, p.ValidBytes)
				fmt.Fprintf(out, "  unreadable:     %d byte(s)  %s\n", p.SuffixBytes, short8(p.SuffixDigest))
				fmt.Fprintf(out, "  bytes:          %s\n", p.SuffixPreview)
				fmt.Fprintf(out, "  repairable:     %v\n", p.Repairable)
				fmt.Fprintf(out, "  %s\n", p.Reason)
				return nil
			}
			if !p.Corrupt {
				fmt.Fprintln(out, "journal is intact — nothing to repair")
				return nil
			}

			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			res, err := s.Repair(Self(), ep, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(out, res)
			}
			fmt.Fprintf(out, "repaired: discarded %d torn trailing byte(s) after the last valid entry.\n", res.Discarded)
			fmt.Fprintf(out, "  quarantined: %s (%s)\n", res.QuarantinePath, short8(res.SuffixDigest))
			fmt.Fprintf(out, "  receipt:     seq %d, outcome degraded — a repair is never a clean success\n", res.Receipt.Seq)
			fmt.Fprintln(out, "No valid entry was removed: the cut is at the last byte that verified.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "say what would be done, and change nothing")
	epochFlag(cmd, &epoch)
	return cmd
}

// ─── writes ───────────────────────────────────────────────────────────────────

func newRecordCmd(o *opts) *cobra.Command {
	var (
		workstream, ref, summary, rationale, outcome string
		evidence                                     []string
		observation                                  bool
		epoch                                        uint64
	)
	cmd := &cobra.Command{
		Use:   "record",
		Short: "append an evidence-bearing effect or observation to the journal",
		Long: `record appends an AUTHORITATIVE entry: something happened in the world.

Bring evidence — and know what evidence buys you.

An entry claiming --outcome success with NO -e/--evidence does not project as success.
It projects as UNKNOWN, on every board, in every checkpoint, forever. The claim is still
recorded faithfully (it is an honest record of what was asserted), but no view will
promote it into a fact.

An entry WITH references projects as ASSERTED — still not verified. A reference is a
pointer, not a check. "command:go test ./..." records that you SAY you ran the tests; it
does not record that they ran, or passed, or exist. A model producing a confident summary
with a plausible command string attached is the most common way a fabricated success
enters a system that means well, because the reference is exactly as easy to generate as
the prose. Only ` + "`steward verify`" + ` — somebody going and looking — makes a claim verified.

Evidence is anything a skeptic could go and check:

  command:go test ./...        file:/tmp/build.log#sha256:abc…
  commit:de6485c               test:TestFencing
  url:https://…                note:asked the human, they confirmed

Only the holder of the seat may write, and only at the CURRENT epoch, and only by
PRESENTING it (--epoch, or $` + EpochEnv + ` exported at claim time). A steward that lapsed and
was taken over is fenced, and its writes are rejected loudly rather than silently
interleaved with its successor's.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			if strings.TrimSpace(summary) == "" {
				return fmt.Errorf("steward: a record needs a summary (-m)")
			}
			evs, err := parseEvidenceList(evidence)
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			kind := KindEffect
			if observation {
				kind = KindObservation
			}
			e, err := s.Record(Entry{
				Actor:      Self(),
				Kind:       kind,
				Workstream: workstream,
				Ref:        ref,
				Summary:    summary,
				Rationale:  rationale,
				Outcome:    Outcome(outcome),
				Evidence:   evs,
			}, ep, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), e)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "recorded seq %d (%s, epoch %d)\n", e.Seq, e.Kind, e.Epoch)
			if eff := e.EffectiveOutcome(); eff != e.Outcome {
				fmt.Fprintf(out, "\nNOTE: you claimed %q with NO EVIDENCE, so the board will show %q.\n", e.Outcome, eff)
				fmt.Fprintln(out, "The claim is recorded exactly as you made it — but nothing will project it as a fact.")
				fmt.Fprintln(out, "Add something checkable: -e \"command:…\" -e \"commit:…\" -e \"test:…\"")
			} else if e.Outcome == OutcomeSuccess {
				fmt.Fprintf(out, "\nThe board will show this as ASSERTED, not verified: you pointed at something, and nobody\n"+
					"has checked it. When someone does: `steward verify --seq %d --result success --method <how>`\n", e.Seq)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&summary, "message", "m", "", "what happened (required)")
	cmd.Flags().StringVar(&workstream, "workstream", "", "the strand of work this belongs to")
	cmd.Flags().StringVar(&ref, "ref", "", "a free pointer (issue, PR, host, service)")
	cmd.Flags().StringVar(&outcome, "outcome", "", "success | failed | degraded | unknown")
	cmd.Flags().StringVar(&rationale, "rationale", "", "why")
	cmd.Flags().StringSliceVarP(&evidence, "evidence", "e", nil, "checkable reference: kind:ref[#sha256:…] (repeatable)")
	cmd.Flags().BoolVar(&observation, "observation", false, "you OBSERVED this rather than caused it")
	epochFlag(cmd, &epoch)
	return cmd
}

func newDecideCmd(o *opts) *cobra.Command {
	var (
		workstream, summary, rationale string
		evidence                       []string
		epoch                          uint64
	)
	cmd := &cobra.Command{
		Use:   "decide",
		Short: "record an explicit, durable decision — what was decided, and WHY",
		Long: `decide records a DECISION: an explicit, durable statement of intent with a rationale.

A decision is authoritative on its own terms. It asserts INTENT, not effect, so it needs
a rationale rather than evidence — nothing about the world is being claimed.

This is the entry a successor reads to understand not just what happened on this host,
but what the previous steward had CONCLUDED and was steering toward. No amount of
replaying effects would recover that: effects tell you what was done, and only a decision
record tells you what it was for.`,
		Example: `  bashy steward decide --workstream api -m "drop the v1 endpoint" \
        --rationale "no callers in 90 days of logs; keeping it forces the auth shim to stay"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			if strings.TrimSpace(summary) == "" {
				return fmt.Errorf("steward: a decision needs a summary (-m)")
			}
			if strings.TrimSpace(rationale) == "" {
				// A decision with no WHY is the one thing a successor cannot reconstruct.
				return fmt.Errorf("steward: a decision needs a rationale (--rationale): " +
					"a successor can replay every effect on this host and still never recover WHY you chose this")
			}
			evs, err := parseEvidenceList(evidence)
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			e, err := s.Decide(Self(), ep, workstream, summary, rationale, evs, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), e)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "decision recorded: seq %d (epoch %d)\n", e.Seq, e.Epoch)
			return nil
		},
	}
	cmd.Flags().StringVarP(&summary, "message", "m", "", "what was decided (required)")
	cmd.Flags().StringVar(&rationale, "rationale", "", "why (required)")
	cmd.Flags().StringVar(&workstream, "workstream", "", "the strand of work this belongs to")
	cmd.Flags().StringSliceVarP(&evidence, "evidence", "e", nil, "supporting reference (repeatable)")
	epochFlag(cmd, &epoch)
	return cmd
}

func newVerifyCmd(o *opts) *cobra.Command {
	var (
		seq            uint64
		hash           string
		result, method string
		evidence       []string
		epoch          uint64
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "attest that you WENT AND CHECKED an earlier entry's claim",
		Long: `verify records a VERIFICATION: somebody went and checked an earlier claim, and this is
the durable attestation of what they found.

It is the ONLY thing that promotes a claim to "verified" on the board.

Everything else — every command string, every commit hash, every URL attached to the
original entry — is a REFERENCE. A reference tells a skeptic where to look. It does not
tell them what they will find, and it is exactly as easy for an agent to invent as the
prose it accompanies. The gap between "here is a plausible pointer" and "I looked" is the
gap this command exists to close, and nothing else in the package closes it.

It binds to the target's HASH, not just its sequence number: an attestation has to name
the exact bytes it vouched for, or it becomes an attestation of whatever ends up at that
seq.

A verification can move a claim BACKWARDS — --result failed refutes it, and the board
believes the refutation. Degradation travels one way.

BUT NOTHING YOU CAN TYPE HERE PROMOTES A CLAIM.

This is the sharp edge, and it has been blunt twice. First --method was required and the
board promoted on the strength of it — so an agent that would write "done ✅" it had not
earned would just as happily write --method "re-ran the suite on a clean checkout" it had
not run. Then digest-bound evidence promoted — but a digest proves the BYTES DID NOT
CHANGE, never that a check ran: attach -e 'file:/tmp/pass.log#sha256:…' and the row went
green, and since nothing rehashed it, any thirty-two bytes did. Each time, the trust-me
claim was not eliminated; it was moved one field sideways and promoted there instead.

So promotion now rests on something you CANNOT WRITE: a seal from a verification verifier
the HOST injected (steward.WithVerificationVerifier) — a CI adapter that asks the CI
system, a git adapter that looks at the commit, a signing service you hold no key for. The
store asks it here, and the BOARD ASKS IT AGAIN when it projects, so a seal typed into the
journal by hand is refused by the one party that can recognize its own.

THIS CLI INJECTS NO SUCH VERIFIER. So on this surface, "verify --result success" RECORDS
the check in full — it is in the log, a human can read the method and rehash the evidence —
and the board leaves the strand at ASSERTED. That is not a bug and it is not a
disappointment: it is the honest state of a claim on a host with nothing that can check it.
A green VERIFIED row here would be a lie the machine told itself.

REFUTING or reporting an INCONCLUSIVE check needs no credential at all, and works fully
here. Doubt is free; confidence is not. We demand credentials to become more confident and
never to become less, because the cost of a false "verified" is unbounded and the cost of a
false "refuted" is a second look.

  --method  how you checked. Still required, because a check nobody can even describe is
            not one — it just does not decide anything on its own.
  -e        what you found. Recorded, auditable, rehashable by a human — and promoting
            nothing on its own, for the reason above.`,
		Example: `  bashy steward verify --seq 7 --result success \
        --method "re-ran the suite on a clean checkout at de6485c" \
        -e "file:/tmp/test.log#sha256:9f2c…"          # recorded; stays ASSERTED here
  bashy steward verify --seq 7 --result failed --method "the endpoint 502s"   # refutes, fully`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			if seq == 0 {
				return fmt.Errorf("steward: verify needs the entry it attests to (--seq)")
			}
			evs, err := parseEvidenceList(evidence)
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			e, err := s.Attest(cmd.Context(), Self(), ep, Verification{
				TargetSeq:  seq,
				TargetHash: hash,
				Result:     Outcome(result),
				Method:     method,
				Observer:   holderName(Self()),
			}, evs, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), e)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "verification recorded: seq %d attests to seq %d (%s)\n", e.Seq, seq, e.Verifies.Result)

			// SAY WHAT IT WAS WORTH, HERE, WHERE THE OPERATOR IS LOOKING. A command that
			// prints "verification recorded" and nothing else invites exactly the belief this
			// package exists to refuse — that the claim is now checked. It is not: with no
			// trusted verifier injected, the board still says asserted, and the operator finds
			// that out later, from a board they may not read, or never.
			if e.Verifies.Result == OutcomeSuccess && !e.Verifies.Sealed() {
				fmt.Fprintln(out, "  NOT PROMOTED: the board still reports this strand as ASSERTED, not verified.")
				fmt.Fprintln(out, "  No trusted verification verifier is injected on this surface, so nothing here could")
				fmt.Fprintln(out, "  establish that the claim came true — your method and evidence are recorded and")
				fmt.Fprintln(out, "  auditable, and they promote nothing. A host wires one with WithVerificationVerifier.")
			}
			return nil
		},
	}
	cmd.Flags().Uint64Var(&seq, "seq", 0, "the journal seq you checked (required)")
	cmd.Flags().StringVar(&hash, "hash", "", "the entry hash you checked (defaults to whatever is at --seq now)")
	cmd.Flags().StringVar(&result, "result", string(OutcomeSuccess), "success | failed | unknown")
	cmd.Flags().StringVar(&method, "method", "", "HOW you checked (required)")
	cmd.Flags().StringSliceVarP(&evidence, "evidence", "e", nil, "what you found (repeatable)")
	epochFlag(cmd, &epoch)
	return cmd
}

func newTranscriptCmd(o *opts) *cobra.Command {
	var workstream, summary, file string
	var epoch uint64
	cmd := &cobra.Command{
		Use:   "transcript",
		Short: "attach an OPTIONAL, non-authoritative conversation artifact",
		Long: `transcript stores a conversation dump and records a hash-linked pointer to it.

It is NON-AUTHORITATIVE, and that is a contract, not a caveat. Nothing derives from a
transcript: delete every transcript artifact on this host and the board, the status, the
history, and every checkpoint are BIT-IDENTICAL. A test pins this
(TestTranscriptDeletionDoesNotAffectProjections) precisely so it cannot quietly stop
being true.

So why store one at all? A decision record says what was decided; a transcript lets a
human go back and see how the room got there. Useful — and never load-bearing.

It is bounded (` + "8 MiB" + `) and authorized BEFORE a byte is written. A courtesy artifact that
no projection reads does not get to fill the human's disk, and a bystander does not get
to write megabytes into the steward's store and only then be told they may not journal it.

Reads from --file, or from stdin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			var src io.Reader = cmd.InOrStdin()
			if file != "" {
				f, err := os.Open(file)
				if err != nil {
					return err
				}
				defer f.Close()
				src = f
			}
			e, err := s.Transcript(Self(), ep, workstream, summary, src, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), e)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "transcript recorded: seq %d — %s (%d bytes)\n", e.Seq, e.Artifact.Digest, e.Artifact.Bytes)
			fmt.Fprintln(cmd.OutOrStdout(), "Non-authoritative: no board, status, history, or checkpoint depends on it.")
			return nil
		},
	}
	cmd.Flags().StringVarP(&summary, "message", "m", "", "what this transcript is")
	cmd.Flags().StringVar(&workstream, "workstream", "", "the strand of work this belongs to")
	cmd.Flags().StringVar(&file, "file", "", "read the transcript from this file (default: stdin)")
	epochFlag(cmd, &epoch)
	return cmd
}

func newWorkstreamCmd(o *opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workstream",
		Aliases: []string{"ws"},
		Short:   "open, update, or close a strand of work",
	}

	var (
		title string
		epoch uint64
	)
	open := &cobra.Command{
		Use:   "open <name>",
		Short: "open a strand of work",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			e, err := s.OpenWorkstream(Self(), ep, args[0], title, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), e)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "opened workstream %q (seq %d)\n", args[0], e.Seq)
			return nil
		},
	}
	open.Flags().StringVar(&title, "title", "", "a human title for the strand")
	epochFlag(open, &epoch)

	var (
		setEpoch                     uint64
		lane, priority, owner, next  string
		nextAt                       string
		agents, blockers, links, clr []string
	)
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "record the Kanban fields: lane, priority, owner, blockers, next action, links",
		Long: `set records a change to a strand's Kanban fields.

It is an ENTRY, not an edit. Nothing is mutated in place: the board folds the latest
recorded value for each field, so the Kanban stays a pure projection of the journal, and
"who moved this to p0, and when" is answerable forever.

  --blocker   what is in the way. A strand with live blockers shows as BLOCKED on the
              board regardless of its lane.
  --clear     put a field back to empty (blockers, agents, links, owner, priority, lane,
              next_action, next_at). Unblocking is as much of an event as blocking.
  --link      where the work really lives: issue:88, github:pr-412, weave:run-88, url:…`,
		Example: `  bashy steward ws set api --priority p0 --owner qiangli --lane in-progress \
        --next "run the race gate" --link issue:88 --link weave:run-88 --agent claude-opus
  bashy steward ws set api --blocker "waiting on review of #412"
  bashy steward ws set api --clear blockers`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(setEpoch)
			if err != nil {
				return err
			}
			ls, err := parseLinks(links)
			if err != nil {
				return err
			}
			e, err := s.UpdateWorkstream(Self(), ep, args[0], WorkstreamUpdate{
				Lane:       Lane(lane),
				Priority:   Priority(priority),
				Owner:      owner,
				Agents:     agents,
				Blockers:   blockers,
				NextAction: next,
				NextAt:     nextAt,
				Links:      ls,
				Clear:      clr,
			}, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), e)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (seq %d)\n", e.Summary, e.Seq)
			return nil
		},
	}
	set.Flags().StringVar(&lane, "lane", "", "backlog | ready | in-progress | blocked | review | done")
	set.Flags().StringVar(&priority, "priority", "", "p0 | p1 | p2 | p3")
	set.Flags().StringVar(&owner, "owner", "", "who owns this strand")
	set.Flags().StringSliceVar(&agents, "agent", nil, "an agent or resource working it (repeatable)")
	set.Flags().StringSliceVar(&blockers, "blocker", nil, "what is in the way (repeatable)")
	set.Flags().StringVar(&next, "next", "", "the next action / checkpoint")
	set.Flags().StringVar(&nextAt, "next-at", "", "when the next checkpoint is due (RFC3339)")
	set.Flags().StringSliceVar(&links, "link", nil, "kind:ref — issue:88, github:pr-412, weave:run-88, url:… (repeatable)")
	set.Flags().StringSliceVar(&clr, "clear", nil, "reset a field to empty (repeatable)")
	epochFlag(set, &setEpoch)

	var (
		summary    string
		outcome    string
		evidence   []string
		closeEpoch uint64
	)
	closeCmd := &cobra.Command{
		Use:   "close <name>",
		Short: "close a strand of work, with its outcome",
		Long: `close records that a strand of work is finished.

It does NOT force the outcome to success. A closing entry claiming success with no
evidence still projects UNKNOWN; one whose references nobody attested to still projects
ASSERTED. "Closed", "claimed done", and "verified done" stay three different facts, which
is the entire difference between a status board and a wish list.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			evs, err := parseEvidenceList(evidence)
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(closeEpoch)
			if err != nil {
				return err
			}
			e, err := s.CloseWorkstream(Self(), ep, args[0], summary, Outcome(outcome), evs, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), e)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "closed workstream %q (seq %d)\n", args[0], e.Seq)
			if eff := e.EffectiveOutcome(); eff != e.Outcome {
				fmt.Fprintf(out, "\nNOTE: closed claiming %q with NO EVIDENCE — the board will show %q.\n", e.Outcome, eff)
				fmt.Fprintln(out, "Closed is not the same fact as verified done.")
			}
			return nil
		},
	}
	closeCmd.Flags().StringVarP(&summary, "message", "m", "", "how it ended")
	closeCmd.Flags().StringVar(&outcome, "outcome", "", "success | failed | degraded | unknown")
	closeCmd.Flags().StringSliceVarP(&evidence, "evidence", "e", nil, "checkable reference (repeatable)")
	epochFlag(closeCmd, &closeEpoch)

	cmd.AddCommand(open, set, closeCmd)
	return cmd
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// emitJSONLine emits one compact object per line — the shape a `--follow --json`
// consumer can read incrementally without waiting for a closing bracket.
func emitJSONLine(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

func writeEntry(out io.Writer, e Entry) {
	fmt.Fprintf(out, "seq %-4d %s  %-18s %s", e.Seq, e.Time, e.Kind, holderName(e.Actor))
	fmt.Fprintf(out, "  (epoch %d)", e.Epoch)
	if e.Workstream != "" {
		fmt.Fprintf(out, " [%s]", e.Workstream)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "         %s\n", e.Summary)
	if e.Rationale != "" {
		fmt.Fprintf(out, "         because: %s\n", e.Rationale)
	}
	if e.Outcome != "" {
		eff := e.EffectiveOutcome()
		if eff != e.Outcome {
			fmt.Fprintf(out, "         outcome: %s → %s  (claimed success with NO EVIDENCE — not projected as a fact)\n", e.Outcome, eff)
		} else {
			fmt.Fprintf(out, "         outcome: %s\n", eff)
		}
	}
	if v := e.Verifies; v != nil {
		fmt.Fprintf(out, "         verifies: seq %d (%s) — %s\n", v.TargetSeq, short8(v.TargetHash), v.Method)
	}
	if a := e.Authz; a != nil {
		fmt.Fprintf(out, "         authz:    grant %s, %s, actor %q, attended=%v\n", a.GrantID, a.Provenance, a.Actor, a.Attended)
	}
	for _, ev := range e.Evidence {
		fmt.Fprintf(out, "         evidence: %s:%s", ev.Kind, ev.Ref)
		if ev.Note != "" {
			fmt.Fprintf(out, " (%s)", ev.Note)
		}
		fmt.Fprintln(out)
	}
	if e.Artifact != nil {
		fmt.Fprintf(out, "         artifact: %s %s (non-authoritative)\n", e.Artifact.Path, short8(e.Artifact.Digest))
	}
}

// warnCorrupt tells the reader that what they just saw is a valid PREFIX, not
// necessarily the whole story. Printing the entries and staying silent about the damage
// would be the one dishonest thing this package could do.
func warnCorrupt(out io.Writer, rep *Replay) {
	if rep == nil || !rep.Corrupt {
		return
	}
	fmt.Fprintf(out, "\nWARNING: the journal's tail is unreadable from line %d (%s).\n", rep.CorruptLine, rep.CorruptReason)
	fmt.Fprintf(out, "The %d entries above ARE valid — a torn tail never hides the history before it.\n", len(rep.Entries))
	fmt.Fprintln(out, "Anything written after the tear cannot be spoken for. `steward repair --plan` says what can be done.")
}

func parseEvidenceList(in []string) ([]Evidence, error) {
	var out []Evidence
	for _, s := range in {
		ev, err := ParseEvidence(s)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

var linkKinds = map[string]bool{
	"issue": true, "pr": true, "github": true, "weave": true, "kb": true, "url": true, "host": true,
}

func parseLinks(in []string) ([]Link, error) {
	var out []Link
	for _, s := range in {
		k, ref, ok := strings.Cut(strings.TrimSpace(s), ":")
		if !ok || ref == "" || !linkKinds[k] {
			return nil, fmt.Errorf("steward: --link %q must be kind:ref, where kind is one of issue, pr, github, weave, kb, url, host", s)
		}
		out = append(out, Link{Kind: k, Ref: ref})
	}
	return out, nil
}

// parseSince accepts an RFC3339 instant or a duration ago ("2h", "30m").
func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("steward: --since %q is neither an RFC3339 time nor a duration like 2h", s)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n && n > 1 {
		return s[:n-1] + "…"
	}
	return s
}

// short8 abbreviates a digest for human output; the full value is always in --json.
func short8(digest string) string {
	if i := strings.IndexByte(digest, ':'); i >= 0 && len(digest) > i+9 {
		return digest[:i+9] + "…"
	}
	return digest
}

func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// ─── the role room ────────────────────────────────────────────────────────────

// OpenRoom and CloseRoom are the seam to pkg/role/meetroom, injected by the
// host rather than imported here.
//
// The reason is a build constraint with teeth: pkg/steward sits in the cross-OS
// canary (see scripts/crossvet.sh), and meet transitively pulls the shell
// interpreter, which the canary target cannot build. Importing the room
// implementation directly broke that gate the first time it was tried. A hook
// keeps the words — role.Assignment, role.Contact — free to use while the
// transport stays outside.
//
// Both default to nil, which means NO ROOM, reported rather than pretended.
// This is the same shape as lexicon.RecordDiscovery: one narrow, auditable path
// between two packages that must not import each other.
var (
	OpenRoom  func(a role.Assignment, holder string) (*role.Contact, error)
	CloseRoom func(c *role.Contact, holder string) error
)

// hostAssignment is this host's steward seat as a role assignment.
// stewardAssignment addresses ONE steward: a login on a machine.
//
// The Ref is the scope id — <host>-<account>-<digest> — and not the bare login,
// because a steward is NOT the user's twin. It manages the resources of one
// local login on one host, so two machines the same person is logged into hold
// two different stewards with two different journals, and an address that named
// only the login would be ambiguous between them: a message meant for the work
// on one machine would be delivered to whichever listener answered first.
//
// The twin — the identity tied to the cloudbox account, which manages the
// stewards of every host registered or shared with it — is a LEVEL UP and is
// not built. When it exists it gets its own address keyed on that account, and
// this one stays what it is: how to reach the steward of a particular machine.
// See docs/agent-identity-model.md.
func stewardAssignment() role.Assignment {
	sc, err := OSScope{}.Scope()
	if err != nil {
		return role.Assignment{Kind: role.Steward, Ref: hostLabel(), Title: "host steward"}
	}
	return role.Assignment{Kind: role.Steward, Ref: sc.ID, Title: "host steward"}
}

// assumeSeatRoom opens the steward's room, returning a line to print.
//
// A failure is REPORTED, never fatal. Holding the seat is the point; the room
// is how people reach the holder, and a host with an accountable steward and no
// intercom is strictly better than one with neither.
func assumeSeatRoom(holder string) string {
	// The bus inbox comes FIRST and does not depend on the room. A room needs
	// meet to be wired (OpenRoom is a host-injected seam and is nil in plenty of
	// builds); the inbox needs nothing, so it must not be gated behind an
	// intercom that may never open. Idempotent — see bus.EnsureRoleInbox.
	if _, err := bus.EnsureRoleInbox(stewardAssignment().Topic()); err != nil {
		return fmt.Sprintf("  seat inbox NOT open (%v) — pings to this seat will be stored and unreadable\n", err)
	}
	if OpenRoom == nil {
		return ""
	}
	c, err := OpenRoom(stewardAssignment(), holder)
	if err != nil {
		return fmt.Sprintf("  no room (%v) — reachable on bus %s only\n", err, stewardAssignment().Topic())
	}
	if err := saveSeatContact(c); err != nil {
		return fmt.Sprintf("  room open at %s but not recorded (%v)\n", c.String(), err)
	}
	return fmt.Sprintf("  reachable at %s\n", c.String())
}

// releaseSeatRoom closes the steward's room on a clean vacate.
//
// A seat released without closing its room leaves a channel addressed to
// somebody who has explicitly stepped away — worse than no channel, because it
// looks live.
func releaseSeatRoom(holder string) string {
	c, err := loadSeatContact()
	if err != nil || c == nil {
		return ""
	}
	if CloseRoom != nil {
		if err := CloseRoom(c, holder); err != nil {
			return fmt.Sprintf("  room %s could not be closed (%v)\n", c.String(), err)
		}
	}
	_ = clearSeatContact()
	return "  room closed\n"
}

// seatHolderName is the acting steward as a room participant.
//
// The room wants a name a human recognises; the journal wants a full principal
// ref. Rendering one from the other here keeps meet from needing to understand
// principals, and keeps the ref canonical where it matters.
func seatHolderName() string {
	r := Self()
	if r.Name != "" {
		return r.Name
	}
	if r.URN != "" {
		return r.URN
	}
	return "steward"
}

// newPingCmd is how anything reaches this host's point of contact.
func newPingCmd(o *opts) *cobra.Command {
	var body, priority string
	cmd := &cobra.Command{
		Use:   "ping",
		Short: "interrupt the steward holding this host's seat",
		Long: `ping reaches the steward: the single point of contact for this login on this host.

It addresses the SEAT, not the holder, so a message survives a handoff or a
takeover — the holder changes, the responsibility does not. Sent to a name, a
message would arrive for whoever held the seat when it was written.

The bus carries it, and that is the half that is actually hard: an agent mid-turn
cannot decide to go and look somewhere, so its sidecar holds the subscription off
the critical path and hands it a buffer at a turn boundary. An interrupt that
cannot be delivered is DEMOTED to a queued notification with a reason, never
dropped.

A seat that has not heartbeated still accepts a ping — the bus holds it — and the
reply says so, because "queued" and "read" are different claims.`,
		Example: "  bashy steward ping --body \"need the GPU for the incident\"\n" +
			"  bashy steward ping --body \"can you release coreutils?\" --priority interrupt",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			line, err := Ping(o.dir, seatHolderName(), body, priority)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), line)
			return nil
		},
	}
	cmd.Flags().StringVar(&body, "body", "", "what you need")
	cmd.Flags().StringVar(&priority, "priority", "", "queued (default) or interrupt")
	return cmd
}

// seatLabel is the human-readable name of this seat: which machine, which
// login. Distinct from the scope id, which is what an ADDRESS needs — a person
// reading "no steward" has to know which of their machines is being described.
func seatLabel() string {
	host := hostLabel()
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return host + "/" + u.Username
	}
	return host
}
