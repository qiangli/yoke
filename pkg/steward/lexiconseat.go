package steward

// Wiring the glossary's seat seam.
//
// pkg/lexicon must not import pkg/steward — the glossary is a projection, the
// seat is authority state, and a glossary that reaches into an authority store
// is one you cannot test without one. So lexicon declares the seam and the
// party that OWNS seats fills it, which is this package.
//
// It is registered from init() rather than from a host's wiring function on
// purpose. Every other seam in this tree defaults to nil and stays nil until
// somebody remembers to connect it — a shape that has produced six silent
// failures here in two days. Importing pkg/steward at all is the only thing a
// host can do that would make seats resolvable, so binding it at import time
// removes the step that keeps being forgotten.

import (
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/lexicon"
)

func init() {
	lexicon.SeatSource = seatsForLexicon
	bus.RegisterHostRoles(rolesForBoard)
}

// rolesForBoard makes `steward` an address on the board.
//
// The label is the bare role name, not the topic: `bashy mb send steward` is
// what somebody actually types, and requiring
// `mb send steward.dragon-u501-b683b300b1` would be the same unusable
// incantation that made the seat's own inbox go unread.
//
// This does NOT consult the seat's state. A vacant seat is still an address —
// mail sent to it waits for whoever claims it next, which is the entire point
// of addressing the role instead of its holder. Refusing to accept mail for an
// unclaimed seat would drop exactly the message that says "nobody is stewarding
// this host".
func rolesForBoard() []bus.HostRole {
	role := bus.HostRole{
		Label: "steward",
		Topic: stewardAssignment().Topic(),
	}
	if st, err := Open(""); err == nil {
		if view, err := st.Status(time.Now()); err == nil && !view.Authority.Vacant {
			role.Holder = view.Authority.Holder.Name
		}
	}
	return []bus.HostRole{role}
}

// seatsForLexicon reports this host's steward seat.
//
// Read-only and failure-tolerant: a glossary lookup must never be the thing
// that reports a broken seat store. An unreadable seat yields no entry, which
// makes `define steward` fall back to the verb — the answer it gave before
// seats existed, and a reasonable one.
//
// A VACANT seat is still reported. "There is an address here and nobody behind
// it" is the single most useful thing this can say, and it is exactly what was
// invisible when a steward ran for hours against an unclaimed seat.
func seatsForLexicon() []lexicon.SeatInfo {
	st, err := Open("")
	if err != nil {
		return nil
	}
	view, err := st.Status(time.Now())
	if err != nil {
		return nil
	}
	info := lexicon.SeatInfo{
		Topic:  stewardAssignment().Topic(),
		Label:  "steward",
		Vacant: view.Authority.Vacant,
	}
	if !view.Authority.Vacant {
		info.Holder = view.Authority.Holder.Name
	}
	return []lexicon.SeatInfo{info}
}
