// Package meetroom implements pkg/role's rooms on top of `bashy meet`.
//
// It is separate from pkg/role because meet transitively pulls the shell
// interpreter, and pkg/steward — which needs the role VOCABULARY — sits in the
// cross-OS build canary that interpreter cannot satisfy. Keeping the words in
// one package and the transport in another means naming a role costs nothing.
package meetroom

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/meet"
	"github.com/qiangli/yoke/pkg/role"
)

// Assume opens the room for a newly-held role.
//
// The holder joins as a PARTICIPANT rather than as the chair: meet requires a
// chair to have someone to call on, and this room deliberately has no roster
// when it opens — existing before anyone needs it is the entire point.
// createRoom is meet.Create behind a seam, so a test can assert WHAT this
// package asks for without opening a real room on the developer's host.
var createRoom = meet.Create

func Assume(a role.Assignment, holder string) (*role.Contact, error) {
	if strings.TrimSpace(a.Ref) == "" {
		return nil, fmt.Errorf("role: assignment has no ref")
	}
	subject := fmt.Sprintf("%s %s", a.Kind, a.Ref)
	if t := strings.TrimSpace(a.Title); t != "" {
		subject += ": " + t
	}
	create := createRoom
	kind := "meet"
	if a.Kind == role.Steward {
		create = func(opts meet.CreateOptions) (*meet.State, error) {
			return meet.EnsurePermanentRoleRoom("steward", "steward", holder, opts)
		}
		kind = "meet-permanent"
		subject = "Steward"
	}
	st, err := create(meet.CreateOptions{
		Name:         roomNameFor(a),
		Topic:        subject,
		Participants: []string{holder},
		Initiator:    holder,
		Agenda:       agendaFor(a.Kind),
		// Unaddressed mail here belongs to the SEAT, not to whoever holds it
		// today. Storing the label makes the room outlive its conductor without
		// the mail going with them — the room follows the sprint's column, the
		// address follows the sprint's lease, and neither follows the agent.
		DefaultTo: a.Label(),
		// Never into the repo. A role room opens on every assume, so the default
		// (<repo>/docs/meetings/) turned lease churn into tracked files naming a
		// real host and user — in public source. See meet.OutStore.
		Out: meet.OutStore,
	})
	if err != nil {
		return nil, err
	}
	if st == nil || st.ID == "" {
		return nil, fmt.Errorf("role: meet returned no room")
	}
	// Record the convener: only the organizer may close a meet room, so a
	// successor has to be able to tell whether this one is closable by it.
	contactHolder := holder
	if kind == "meet-permanent" {
		contactHolder = ""
	}
	return &role.Contact{Kind: kind, Ref: st.ID, Room: st.Room, Topic: a.Topic(), Holder: contactHolder, Name: roomNameFor(a)}, nil
}

// roomNameFor gives a bounded role room the name of the durable work object
// whose history it carries. A sprint room is the bashy equivalent of an
// agentic tool session: managers may change, and the descriptive sprint title
// may be edited, but "sprint <id>" stays the one name of its conversation.
// Permanent steward rooms already receive their configured name through their
// separate creation path.
func roomNameFor(a role.Assignment) string {
	if a.Kind == role.Conductor {
		return fmt.Sprintf("sprint %s", strings.TrimSpace(a.Ref))
	}
	return ""
}

// EnsureDefaultTo declares the seat as an EXISTING room's default addressee.
//
// Assume sets it for rooms it opens; this is for the ones that were already
// open when the field was introduced, and for any room whose assignment is
// re-derived later. Failure is not fatal to the caller: a room without a
// default addressee still works, it just does not route unaddressed mail to the
// seat, which is exactly the state it was already in.
func EnsureDefaultTo(c *role.Contact, a role.Assignment) error {
	if c == nil || strings.TrimSpace(c.Ref) == "" {
		return nil
	}
	return setRoomDefaultTo(c.Ref, a.Label())
}

var setRoomDefaultTo = meet.SetDefaultTo

// EnsureName heals a role room opened before stable object-owned names existed.
// It changes metadata in place: the room ID and transcript remain untouched.
//
// On success it also sets c.Name, so the *role.Contact the caller already
// holds renders the stable name immediately through Contact.String() —
// without that, every surface sharing this contact would keep printing the
// old, nameless string until something reloaded it from disk. On failure
// c.Name is left untouched: a Contact must never claim a name the room store
// never actually recorded.
func EnsureName(c *role.Contact, a role.Assignment) error {
	if c == nil || strings.TrimSpace(c.Ref) == "" {
		return nil
	}
	name := roomNameFor(a)
	if name == "" {
		return nil
	}
	if err := setRoomName(c.Ref, name); err != nil {
		return err
	}
	c.Name = name
	return nil
}

var setRoomName = meet.SetName

// Release closes a bounded role room on a graceful handoff. A permanent
// steward room stays open; release clears only its current @steward routing
// alias, so the host address and transcript survive the handoff.
//
// A room that is already gone is NOT an error: the point of releasing is that
// the room ends up closed, and a successor sweeping a dead holder's room may
// have closed it first. Reporting that as a failure would make the honest path
// look broken.
func Release(c *role.Contact, holder string) error {
	if c == nil || c.Ref == "" {
		return nil
	}
	if c.Kind == "meet-permanent" {
		err := meet.ClearPermanentRoleHolder(c.Ref, "steward", holder)
		if err != nil && isGone(err) {
			return nil
		}
		return err
	}
	err := meet.Close(c.Ref, holder)
	if err != nil && isGone(err) {
		return nil
	}
	return err
}

// Sweep closes the rooms of roles whose holder is no longer live.
//
// This is the half no verb can cover. A holder that died runs nothing, so
// without a sweep its room stays open forever, advertising a channel to
// somebody who will never read it — and an unanswered room is more damaging
// than an absent one, because it consumes the time of whoever trusted it.
func Sweep(open []role.Occupied, actor string) role.SweepResult {
	res := role.SweepResult{Failed: map[string]error{}}
	for _, o := range open {
		if o.Live || o.Contact == nil || o.Contact.Ref == "" {
			continue
		}
		if err := Release(o.Contact, actor); err != nil {
			res.Failed[o.Label] = err
			continue
		}
		res.Closed = append(res.Closed, o.Label)
	}
	return res
}

func agendaFor(k role.Kind) []string {
	switch k {
	case role.Steward:
		return []string{"what is running on this host", "contention", "handoff"}
	default:
		return []string{"delivery", "blockers", "handoff"}
	}
}

// isGone reports an error that means the room is already closed or missing —
// the desired end state either way.
func isGone(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") || strings.Contains(s, "closed") ||
		strings.Contains(s, "no such")
}
