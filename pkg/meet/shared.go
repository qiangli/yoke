package meet

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
)

// Shared rooms — one room on many hosts (Sprint 217, story 8).
//
// A shared board's transcript is the relay's: each post is appended locally
// (the durable copy on this host) and relayed to the repo session as a
// message carrying room:<id>; a host that drains the feed files it into a
// MIRROR of the room under the same id, seating whoever the post names. The
// same happens in the other direction, so people and agents on every host
// see one conversation. Dedupe is by the message's universal id, kept on the
// event as Origin{Source: "session:<uuid>"}. meet never imports the relay:
// RelayShared is the seam bashy wires to the session client; delivery calls
// DeliverShared directly (weave imports meet).

// RelayShared sends a shared room's post to the relay. Nil = no relay wired.
var RelayShared func(st *State, ev Event) error

// SharedSessionID answers "what session does this checkout belong to" for
// `meet open --shared`. Nil = no relay wired.
var SharedSessionID func() (string, error)

// sharedOriginPrefix marks an event copied from (or relayed to) the feed.
const sharedOriginPrefix = "session:"

// relayIfShared is called after a board post landed locally. A relay refusal
// is reported loudly: the post is on this host, and nowhere else.
func relayIfShared(st *State, ev Event) error {
	if !st.Shared || RelayShared == nil {
		return nil
	}
	if err := RelayShared(st, ev); err != nil {
		return fmt.Errorf("meet: posted to %s on this host, but the relay refused it — colleagues on other hosts will not see it: %w", st.ID, err)
	}
	return nil
}

// SharedEvent is one room post as it arrives from the feed.
type SharedEvent struct {
	ID      string // the message's universal id
	RoomID  string
	Topic   string
	Session string
	Roster  []string // seats as the sender's host knows them (<name>@<host>, persons)
	From    string   // <name>@<host>
	To      string   // "" (the room) or a seat
	Kind    string   // "message" | "invite" | "kick" — defaults to message
	Body    string
	At      time.Time
}

// DeliverShared files one feed post into the local mirror of its room,
// creating the mirror on first sight. Returns false when the event was
// already filed (the uuid is on the transcript) or is this host's own post.
func DeliverShared(ev SharedEvent, myHost string) (bool, error) {
	if ev.RoomID == "" || ev.ID == "" {
		return false, fmt.Errorf("meet: shared event needs a room id and a message id")
	}
	st, err := loadState(ev.RoomID)
	if err != nil {
		if !os.IsNotExist(err) && !strings.Contains(err.Error(), "no such") && !strings.Contains(err.Error(), "not found") {
			return false, err
		}
		st, err = createMirror(ev, myHost)
		if err != nil {
			return false, err
		}
	}
	if !st.Shared {
		// A local room by the same id that was never shared: do not merge a
		// feed into it.
		return false, fmt.Errorf("meet: room %s exists here and is not shared", ev.RoomID)
	}
	// Dedupe on the message id.
	events, err := readTranscript(st.ID)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, e := range events {
		if e.Origin != nil && e.Origin.Source == sharedOriginPrefix+ev.ID {
			return false, nil
		}
	}
	// Seat what the post names, as this host knows them.
	changed := seatLocal(st, myHost, ev.Roster...)
	changed = seatLocal(st, myHost, ev.From) || changed
	if ev.To != "" {
		changed = seatLocal(st, myHost, ev.To) || changed
	}
	if changed {
		if err := st.save(); err != nil {
			return false, err
		}
	}
	kind := ev.Kind
	if kind == "" {
		kind = "message"
	}
	at := ev.At
	if at.IsZero() {
		at = nowFn()
	}
	e := Event{
		Round: st.Round, Speaker: localSeatName(ev.From, myHost), Role: string(RoleParticipant),
		Kind: kind, To: localSeatName(ev.To, myHost), Text: ev.Body, TS: at,
		Origin: &EventOrigin{Source: sharedOriginPrefix + ev.ID},
	}
	return true, AppendEvent(st.ID, e)
}

// createMirror opens the local copy of a room first seen on the feed: a
// shared board, no human of its own, no secretary, convened by nobody here.
func createMirror(ev SharedEvent, myHost string) (*State, error) {
	cwd, _ := os.Getwd()
	st := &State{
		ID: ev.RoomID, Room: assignRoom(), Topic: ev.Topic,
		Board: true, Shared: true, Session: ev.Session,
		Status: "open", Cwd: cwd, Out: OutStore, TurnTimeout: "20m", Created: nowFn(),
	}
	if strings.TrimSpace(st.Topic) == "" {
		st.Topic = "shared room " + ev.RoomID
	}
	seatLocal(st, myHost, ev.Roster...)
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if err := st.save(); err != nil {
		return nil, err
	}
	return st, nil
}

// localSeatName maps a seat as another host spelled it to this host's name
// for it: `<name>@<myHost>` is the local `<name>`; anything else stays as is.
func localSeatName(seat, myHost string) string {
	seat = strings.TrimSpace(seat)
	if myHost != "" && strings.HasSuffix(strings.ToLower(seat), "@"+strings.ToLower(myHost)) {
		return seat[:len(seat)-len(myHost)-1]
	}
	return seat
}

// seatLocal seats names in the mirror: a local registered agent joins
// Participants (it reads and posts here), everything else — a person, a
// colleague on another host — is an attendee. Returns whether anything changed.
func seatLocal(st *State, myHost string, names ...string) bool {
	changed := false
	for _, n := range names {
		n = localSeatName(n, myHost)
		if n == "" || st.seated(n) {
			continue
		}
		if _, ok := registeredAgentFn(n); ok {
			st.Participants = append(st.Participants, canonAgent(n))
		} else {
			st.Observers = append(st.Observers, n)
		}
		changed = true
	}
	return changed
}

// sharedRoster is the room's seats as other hosts should know them: local
// agents and persons qualified with this host's name, remote seats verbatim.
func sharedRoster(st *State, myHost string) []string {
	out := make([]string, 0, len(st.Participants)+len(st.Observers)+1)
	qualify := func(n string) string {
		if bus.IsRemoteAddress(n) || myHost == "" {
			return n
		}
		return n + "@" + myHost
	}
	for _, p := range st.Participants {
		out = append(out, qualify(p))
	}
	if h := strings.TrimSpace(st.Human); h != "" {
		out = append(out, qualify(h))
	}
	for _, o := range st.Observers {
		out = append(out, qualify(o))
	}
	return out
}

// SharedRoster is sharedRoster for callers outside the package (the relay).
func SharedRoster(st *State, myHost string) []string { return sharedRoster(st, myHost) }
