package meet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Dispatched is one participant's outcome: what it had waiting, and what it said.
type Dispatched struct {
	Agent  string
	Unread int
	Reply  Event
	Err    error
}

// Dispatch wakes every participant that has mail addressed to it and has not
// read it, running one turn each.
//
// # The gap this closes
//
// A 1:1 relay DM spawns its counterpart the moment a message arrives
// (relay_dm.go runRelayDMTurn). A room does not: mail lands in the transcript
// and waits for a ROUND that, in a sprint's conductor room, nobody runs. That is
// why `meet tell` into a live conductor room was measured landing "Exit 0,
// silent", the room sitting at round 0 with no contribution for four hours.
//
// The spawn half generalises directly from the DM — it is the same runTurn. The
// half that does NOT generalise, because at N=1 it does not exist, is deciding
// WHO a message is for. A DM has exactly one possible recipient. A room has a
// roster, per-reader cursors, explicit addressees, and late-bound seat labels,
// so one arriving message can mean zero, one, or N turns. Resolving that is what
// this function adds; everything it resolves with was already here.
//
// # DIRECTED MAIL ONLY, and this is a correctness rule rather than a policy
//
// Only messages ADDRESSED to a participant wake it. An UNADDRESSED post wakes
// nobody, and the reason is that the alternative does not terminate: if "no
// addressee" also meant "everybody", each of the N replies is itself an
// unaddressed post in the same transcript, so it would wake the other N-1,
// forever.
//
// Replies are safe by exactly that token — a turn is recorded with no addressee
// (classifyTurn sets no To), so it can never itself trigger a dispatch. And
// UnreadRecords already skips a reader's OWN records, so an agent cannot wake
// itself.
//
// A BROADCAST is therefore something a caller has to SAY, not something that
// falls out of saying nothing: `--to all` (AllSeats) is directed at every
// participant and wakes each one ONCE. It terminates for the same reason
// replies do — the turns it provokes are unaddressed — so the bound is N, not
// a cascade. Rounds remain the mechanism for a moderated "everyone speaks"
// with a chair and an agenda item; a broadcast is mail, and the difference is
// that a round drives the floor while this one only fills inboxes.
//
// # Ordering and the cursor
//
// The room lease is held for the whole pass, because a turn appends to the
// transcript and two speakers holding the floor at once is the incoherence the
// lease exists to prevent. That also serialises the turns, so an agent addressed
// twice in quick succession answers once and then again, never concurrently.
//
// A participant's cursor advances ONLY after its turn is recorded. A failed turn
// therefore leaves its mail unread and the next pass retries it, which is the
// behaviour that matters: mail nobody read is the failure this whole area
// exists to prevent, and losing it to a crashed agent would be the same bug with
// a different cause.
func Dispatch(ctx context.Context, ref string) ([]Dispatched, error) {
	st, err := roomOf(ref)
	if err != nil {
		return nil, err
	}
	if st.board() {
		// A board deliberately spawns nobody: its participants read and post on
		// their own turns. Waking them here would quietly convert a board into a
		// chaired room, which is a room TYPE, not a knob.
		return nil, st.boardRefusal("dispatch mail")
	}

	lease, err := acquireRunLease(st.ID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	runner := apiRunner()
	var out []Dispatched
	for _, p := range st.Participants {
		agent := canonAgent(strings.TrimSpace(p))
		if agent == "" {
			continue
		}
		directed, _, _, through, err := UnreadRecords(st.ID, agent, 0)
		if err != nil {
			out = append(out, Dispatched{Agent: agent, Err: err})
			continue
		}
		if len(directed) == 0 {
			continue
		}
		res := Dispatched{Agent: agent, Unread: len(directed)}
		reply, err := runTurn(ctx, st, agent, dispatchPrompt(directed), runner)
		if err != nil {
			// Cursor deliberately NOT advanced: the mail is still unread and the
			// next pass will try again.
			res.Err = err
			out = append(out, res)
			continue
		}
		res.Reply = reply
		if err := MarkSeenThrough(st.ID, agent, through); err != nil {
			// The turn happened and is in the transcript. Report the cursor
			// failure rather than swallowing it — an unadvanced cursor means the
			// next pass asks the same question again, which is visible and
			// recoverable, but only if somebody is told.
			res.Err = fmt.Errorf("turn recorded but cursor not advanced: %w", err)
		}
		out = append(out, res)
	}
	return out, nil
}

// dispatchPrompt renders what was waiting, attributed.
//
// Attribution is not decoration: a participant answering "who is asking" needs
// the speaker, and a seat address means the answer goes back to a role rather
// than to whoever happened to hold it. The transcript context a turn already
// carries supplies the history; this supplies only what is NEW and FOR YOU.
func dispatchPrompt(directed []UnreadRecord) string {
	var b strings.Builder
	b.WriteString("Messages addressed to you since you last read this room:\n\n")
	for _, r := range directed {
		speaker := strings.TrimSpace(r.Event.Speaker)
		if speaker == "" {
			speaker = "someone"
		}
		fmt.Fprintf(&b, "%s: %s\n", speaker, strings.TrimSpace(r.Event.Text))
	}
	b.WriteString("\nAnswer them. Reply with your response only.")
	return b.String()
}

// newDispatchCmd exposes the pass as a verb.
//
// It is a VERB and not an automatic side effect of `meet tell`, deliberately.
// `tell` is a short-lived command: spawning turns inside it would either block
// the caller until N agents had answered, or fire them into a process about to
// exit and lose them. Neither is a good default, and choosing between them is a
// policy question that belongs to whoever is driving the room — a long-lived
// `meet serve`, a sprint's own loop, or an operator running this by hand.
//
// So the primitive is explicit and idempotent, and a caller with a process to
// spare can run it as often as it likes: with nothing addressed and unread, it
// does nothing and says so.
func newDispatchCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "dispatch <room>|<id>",
		Short: "wake every participant that has unread mail addressed to it",
		Long: "Runs one turn for each participant holding unread DIRECTED mail.\n\n" +
			"Unaddressed posts wake nobody: a broadcast that woke everyone would be\n" +
			"re-triggered by each of their replies, without end. Use `meet round` when\n" +
			"everyone should speak — a round is bounded by construction.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := Dispatch(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(map[string]any{
					"schema_version": "bashy-meet-dispatch-v1",
					"room":           args[0],
					"woken":          res,
				})
			}
			if len(res) == 0 {
				fmt.Fprintln(out, "meet dispatch: nothing addressed and unread")
				return nil
			}
			var failed int
			for _, r := range res {
				if r.Err != nil {
					failed++
					fmt.Fprintf(out, "  %-20s %d unread — FAILED: %v\n", r.Agent, r.Unread, r.Err)
					continue
				}
				fmt.Fprintf(out, "  %-20s %d unread — answered\n", r.Agent, r.Unread)
			}
			// A failure here is reported and non-zero: the mail is still owed,
			// and a caller looping on this must be able to tell.
			if failed > 0 {
				return fmt.Errorf("meet dispatch: %d of %d participants could not answer", failed, len(res))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable envelope")
	return cmd
}
