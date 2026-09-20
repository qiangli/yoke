package weave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/meet"
	"github.com/qiangli/yoke/pkg/principal"
)

// The relay half of cross-host mail (bus/remote.go is the sender half).
//
// A message to <name>@<host> rides the repo's shared session as one
// TaskEvent{Kind: "message"}: Summary is the body (mb's own 1024-byte cap
// applies), Detail is the envelope. The recipient host's inbox drains events
// addressed to it into its own board under the message's id. cloudbox holds
// the durable copy in transit; neither host needs the other to be up.

// MessageEventKind is the TaskEvent kind carrying mail. cloudbox validates
// only that a kind is non-empty, so this needs no server change.
const MessageEventKind = "message"

// messageDetail is the envelope stored in TaskEvent.Detail.
type messageDetail struct {
	Schema    string   `json:"schema"`
	ID        string   `json:"id"`
	From      string   `json:"from"`
	FromURN   string   `json:"from_urn,omitempty"`
	To        string   `json:"to"`
	Topic     string   `json:"topic,omitempty"`
	Priority  string   `json:"priority,omitempty"`
	Room      string   `json:"room,omitempty"`
	RoomTopic string   `json:"room_topic,omitempty"`
	Roster    []string `json:"roster,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	At        string   `json:"at,omitempty"`
}

// ResolveRemoteParticipant answers bus.RemoteResolve for the checkout at
// repoRoot: `<name>@<host>` must match a roster signature exactly; a bare
// name matches when exactly one participant carries it. The sender's own
// signature is never a match — mail to yourself is local.
func ResolveRemoteParticipant(ctx context.Context, repoRoot, target string) (bus.RemoteRoute, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return bus.RemoteRoute{}, bus.ErrRemoteUnknown
	}
	sc, err := EnsureRepoSession(ctx, repoRoot)
	if err != nil {
		// Not paired or no origin: there is no relay to ask, which is "no
		// participant", not a hard error — the local ladder still decides.
		return bus.RemoteRoute{}, fmt.Errorf("%w (%v)", bus.ErrRemoteUnknown, err)
	}
	roster, err := sessionRoster(ctx, sc.client, sc.pointer.TaskID)
	if err != nil {
		return bus.RemoteRoute{}, fmt.Errorf("%w (roster: %v)", bus.ErrRemoteUnknown, err)
	}
	me, _ := SessionParticipant()
	var hits []string
	for _, p := range roster.Participants {
		if strings.EqualFold(p, me) {
			continue
		}
		if strings.EqualFold(p, target) {
			hits = []string{p}
			break
		}
		if !strings.Contains(target, "@") {
			name := strings.TrimPrefix(strings.SplitN(p, "@", 2)[0], "person:")
			if strings.EqualFold(name, target) {
				hits = append(hits, p)
			}
		}
	}
	switch len(hits) {
	case 0:
		return bus.RemoteRoute{}, bus.ErrRemoteUnknown
	case 1:
		host := ""
		if i := strings.LastIndexByte(hits[0], '@'); i >= 0 {
			host = hits[0][i+1:]
		}
		return bus.RemoteRoute{Participant: hits[0], Host: host, Session: sc.pointer.TaskID}, nil
	}
	return bus.RemoteRoute{}, fmt.Errorf("%w: %s", bus.ErrRemoteAmbiguous, strings.Join(hits, ", "))
}

// SendRemoteMessage answers bus.RemoteSend: append the envelope to the
// session the route was found on.
func SendRemoteMessage(ctx context.Context, repoRoot string, m bus.RemoteMessage) (bus.RemoteReceipt, error) {
	if m.ID == "" || m.From == "" || (m.To == "" && m.Room == "") {
		return bus.RemoteReceipt{}, errors.New("remote message needs id, from, and a recipient (to) or a room")
	}
	sc, err := EnsureRepoSession(ctx, repoRoot)
	if err != nil {
		return bus.RemoteReceipt{}, err
	}
	taskID := m.Session
	if taskID == "" {
		taskID = sc.pointer.TaskID
	}
	detail, err := json.Marshal(messageDetail{
		Schema: bus.BoardSchema, ID: m.ID, From: m.From, FromURN: m.FromURN,
		To: m.To, Topic: m.Topic, Priority: m.Priority, Room: m.Room,
		RoomTopic: m.RoomTopic, Roster: m.Roster, Kind: m.Kind, At: m.At,
	})
	if err != nil {
		return bus.RemoteReceipt{}, err
	}
	ev, err := sc.client.AppendEvent(ctx, taskID, AppendEventReq{Kind: MessageEventKind, Summary: m.Body, Detail: detail})
	if err != nil {
		return bus.RemoteReceipt{}, err
	}
	return bus.RemoteReceipt{EventID: ev.ID}, nil
}

// RemoteSenderSignature answers bus.RemoteSender: this process as the roster
// knows it, plus its canonical principal (the account it acts for is the
// URN's owner once paired; the OS login is never in either).
func RemoteSenderSignature() (from, fromURN string) {
	from, _ = SessionParticipant()
	if name, ok := weaveConductorIdentity(""); ok {
		if urn := strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL")); urn != "" {
			return from, urn
		}
		return from, principal.URN(principal.KindAgent, name, "")
	}
	return from, ""
}

// RelaySharedRoomPost answers meet.RelayShared: a post in a shared board rides
// the room's session as a message carrying room:<id>. The message id is the
// one the event already carries (Origin "session:<uuid>"), so every host —
// this one included, when the feed echoes it — files it exactly once. A room
// post addressed to nobody is for the whole room; To otherwise names a seat
// as the other hosts know it.
func RelaySharedRoomPost(ctx context.Context, repoRoot string, st *meet.State, ev meet.Event) error {
	if st == nil || !st.Shared {
		return nil
	}
	id := ""
	if ev.Origin != nil {
		id = strings.TrimPrefix(ev.Origin.Source, "session:")
	}
	if id == "" {
		id = bus.NewPostID()
	}
	me, host := SessionParticipant()
	from, fromURN := RemoteSenderSignature()
	if from == "" {
		from = me
	}
	to := strings.TrimSpace(ev.To)
	if to != "" && !bus.IsRemoteAddress(to) && !strings.EqualFold(to, "all") {
		to = to + "@" + host // a local seat, qualified for the other hosts
	}
	if strings.EqualFold(to, "all") {
		to = ""
	}
	kind := ev.Kind
	if kind == "" {
		kind = "message"
	}
	m := bus.RemoteMessage{
		ID: id, From: from, FromURN: fromURN, To: to, Topic: "meet",
		Room: st.ID, RoomTopic: st.Topic, Roster: meet.SharedRoster(st, host), Kind: kind,
		At: ev.TS.UTC().Format(time.RFC3339Nano), Body: ev.Text, Session: st.Session,
	}
	_, err := SendRemoteMessage(ctx, repoRoot, m)
	return err
}
