package room

import (
	"fmt"
	"strings"
)

// OwnerTransport answers the only question that matters before a name is given
// a seat it must answer for: CAN MAIL ADDRESSED TO IT ACTUALLY ARRIVE?
//
// # Why this is not the same as "is it registered"
//
// Registration is already enforced at both seats: `sprint start` and `meet open`
// each refuse a name the fleet does not know. Neither asks the next question,
// and measurement on 2026-09-03 showed what that costs — an agent that was
// registered and had never been launched took a sprint lease and chaired a
// meeting. It was addressable in the sense that a letter can be addressed to an
// empty house.
//
// # Why it keys on the CAPABILITY and not on the card
//
// This is the trap, and it is a live one. The refusal a sprint prints today
// offers `bashy agent track start <id> --agent NAME` as a remedy — which mints
// a room card advertising NOTHING. A predicate that asks "does a live card
// exist" therefore passes the exact case it was written to catch, and the
// remedy the tool recommends is the way to defeat it. Only a card that CLAIMS a
// delivery capability counts, because only that claim corresponds to a process
// that will do something with the message.
type OwnerTransport int

const (
	// TransportNone means nothing will carry mail to this name. Refuse the seat.
	TransportNone OwnerTransport = iota

	// TransportAttached means an external harness owns the name's foreground
	// stream and is responsible for reading it (room.CapInboxStream). Bashy is
	// NOT claiming it can inject a model turn — only that something is holding
	// the stream open and has undertaken to read it.
	//
	// It is the weaker rung on purpose. A conversation-hosted agent cannot hold
	// a foreground process for the life of a sprint: measured four times in one
	// session, twice killed by harness reaping at ~25 and exactly 60 minutes.
	TransportAttached

	// TransportManaged means bashy owns the session and injects unread input
	// through the member's real control transport (room.CapInboxDelivery).
	// This is the rung that survives an unattended run, and the one every
	// automatic owner launch should aim at.
	TransportManaged
)

func (t OwnerTransport) String() string {
	switch t {
	case TransportManaged:
		return "managed"
	case TransportAttached:
		return "attached"
	default:
		return "none"
	}
}

// Deliverable reports whether this rung may hold an accountable seat.
func (t OwnerTransport) Deliverable() bool { return t != TransportNone }

// OwnerTransportFor classifies how mail addressed to a fleet name can reach it,
// and on TransportNone returns a reason naming what was missing. The reason is
// deliberately specific: "no live session" and "a live session that advertises
// no delivery capability" are different defects with different fixes, and
// collapsing them sends an operator down the wrong one.
//
// A dead process needs no special case here: Members prunes cards whose PID is
// gone on every read, so a stale card cannot answer.
func OwnerTransportFor(name string) (OwnerTransport, string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TransportNone, "no owner named"
	}

	card, live, err := Find(AgentClaimID(name))
	if err != nil {
		return TransportNone, fmt.Sprintf("could not inspect the session claim for %q: %v", name, err)
	}
	// A pre-contract inbox watcher registered under the raw fleet name rather
	// than the claim id. Keep honouring it until that process exits; every new
	// card uses the claim id, so this fallback retires itself.
	if !live && AgentClaimID(name) != name {
		card, live, err = Find(name)
		if err != nil {
			return TransportNone, fmt.Sprintf("could not inspect the session claim for %q: %v", name, err)
		}
	}
	if !live {
		return TransportNone, fmt.Sprintf("%s has no live session on this host", name)
	}

	switch {
	case HasCapability(card, CapInboxDelivery):
		return TransportManaged, ""
	case HasCapability(card, CapInboxStream):
		return TransportAttached, ""
	}

	// The loophole case, stated plainly so the operator does not retry the very
	// command that produced it.
	return TransportNone, fmt.Sprintf(
		"%s has a live session that advertises no delivery capability, so nothing "+
			"there will read its mail (a tracked work record is not a mailbox)", name)
}

// OwnerTransportRemedies is the shared fix-it text for a refused owner. It lives
// here so the sprint and meeting refusals cannot drift apart, and so neither can
// re-acquire the habit of recommending `agents track start`: that mints a work
// record with no delivery capability, which is precisely the state being
// refused.
func OwnerTransportRemedies(name string) string {
	return strings.Join([]string{
		fmt.Sprintf("  let bashy run it:  bashy chat --agent %s", name),
		fmt.Sprintf("  or hold it yourself: bashy inbox --as %s --watch", name),
	}, "\n")
}
