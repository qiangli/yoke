package steward

// The receive half. A channel nobody reads is not a channel.
//
// `steward ping` publishes to the seat's topic and that half now works. But the
// only way to READ it was `bashy bus pending --as steward.dragon-u501-b683b300b1`
// — an incantation that requires knowing both the verb and the scope id, which
// is a string nobody memorises and no skill mentions. A channel whose read side
// costs that much is one that stays unread, and an unread channel fails exactly
// like a broken one while looking healthy.
//
// So: the same messages, addressable as the thing the reader already is.
//
//	bashy steward inbox
//
// It reads the SEAT's inbox, not the holder's. Whoever holds the seat gets the
// backlog, including everything sent to their predecessors — which is the point
// of addressing a role: a handover does not lose the mail.

import (
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/spf13/cobra"
)

// newInboxCmd reads what has been sent to this host's steward seat.
func newInboxCmd(o *opts) *cobra.Command {
	var peek, all bool
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "read what has been sent to this seat (and mark it read)",
		Long: `inbox prints the messages addressed to this host's steward SEAT.

The seat, not you. Everything sent while a predecessor held it is here too —
that is what addressing a role buys: a handover does not lose the mail.

Reading marks. Nothing is deleted, so --all always answers "what was this seat
told, and when", and --peek reads without changing anything.

Run it at the start of a turn, before planning. A message read afterwards has
already cost you the work it was trying to prevent.`,
		Example: "  bashy steward inbox\n" +
			"  bashy steward inbox --peek     # read without marking\n" +
			"  bashy steward inbox --all      # everything ever sent to this seat",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return renderSeatInbox(cmd.OutOrStdout(), o, peek, all)
		},
	}
	cmd.Flags().BoolVar(&peek, "peek", false, "read without marking anything read")
	cmd.Flags().BoolVar(&all, "all", false, "every message ever sent to this seat, read or not")
	return cmd
}

func renderSeatInbox(w io.Writer, o *opts, peek, all bool) error {
	topic := stewardAssignment().Topic()

	// Idempotent, and on the READ path deliberately: a seat claimed by an older
	// build has no inbox, and the first thing its holder does is look.
	if _, err := bus.EnsureRoleInbox(topic); err != nil {
		return fmt.Errorf("steward inbox: cannot open the seat inbox: %w", err)
	}

	// THE SAME BOARD everyone reads — this is a FILTER, not a second store.
	//
	// `bashy mb` already shows these: a post addressed to the seat is directed
	// at any reader on this host. This verb exists only to answer the narrower
	// question "what was sent to the SEAT", for a steward who does not want the
	// whole board. If the two ever disagree, the board is right.
	posts, err := bus.Posts()
	if err != nil {
		return err
	}
	var items []bus.Pending
	for _, p := range posts {
		if !bus.AddressedToRole(p.To) {
			continue
		}
		items = append(items, bus.Pending{
			Seq: p.Seq, TS: p.At, Principal: p.From, Body: p.Body,
		})
	}

	// Legacy: mail published to the seat's bus topic before the board carried
	// role addresses. Merged rather than dropped — those messages were already
	// undeliverable once, and losing them on the way to the fix would be the
	// same failure twice.
	if legacy, lerr := bus.SeatPending(topic, peek, all); lerr == nil {
		items = append(items, legacy...)
	}

	if len(items) == 0 {
		// Say WHICH seat is empty. On a host with several logins, "no messages"
		// without a name is a claim the reader cannot check.
		fmt.Fprintf(w, "no messages for %s\n", seatLabel())
		return nil
	}
	for _, it := range items {
		// Principal, not a display name: the sender is recorded as who they
		// PROVED to be. An empty one is shown as unattributed rather than
		// silently attributed to anybody — the same rule `bashy mb` now follows.
		from := strings.TrimSpace(it.Principal)
		if from == "" {
			from = "(unattributed)"
		}
		fmt.Fprintf(w, "\n— from %s", from)
		if t := strings.TrimSpace(it.TS); t != "" {
			fmt.Fprintf(w, " · %s", t)
		}
		if d := strings.TrimSpace(it.Demoted); d != "" {
			// An interrupt that could not be delivered arrived as a queued
			// message. Saying so is the difference between "nobody wanted to
			// interrupt you" and "somebody did and could not".
			fmt.Fprintf(w, " · sent as an INTERRUPT, demoted: %s", d)
		}
		fmt.Fprintf(w, "\n%s\n", strings.TrimRight(it.Body, "\n"))
	}
	fmt.Fprintf(w, "\n%d message(s) for %s.\n", len(items), seatLabel())
	if peek {
		fmt.Fprintln(w, "Peeked — nothing was marked read. Drop --peek to clear them.")
	}
	return nil
}
