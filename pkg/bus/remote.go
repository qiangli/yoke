// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package bus

import (
	"errors"
	"fmt"
	"strings"
)

// Cross-host mail — the email model.
//
// A message to an agent on ANOTHER host is duplicated per host and delivered
// asynchronously, the way mail is: the sender keeps the sent copy in its own
// board, a relay carries the envelope, and the recipient host's `inbox`
// delivers it into ITS board (PostMessageOnce, key = the message's ID). The
// uuid is the message's identity on every host; a seq is a local handle.
//
// bus knows nothing about the relay. The host (bashy's agentos) injects two
// seams — RemoteResolve answers "who is <name>@<host> on the session I am in"
// from the roster, RemoteSend appends the envelope to the relay — so this
// package stays free of the weave/cloudbox import, exactly as FleetNames and
// FleetSelect keep it free of the fleet catalog.
//
// The address unit is <name>@<host> for agents and people alike. The OS login
// is never part of it, and the cloudbox account (FromURN's @<email>) is
// provenance, never an address.

// RemoteRoute is a resolved cross-host addressee.
type RemoteRoute struct {
	// Participant is the roster signature, `<name>@<host>` — the address as
	// the recipient host will match it against its own readers.
	Participant string
	Host        string
	// Session names the relay (the task id) the route was found on.
	Session string
}

// RemoteMessage is the envelope handed to the relay.
type RemoteMessage struct {
	// ID is the post's universal identity, minted by the sender.
	ID string `json:"id"`
	// From is the sender's <name>@<host>; FromURN its canonical principal,
	// carrying the account it acts for.
	From    string `json:"from"`
	FromURN string `json:"from_urn,omitempty"`
	// To is the resolved participant, `<name>@<host>`.
	To       string `json:"to"`
	Topic    string `json:"topic,omitempty"`
	Priority string `json:"priority,omitempty"`
	Room     string `json:"room,omitempty"`
	Body     string `json:"-"`
	Session  string `json:"-"`
}

// RemoteReceipt is what the relay reports back. It never claims delivery:
// the relay accepted the envelope; the recipient host delivers it later.
type RemoteReceipt struct {
	// EventID is the relay's own id for the envelope (an audit handle).
	EventID string
}

// ErrRemoteAmbiguous is returned when a bare name matches several roster
// participants (the same agent name on two hosts). The sender must qualify
// with @<host>; guessing would deliver to the wrong seat and report success.
var ErrRemoteAmbiguous = errors.New("remote: name matches more than one participant; qualify it as <name>@<host>")

// ErrRemoteUnknown is returned when nothing on the session answers to the
// target. It is the remote counterpart of unresolvedTargetError and, like it,
// means NOTHING was posted anywhere.
var ErrRemoteUnknown = errors.New("remote: no participant answers to that name on the session")

// RemoteResolve maps a target — `<name>@<host>` or a bare `<name>` — to a
// route on the session the calling checkout belongs to. Nil when the host
// wired no relay. A bare name resolves only when exactly one participant
// carries it; several → ErrRemoteAmbiguous; none → ErrRemoteUnknown.
var RemoteResolve func(target string) (RemoteRoute, error)

// RemoteSend appends the envelope to the relay. Nil when the host wired no
// relay. Its error means the relay did not take the message.
var RemoteSend func(RemoteMessage) (RemoteReceipt, error)

// RemoteSender returns the sender's own signature and principal, so the
// receipt and the envelope name who sent it in the address grammar the
// recipient host understands. Nil → the local From is used as is.
var RemoteSender func() (from, fromURN string)

// IsRemoteAddress reports whether a target is spelled as a cross-host
// address. An email-shaped person handle is not (it has a dot in its host
// part and the roster will not know it); the roster is the authority, so this
// is only the cheap pre-check that decides whether to ask it FIRST.
func IsRemoteAddress(target string) bool {
	t := strings.TrimSpace(target)
	i := strings.IndexByte(t, '@')
	if i <= 0 || i == len(t)-1 {
		return false
	}
	name, host := t[:i], t[i+1:]
	if strings.ContainsAny(name, " \t/") || strings.ContainsAny(host, " \t/@") {
		return false
	}
	return true
}

// sendRemote is the cross-host half of Send. Order matters and is the
// opposite of the local path's "board first, steer second": here the RELAY
// holds the durable copy that reaches the recipient, so it is written first,
// and the sender's outbox copy (same ID) is appended only once the relay
// accepted the envelope. A relay refusal therefore posts NOTHING locally —
// a sent copy of a message nobody will receive is the receipt this package
// forbids.
func sendRemote(req SendRequest, route RemoteRoute) (SendResult, error) {
	if err := ValidateCoordinationBody(req.Body); err != nil {
		return SendResult{}, &BodyError{Err: err}
	}
	if RemoteSend == nil {
		return SendResult{}, fmt.Errorf("remote: %s resolves to %s but no relay is wired on this host", req.To, route.Participant)
	}
	from, fromURN := req.From, ""
	if RemoteSender != nil {
		if f, u := RemoteSender(); f != "" {
			from, fromURN = f, u
		}
	}
	id := NewPostID()
	msg := RemoteMessage{
		ID: id, From: from, FromURN: fromURN, To: route.Participant,
		Topic: req.Topic, Body: req.Body, Session: route.Session,
	}
	if _, err := RemoteSend(msg); err != nil {
		return SendResult{}, fmt.Errorf("failed: relay did not accept the message for %s — nothing was posted: %w", route.Participant, err)
	}
	// The outbox copy. Its To is the remote address, so no local reader is
	// obliged by it; it is the record of what was sent, with the same ID the
	// recipient host will file it under.
	seq, err := PostMessageSeq(Post{ID: id, From: req.From, To: route.Participant, Topic: req.Topic, Body: req.Body})
	if err != nil {
		return SendResult{}, fmt.Errorf("relay accepted %s (id %s) but the local outbox copy failed: %w", route.Participant, id, err)
	}
	d := Delivery{To: route.Participant, State: StateQueued, Reason: "relayed; delivered when the recipient host next reads its inbox"}
	return SendResult{Seq: seq, ID: id, Kind: SendRemote, Label: route.Participant, Deliveries: []Delivery{d}}, nil
}

// resolveRemote consults the relay's roster. It is asked FIRST for a target
// spelled as an address (the roster is the authority for `<name>@<host>`),
// and LAST for a bare name the local ladder could not place — so a name that
// exists both here and remotely keeps meaning the local one, which is what a
// sender on this host expects.
func resolveRemote(target string) (RemoteRoute, bool, error) {
	if RemoteResolve == nil {
		return RemoteRoute{}, false, nil
	}
	route, err := RemoteResolve(target)
	if err == nil {
		return route, true, nil
	}
	if errors.Is(err, ErrRemoteAmbiguous) {
		return RemoteRoute{}, false, err
	}
	return RemoteRoute{}, false, nil
}
