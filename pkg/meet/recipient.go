package meet

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
)

// seatTarget is an addressee after the common send-time validation.
type seatTarget struct {
	// Seat is what a room stores: an agent name, an instance UUID, or a role
	// label (the seat, never its current holder).
	Seat string
	// Party snapshots an instance addressee; nil otherwise.
	Party *bus.Party
	// Role is true when Seat is a role address.
	Role bool
	// Warning is a caveat known at resolve time, e.g. a vacant seat.
	Warning string
}

// resolveSeatTarget validates what a sender typed with the SAME resolver as
// `mb send` and `ping`, before anything is written. Ambiguous, retired and
// invalid-role names are definitive refusals; an instance resolves to its
// UUID; a role stays a role address so a handover keeps the mail. Anything
// the bus does not know as a name falls back to Meet's own roster, whose
// membership check still decides; a validation failure never does.
func resolveSeatTarget(typed string) (seatTarget, error) {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(typed), "@"))
	rec, err := bus.ResolveRecipient(t)
	if err != nil {
		// Only "no instance or role by this name" may use the legacy roster. An
		// unreadable instance store, an unknown UUID, an ambiguous, retired or
		// invalid-role name all fail closed BEFORE anything is appended.
		if bus.LegacyName(err) {
			return seatTarget{Seat: canonAgent(t)}, nil
		}
		return seatTarget{}, fmt.Errorf("meet: %w", err)
	}
	switch rec.Kind {
	case bus.TargetInstance:
		return seatTarget{Seat: strings.ToLower(rec.Party.UUID), Party: rec.Party}, nil
	case bus.TargetRole:
		return seatTarget{Seat: rec.Label, Role: true, Warning: rec.Warning}, nil
	}
	return seatTarget{Seat: canonAgent(t)}, nil
}

// instanceSeat reports whether name is an ACTIVE instance UUID; a retired one
// is an error carrying the bus's actionable retired-recipient text.
func instanceSeat(name string) (bool, error) {
	id, ok := bus.ExplicitInstanceID(name)
	if !ok {
		return false, nil
	}
	rec, err := bus.ResolveRecipient(id)
	if err != nil {
		return false, fmt.Errorf("meet: %w", err)
	}
	return rec.Kind == bus.TargetInstance, nil
}
