package bus

// Roles are ADDRESSES ON THE BOARD, not a second messaging system.
//
// This is the correction to a design that grew one channel per role. Reaching
// the steward meant `steward ping`; reading it meant `steward inbox`; a
// conductor needed `sprint ping` and would have needed `sprint inbox`. Two
// verbs per role, a separate store behind each, and every one of them
// re-implementing addressing — which is how the tool-vs-agent mismatch got in.
//
// Walking three ordinary cases makes the shape obvious. An agent volunteering
// to a steward, a conductor asking a steward for work, a coder asking a
// conductor for a task: all of them are "ask a named party for something and
// get an answer back". That is a board, and the board already had everything
// the role channels lacked — an address book, receipts, claims, selectors,
// public history, and live steering.
//
// So a role becomes a name you can address on the ONE board:
//
//	bashy mb send steward       "volunteering as conductor"
//	bashy mb send conductor:22  "ready for an assignment"
//	bashy mb                    reads all of it, roles included
//
// # What must survive from the old design
//
// One thing, and it is the whole reason roles are addressable at all: THE
// ADDRESS IS THE SEAT, NOT ITS HOLDER. If codex hands the steward seat to
// claude, mail sent to `codex-gpt5.6-sol` follows the agent rather than the
// responsibility, and a handover silently loses it. Addressing the role is what
// makes a handover keep the mail — see role.Assignment.Topic, which derives the
// topic from the assignment precisely so it cannot name a person.

import "strings"

// HostRole is a role that exists on this host and can be addressed by name.
type HostRole struct {
	// Label is what a person types: "steward", "conductor:22".
	Label string
	// Topic is the stable address it resolves to: "steward.<scope>".
	Topic string
	// Holder is the only principal authorized to drain the retained legacy
	// inbox for this role. Empty means vacant or unknown; the public board
	// address remains valid, but no generic reader may advance role state.
	Holder string
}

// HostRoles lists the addressable roles on this host, injected by whoever owns
// them.
//
// pkg/bus must not import pkg/steward or pkg/weave — the board is transport, a
// seat is authority state. Same seam shape as FleetNames and DetectHarness, and
// bound from the owner's init() for the same reason lexicon.SeatSource is: a
// seam that waits for someone to remember to wire it is the failure mode this
// codebase produces most reliably.
//
// Assign it directly only if you own EVERY role on the host. There is more than
// one owner — steward holds the seat, weave holds the conductors — so use
// RegisterHostRoles, which composes. A bare assignment from a second package
// silently replaces the first, and last-writer-wins on a var set from two
// init() functions is a bug whose outcome depends on link order.
var HostRoles func() []HostRole

// RegisterHostRoles adds a source of addressable roles.
//
// Composes rather than replaces, which is the whole reason it exists.
func RegisterHostRoles(src func() []HostRole) {
	if src == nil {
		return
	}
	prev := HostRoles
	HostRoles = func() []HostRole {
		var out []HostRole
		if prev != nil {
			out = append(out, prev()...)
		}
		return append(out, src()...)
	}
}

// ResolveRole maps a label a person typed to the role's stable address.
//
// Case-insensitive on the label, because "Steward" and "steward" are plainly
// the same request. It also accepts the TOPIC itself, so a caller that already
// has the address does not have to know it is not a label.
func ResolveRole(label string) (topic string, ok bool) {
	label = strings.TrimSpace(label)
	if label == "" || HostRoles == nil {
		return "", false
	}
	for _, r := range HostRoles() {
		if strings.EqualFold(r.Label, label) || strings.EqualFold(r.Topic, label) {
			return r.Topic, true
		}
	}
	return "", false
}

// RoleLabelFor is the reverse: the human name for an address, for rendering.
//
// Falls back to the topic when the role is not one this host knows. A message
// addressed to a role that no longer exists here must still show WHO it was
// for — the alternative is a post whose recipient is unreadable, which is how
// mail becomes unattributable in the other direction.
func RoleLabelFor(topic string) string {
	topic = strings.TrimSpace(topic)
	if topic == "" || HostRoles == nil {
		return topic
	}
	for _, r := range HostRoles() {
		if strings.EqualFold(r.Topic, topic) {
			return r.Label
		}
	}
	return topic
}

// RoleHolderFor resolves a role LABEL to whoever holds the seat RIGHT NOW.
//
// This is the read-time half of "the address is the seat, not its holder". A
// message is stored addressed to `conductor:99`; who that is gets decided every
// time somebody reads it, so a handover re-targets mail already in flight
// instead of orphaning it against a name nobody answers to any more. Never
// call this at write time and store the result — that is precisely the bug the
// role address exists to prevent.
//
// ok reports that the LABEL names a live seat on this host. A seat that exists
// but is vacant is not one: there is nobody to deliver to, and saying so is the
// honest answer.
func RoleHolderFor(label string) (holder string, ok bool) {
	label = strings.TrimSpace(label)
	if label == "" || HostRoles == nil {
		return "", false
	}
	for _, r := range HostRoles() {
		if !strings.EqualFold(r.Label, label) && !strings.EqualFold(r.Topic, label) {
			continue
		}
		if strings.TrimSpace(r.Holder) == "" {
			return "", false
		}
		return r.Holder, true
	}
	return "", false
}

// AddressedToRole reports a post addressed to a role that exists on this host.
//
// This is what makes role mail DIRECTED for a reader rather than background
// chatter, and the scoping is deliberate: a seat is host-and-login scoped, so
// every session on this host can see and act on its mail. That matches the
// board's existing rule — public by construction, where addressing says who
// should ACT and never who may read — and it is what lets a raw third-party TUI
// read the seat's mail with no identity, no flag and no setup.
func AddressedToRole(to string) bool {
	if strings.TrimSpace(to) == "" || HostRoles == nil {
		return false
	}
	for _, r := range HostRoles() {
		if strings.EqualFold(r.Topic, to) {
			return true
		}
	}
	return false
}
