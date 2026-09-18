// Package role gives an assumed responsibility a place to be reached.
//
// Two roles in this tree carry accountability for work other agents depend on:
// the STEWARD runs a host, the CONDUCTOR delivers a sprint. Both already have a
// lifecycle — something is taken over, held with a heartbeat, and handed off —
// and both had the same gap: knowing WHO holds a role told you nothing about
// how to reach them.
//
// The rule this package exists to enforce is one line: ASSUMING A ROLE OPENS A
// ROOM, RELEASING IT CLOSES ONE. Neither half is optional, because each without
// the other is actively misleading:
//
//   - an open room with no live holder is a LIE — someone posts into it and
//     waits for an answer that is never coming;
//   - a closed room with work still running is a DEAD LETTERBOX — the work is
//     real, the channel to its owner is not.
//
// # Required, but never blocking
//
// "Required" here means the absence is VISIBLE, not that anything halts. A
// conductor who cannot open a room still has a sprint to deliver, and refusing
// to start work because the intercom is down trades real delivery for a
// protocol. So Assume reports its failure to the caller and the caller records
// "no contact" — which every surface then says out loud, rather than implying a
// channel that does not exist.
//
// # Discovery stays on the board
//
// This package is the CHANNEL, never the directory. A room can say who happens
// to be in it; it cannot say who SHOULD be holding a role. That question is
// answered by the sprint board and the steward's own record, and inverting the
// two would mean losing the one signal that matters most — a role with no live
// holder at all.
//
// # The vocabulary is separate from the transport, deliberately
//
// This package holds only the SHAPE of a role — its kind, its topic, where its
// holder can be reached. Opening and closing rooms lives in role/meetroom,
// because pkg/steward imports this one and sits in the cross-OS build canary:
// meet transitively pulls the shell interpreter, which does not build on the
// canary target, and a portability gate that a vocabulary import can break is
// not a gate. The split keeps the shared words free for anyone to use.
//
// # Why the sweep is keyed on liveness, not on a verb
//
// A role is released three ways, and only one of them runs a command. A
// graceful handoff closes its room. A successor taking over from a STALE lease
// must close the dead holder's room, because the dead holder cannot. And a
// holder that simply dies — SIGKILL, token exhaustion, a closed laptop — runs
// nothing at all.
//
// That third case is why Sweep exists and why it takes LIVENESS as an input
// rather than inferring it: the judgment of whether a holder is alive belongs
// to whoever owns the lease (weave for sprints, steward for hosts), and this
// package must not grow a second opinion about it. Without the sweep, abandoned
// rooms accumulate and every one of them looks reachable — which is worse than
// having no rooms at all.
package role

import (
	"fmt"
	"strings"
)

// Kind is the responsibility being assumed.
type Kind string

const (
	// Conductor delivers one sprint.
	Conductor Kind = "conductor"
	// Steward runs one host.
	Steward Kind = "steward"
)

// Assignment identifies the specific responsibility: a kind plus what it is
// over — a sprint id, a host name.
type Assignment struct {
	Kind Kind
	// Ref scopes the role. It is part of the bus topic, so it must be stable
	// for the life of the responsibility.
	Ref string
	// Title is human context for the room's subject line.
	Title string
}

// Topic is the bus topic that reaches whoever currently holds this role.
//
// It is derived from the assignment rather than stored, so it cannot drift from
// the thing it names, and it deliberately does NOT include the holder: the
// holder changes across a handoff, and a topic keyed on a person would strand
// every message sent to the previous one.
func (a Assignment) Topic() string {
	return fmt.Sprintf("%s.%s", a.Kind, strings.TrimSpace(a.Ref))
}

// Label is what a PERSON types to address the seat: "steward",
// "conductor:99". Topic is the wire address; this is its human spelling, and
// the two must stay in step because a label a reader cannot resolve back to a
// topic is an address that silently reaches nobody.
//
// The steward is scoped to the host rather than to a ref, so it has no
// qualifier — there is exactly one seat and "steward" names it.
func (a Assignment) Label() string {
	if a.Kind == Steward {
		return string(Steward)
	}
	return fmt.Sprintf("%s:%s", a.Kind, strings.TrimSpace(a.Ref))
}

// Contact is where a role's holder can be reached.
type Contact struct {
	Kind string `json:"kind"`
	// Ref is meet's room ID — the IDENTITY.
	//
	// Never the short room number: that is a pointer, released and REUSED when
	// a room closes, so anything holding one would eventually name a stranger's
	// meeting while looking perfectly valid.
	Ref string `json:"ref"`
	// Room is the short number, for display only. It may be stale; nothing
	// resolves against it.
	Room  int    `json:"room,omitempty"`
	Topic string `json:"topic,omitempty"`
	// Name is the stable object name the room was opened for — "sprint 126" —
	// the same string meetroom writes into the room's own metadata. Unlike
	// Room it survives a manager handoff, a title edit and a pause/resume
	// cycle, which is exactly why sprint-facing contact output leads with
	// this instead of the reusable, display-only room number. Empty for
	// roles with no durable object to name (the steward's singleton seat) and
	// for contacts opened before this field existed — see meetroom.EnsureName
	// for healing the latter in place.
	Name string `json:"name,omitempty"`
	// Holder is who CONVENED this room, and it is recorded because meet lets any
	// member post but only the ORGANIZER change the roster.
	//
	// Without it, a successor that inherited a predecessor's saved contact would
	// happily reuse the room and then be unable to close it on release — leaving
	// a live channel addressed to a seat nobody holds, which is worse than no
	// channel at all because it still looks answerable. Empty means the contact
	// predates this field; treat it as reusable, since that is what it was.
	Holder string `json:"holder,omitempty"`
}

// RefID is the room identity, or empty when there is no contact.
func (c *Contact) RefID() string {
	if c == nil {
		return ""
	}
	return c.Ref
}

// String renders a contact for someone deciding how to reach in.
//
// The stable Name leads when there is one — it is the identity that survives
// a handoff, a title edit or a pause/resume, where the short Room number is
// only ever a reusable, display-only pointer. A contact with no Name (the
// steward's singleton seat, or a legacy room nothing has healed yet) renders
// exactly as it always has.
func (c *Contact) String() string {
	if c == nil || c.Ref == "" {
		return ""
	}
	room := fmt.Sprintf("meet %s", c.Ref)
	if c.Room > 0 {
		room = fmt.Sprintf("meet #%d", c.Room)
	}
	if c.Name == "" {
		return fmt.Sprintf("%s · bus %s", room, c.Topic)
	}
	return fmt.Sprintf("%s · %s · bus %s", c.Name, room, c.Topic)
}

// Occupied is one role's room plus the caller's verdict on whether its holder
// is still alive.
//
// Live is an INPUT because the lease that answers it belongs to the caller —
// weave for sprints, steward for hosts. A second opinion here could disagree
// with the authority, and the disagreement would be invisible.
type Occupied struct {
	Label   string
	Contact *Contact
	Live    bool
}

// SweepResult reports what a sweep did, so a caller can say it out loud.
type SweepResult struct {
	Closed []string
	Failed map[string]error
}
