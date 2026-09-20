// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package meet

import (
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
)

// ONE NAME, ONE AGENT — on the receive side too.
//
// An agent is a singleton identity on a host: one conversation store, one
// inbox cursor, one seat. `bashy chat --agent X` refuses to start a second X
// while one is live. But a message TO X — a 1:1 from the Meet app, or
// `@X …` in a sprint room — used to run a fresh one-shot X regardless, so the
// sprint manager you addressed answered from a process that had never seen
// its sprint, its room, or the sessions it manages, while the live manager
// found the same message in its inbox and answered it a second time. The name
// pointed at two agents. That defeats the point of addressing a manager at
// all: the value of the seat IS its context.
//
// The rule is therefore: a message to an agent with a LIVE seat is delivered
// to that seat and to nothing else. The durable copy is the directed mail the
// caller already recorded (it is what the seat's unified inbox shows); the
// push, when the seat has a control socket, is bus.SteerLive — the same
// primitive `mb`/`ping` use. A seat launched outside bashy has no socket and
// reads the mail at its next turn; the sender is told exactly that. Only an
// identity NOBODY holds gets a one-shot.

// liveSeat reports the session currently holding an agent's identity, if any.
// room.Members prunes dead PIDs, so a returned card is a running process.
func liveSeat(agent string) (room.Card, bool) {
	name := canonAgent(strings.TrimSpace(strings.TrimPrefix(agent, "@")))
	if name == "" {
		return room.Card{}, false
	}
	card, ok, err := room.Find(room.AgentClaimID(name))
	if err != nil || !ok {
		return room.Card{}, false
	}
	// Find falls back to prefix and nick matches; a seat is only THIS identity.
	if !strings.EqualFold(card.ID, room.AgentClaimID(name)) && !strings.EqualFold(card.Nick, name) {
		return room.Card{}, false
	}
	return card, true
}

// seatDelivery is what the sender learns: where the message went and whether
// the seat was pushed now or will read it at its next turn.
type seatDelivery struct {
	Agent   string `json:"agent"`
	Session string `json:"session"` // the seat's mode: inbox, weave, meet-work, interactive, …
	PID     int    `json:"pid,omitempty"`
	Steered bool   `json:"steered"`          // pushed into the live session now
	Reason  string `json:"reason,omitempty"` // why it could not be pushed (it still has the mail)
}

// note renders the delivery for a human, in the terms the Meet app shows.
func (d seatDelivery) note() string {
	if d.Steered {
		return "delivered into " + d.Agent + "'s live session; it is answering now"
	}
	why := d.Reason
	if why == "" {
		why = "the session cannot be pushed"
	}
	return "delivered to " + d.Agent + "'s live " + d.Session + " session (" + why + "); it reads its mail at its next turn and answers here"
}

// deliverToLiveSeat pushes text at the seat holding agent, after the caller
// has recorded the durable copy. Best-effort in one direction only: a failed
// push costs immediacy, never the message (the mail is already there).
func deliverToLiveSeat(card room.Card, agent, text string) seatDelivery {
	d := seatDelivery{Agent: agent, Session: card.Mode, PID: card.PID}
	if d.Session == "" {
		d.Session = "live"
	}
	if strings.TrimSpace(card.CtlSock) == "" {
		d.Reason = "no control socket — launched outside bashy"
		return d
	}
	push := bus.SteerLive(agent, text)
	d.Steered = push.Steered
	d.Reason = push.Reason
	return d
}
