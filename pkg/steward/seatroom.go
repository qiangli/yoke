package steward

// The steward's room address, stored beside the seat.
//
// It is a small separate file rather than a field on the journal, because the
// journal is the AUTHORITY on who holds the seat and must stay replayable from
// nothing. A room address is neither authority nor history — it is a live
// pointer that is meaningless once the room closes, and putting it in the
// journal would mean replaying a stream of dead addresses to learn the current
// one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/qiangli/yoke/pkg/role"
)

func seatContactPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "seat-room.json"), nil
}

func saveSeatContact(c *role.Contact) error {
	p, err := seatContactPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func loadSeatContact() (*role.Contact, error) {
	p, err := seatContactPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var c role.Contact
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SeatContact is the room the seat is currently reachable on, or nil when none
// is open. Exported for the host: `bashy steward start` has to say where a
// background steward can be contacted, and `stop` has to close the same room it
// found — guessing at either would advertise a channel nobody reads.
func SeatContact() (*role.Contact, error) { return loadSeatContact() }

// EnsureRoom opens the seat's room if it is not already open, returning the same
// one-line report assumeSeatRoom does (empty when there is nothing to say).
//
// It is EnsureRoom rather than OpenRoom because a steward session may be started
// against a seat that is already held — a restart, a takeover, a supervisor that
// came back — and opening a SECOND room for one seat is worse than opening none:
// two live addresses for a singular responsibility, with no way for a caller to
// know which one is read.
func EnsureRoom(holder string) string {
	c, err := loadSeatContact()
	if err != nil || c == nil {
		return assumeSeatRoom(holder)
	}
	// A permanent room belongs to the ROLE, not to one holder. Re-ensuring it
	// lets meet refresh the configured header and returns the same durable room.
	if c.Kind == "meet-permanent" && OpenRoom != nil {
		return assumeSeatRoom(holder)
	}
	// REUSE ONLY A ROOM THIS HOLDER CAN ALSO CLOSE.
	//
	// meet lets any member post but only the ORGANIZER change the roster, so a
	// room convened by a previous steward cannot be closed by this one. Reusing
	// it looked like the tidy thing — one seat, one room — and produced the
	// worst outcome available: `steward stop` reported "room could not be
	// closed", and the host went on advertising a live channel to a seat nobody
	// held. An abandoned room is more damaging than an absent one, because it
	// costs the time of whoever trusts it.
	//
	// A live run caught this the first time a second steward started against a
	// room a previous one had opened.
	if c.Holder != "" && c.Holder != holder {
		return assumeSeatRoom(holder)
	}
	return fmt.Sprintf("  already reachable at %s\n", c.String())
}

// AssumeRoom and ReleaseRoom expose the seat's room lifecycle to the host, which
// owns the start/stop verbs but must not reimplement where the contact is stored.
func AssumeRoom(holder string) string  { return assumeSeatRoom(holder) }
func ReleaseRoom(holder string) string { return releaseSeatRoom(holder) }

// HolderName is the acting steward as a room participant — the name a human
// recognises, rendered from the canonical principal ref.
func HolderName() string { return seatHolderName() }

// Assignment is this host's steward seat as a role assignment. Its Topic() is
// the bus address anything can reach the seat on, whether or not a room is open.
func Assignment() role.Assignment { return stewardAssignment() }

func clearSeatContact() error {
	p, err := seatContactPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
