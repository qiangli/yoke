package steward

// REACHING THE STEWARD — the point of contact, contactable.
//
// A steward is the single point of contact for one login on one host: the seat
// anything addressed to that machine-and-login goes through, and the one that
// answers for what happened there. It was the one role in the tree you could
// not actually reach. `sprint ping` could interrupt a conductor; nothing could
// interrupt the person's own POC, which for a messenger is backwards.
//
// # It addresses the SEAT, not the holder
//
// The topic is the scope id, so a message survives a handoff or a takeover: the
// holder changes, the responsibility does not. Addressed to a name, a message
// sent an hour ago would arrive for somebody who has since released the seat —
// delivered to an agent with no context, or to nobody at all.
//
// # A stale seat is still worth sending to, and worth flagging
//
// The bus holds what it cannot deliver (demote, never drop), so a ping to a
// lapsed steward is not lost — it waits. What would be wrong is letting the
// sender believe it was READ. So the send succeeds and says plainly that the
// seat has not heartbeated, which is a different sentence from "delivered".
//
// # Why not just post in the room
//
// The room is where a conversation happens; the bus is what makes somebody look
// at it. An agent mid-turn cannot decide to go and check a room — its sidecar
// holds the subscription off the critical path and hands it a buffer at a turn
// boundary. Posting without pinging is leaving a note on a desk nobody is at.

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
)

// Ping sends an interrupt to whoever holds this host's steward seat.
//
// It returns the line to show the sender: who it went to, where to expect a
// reply, and — when the seat has not heartbeated — that the message is queued
// rather than read.
func Ping(dir, from, body, priority string) (string, error) {
	return ping(dir, from, body, priority)
}

// ping takes the store options a test needs to stay inside its own registry —
// the seat is a singleton, so a test that opened a second store for one would
// be refused, and rightly.
func ping(dir, from, body, priority string, opts ...Option) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("nothing to send — `--body \"<what you need>\"`")
	}
	st, err := Open(dir, opts...)
	if err != nil {
		return "", err
	}
	view, err := st.Status(time.Now())
	if err != nil {
		return "", err
	}
	if view.Authority.Vacant {
		// Sending to an empty seat would queue a message for nobody, and the
		// bus would hold it indefinitely. Refusing says the useful thing
		// instead: there is no POC here, and claiming the seat is how one
		// appears.
		// The readable label, not the scope id: the id is what a topic needs,
		// a person needs to know which machine and login.
		return "", fmt.Errorf("%s has no steward to ping — the seat is open (`bashy steward claim`)",
			seatLabel())
	}

	// Address the SEAT, which is what --help has always promised. The holder's
	// name is a TOOL (`codex`), every bus subscriber is an AGENT
	// (`codex-gpt5.6-sol`), and `To: Holder.Name` therefore matched nothing —
	// the ping was published, durable, and unreadable by any read verb. See
	// bus.EnsureRoleInbox.
	topic := stewardAssignment().Topic()

	// Idempotent, and called on the SEND path on purpose: a ping must not depend
	// on a claim having run under an earlier version that did not open the inbox.
	// A failure here is reported rather than swallowed — the whole defect being
	// fixed is a send that looked like it worked.
	if _, ierr := bus.EnsureRoleInbox(topic); ierr != nil {
		return "", fmt.Errorf("steward ping: cannot open the seat inbox: %w", ierr)
	}

	// THE BOARD IS THE STORE, and ping is now a board post with a steer.
	//
	// Reaching a role used to mean its own channel with its own store, so the
	// host grew two verbs per role and each re-implemented addressing — which is
	// exactly where the tool-vs-agent mismatch got in. The board already had
	// what those channels lacked: receipts, claims, selectors, public history
	// and live steering. So a ping writes where every other message on this host
	// writes, and `bashy mb` reads it with everything else.
	//
	// Board FIRST, notification second. The durable copy must not be the
	// optional one.
	if err := bus.PostMessage(bus.Post{
		From:  from,
		To:    topic,
		Topic: "steward",
		Body:  body,
	}); err != nil {
		return "", err
	}

	// ONE STORE. There is deliberately no second copy on the bus.
	//
	// A message addressed to the steward is an ordinary board post that happens
	// to name the seat: it lives where every other message on this host lives,
	// anyone can read it, and the steward reads it the same way anyone else
	// does. It is NOT filed into a private mailbox and NOT turned into a task in
	// somebody's queue — what to do about it is the steward's judgement, and a
	// channel that decides that for them is a different feature wearing a
	// message's clothes.
	//
	// Writing both here was the same mistake one layer up: two stores holding
	// one conversation, so a reader who checks the wrong one concludes nothing
	// was said.
	if strings.EqualFold(strings.TrimSpace(priority), "interrupt") {
		// The live tier is the board's own, not a parallel mechanism. It is
		// best-effort by construction: the durable post above already landed, so
		// a failed steer costs latency, never the message.
		_ = bus.SteerLive(view.Authority.Holder.Name, body)
	}

	// Say where it went and how to follow it, in the vocabulary of the ONE store
	// it went to. A sender who is told "pinged" and nothing else has no way to
	// check, and the reply comes back on the same board.
	line := fmt.Sprintf("posted to the board for the steward seat (held by %s)\n"+
		"  it is public: `bashy mb` shows it to anyone on this host, the steward included\n"+
		"  their reply comes back the same way — `bashy mb` to read it",
		view.Authority.Holder.Name)
	if view.Liveness != LivenessLive {
		// Sent, and honest about what that means. The board holds it; nobody has
		// necessarily read it.
		line += fmt.Sprintf("\n  note: the seat is %s, not live — the message is WAITING, not read", view.Liveness)
		if view.Liveness == LivenessLapsed {
			line += "\n  a lapsed seat is takeable: `bashy steward takeover` if this needs an answer now"
		}
	}
	return line, nil
}
