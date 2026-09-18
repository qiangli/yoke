package weave

// THE OPEN-SPRINT REACHABILITY INVARIANT.
//
// An OPEN sprint must be reachable. That means two things at once, and neither
// is sufficient alone:
//
//	an OWNER that resolves to a real agent   — so a name can actually be addressed
//	a ROOM                                   — so there is somewhere to address it
//
// Both were nearly right already: `sprint start` opens a room automatically and
// records the owner. Two holes made the invariant advisory rather than enforced.
//
// # The room was bound to the LEASE, not to the sprint being OPEN
//
// `sprint pause` and `sprint handoff` released the lease AND closed the room,
// while the card stayed in `doing` with its box running. So an open sprint sat
// with no conductor and no room during exactly the window when an arriving
// agent most needs one — the handoff. Observed live on sprint #99: column
// `doing`, box past cutoff, an owner recorded, and no lease and no contact.
//
// The room now follows the COLUMN. An open sprint keeps its room across a pause
// or a handoff; only stop/end/abort, or a move out of an open column, closes it.
// The room is supposed to outlive the conductor — a room that exists only while
// someone is holding it is a room that is never there when you need it, which is
// the same argument `sprint start` already makes for opening one at all.
//
// Retaining it also stops churning meet minutes: closing synthesizes a summary,
// so a sprint handed off four times used to file four sets of minutes for one
// continuous conversation.
//
// # The owner was an unvalidated free string
//
// Nothing checked it against `bashy agent`, so a sprint could record an owner
// that `mb send` / `chat --agent` / `inbox --as` cannot address — and the
// coordination line printed by take/resume would name it anyway. An address
// nobody answers is worse than a missing one: it consumes the time of whoever
// trusted it before they discover there is nobody there.
//
// Registration is required at every point an owner is WRITTEN. The check is
// deliberately REGISTRATION, not liveness — a conductor is ephemeral by design (the lease is
// a heartbeat precisely because conductors die), so refusing to take a sprint
// because the roster has not seen a trace yet would refuse the recovery case.
// Liveness is REPORTED instead, where it is actionable.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"

	"github.com/qiangli/yoke/pkg/fleet"
)

// sprintUnansweredAge is how long a message addressed to a sprint's owner may
// sit unread before the sprint is reported unreachable.
//
// It is generous on purpose. An agent works in turns, so a few minutes of lag
// is normal operation, not a fault. What this catches is the failure the whole
// mechanism exists for: a request nobody is coming back to, with a sender
// blocked on an answer that will never arrive.
const sprintUnansweredAge = 30 * time.Minute

// sprintColumnOpen reports whether a column means "this sprint is live work".
//
// backlog is not yet started and done is finished; both are unattended by
// design and neither needs a room standing by.
func sprintColumnOpen(col string) bool {
	switch strings.ToLower(strings.TrimSpace(col)) {
	case "doing":
		return true
	}
	return false
}

// sprintRoomRetained reports whether a lease-releasing verb must LEAVE the room
// open. pause and handoff release the conductor; they do not end the sprint.
func sprintRoomRetained(s *weaveStory) bool {
	return s != nil && s.Contact != nil && sprintColumnOpen(s.Column)
}

// sprintOwnerRegistered reports whether a name resolves to a durable fleet
// agent — one of the canonical identities shown by `bashy agent list`.
func sprintOwnerRegistered(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	// A production sprint manager must be able to take autonomous turns. Humans
	// steer that agent through Apps, Meet, MB, or Inbox; registering a person
	// makes them addressable but does not make them runnable.
	_, kind, err := fleetCatalog().ResolvePrincipal(n)
	return err == nil && kind == fleet.KindAgent
}

func canonicalFleetAgentName(name string) (string, bool) {
	canonical, kind, err := fleetCatalog().ResolvePrincipal(strings.TrimSpace(name))
	if err != nil || kind != fleet.KindAgent {
		return "", false
	}
	return canonical, true
}

// sprintOwnerInRoster reports whether the name is currently present in the host
// room — i.e. something is actually running under it.
func sprintOwnerInRoster(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	cards, err := room.Members()
	if err != nil {
		// A roster we cannot read is not evidence of absence. Say "not found"
		// only when we actually looked; callers treat registration failure as a
		// refusal, and refusing on an unreadable roster would block the very
		// recovery path this exists to protect.
		return false
	}
	for _, c := range cards {
		// A card is addressable by its NICK (what `agents list` shows and what
		// mb/chat/inbox key on) or by its room ID. Match both: an ad-hoc worker
		// published with `track start --agent X` carries X as its nick, while a
		// bashy-managed launch is addressed by the id it joined under.
		if strings.EqualFold(strings.TrimSpace(c.Nick), n) ||
			strings.EqualFold(strings.TrimSpace(c.ID), n) {
			return true
		}
	}
	return false
}

// sprintOwnerLive reports whether the recorded owner is not merely registered
// but PRESENT. Reported, never enforced — see the package comment.
func sprintOwnerLive(name string) bool { return sprintOwnerInRoster(name) }

// validateSprintClaimant refuses to seat an owner that cannot receive a turn.
//
// This is stricter than validateSprintOwner and applies only where a sprint is
// CLAIMED — take, start, resume. The name must ADDRESS somebody. That is all.
//
// It used to additionally require a live delivery path, which meant an agent
// could not take a seat without first registering a process and then holding a
// foreground watch open for the life of the sprint. That refused the ordinary
// case — "take this sprint and read your inbox" — and it was measured standing
// in the way of this project's own conductor five times in one session: a
// pre-registration step, a foreground process a conversation-hosted agent
// cannot hold, a fuse that ended the seat twice for not running a second
// command, and a heartbeat scheduled to fire AFTER that fuse.
//
// The proxy was always the wrong test, and the comment on sprintOwnerUnanswered
// below said so before this did: a live process is a proxy; unread mail with a
// sender waiting on it is the actual failure. So claiming is not gated on a
// process, and READING YOUR INBOX is what keeps the seat live — see
// RefreshSprintOwnerActivity. An agent that reads its mail is reachable, which
// is evidence rather than a proxy for it, and it is the thing an agent already
// does at a turn boundary rather than a ritual invented for the tool.
func validateSprintClaimant(name string) error {
	return validateSprintOwner(name)
}

// sprintOwnerUnanswered reports messages addressed to the owner that nobody has
// read, and how long the oldest has waited.
//
// This is the measurement that matters. A live process is a proxy; an unread
// message with a sender waiting on it is the actual failure — and it is
// observable without a daemon, because the bus already records ReadAt.
func sprintOwnerUnanswered(name string) (count int, oldest time.Duration) {
	n := strings.TrimSpace(name)
	if n == "" {
		return 0, 0
	}
	// READ-ONLY. SnapshotInbox would open a subscription as a side effect, and
	// a consistency check run by a passer-by must not enrol somebody else's
	// name in anything.
	items, err := bus.ReadPending(n)
	if err != nil {
		return 0, 0
	}
	now := time.Now().UTC()
	for _, it := range items {
		if strings.TrimSpace(it.ReadAt) != "" {
			continue
		}
		count++
		if ts, perr := time.Parse(time.RFC3339, strings.TrimSpace(it.TS)); perr == nil {
			if age := now.Sub(ts.UTC()); age > oldest {
				oldest = age
			}
		}
	}
	return count, oldest
}

// sprintUnreadReminder is the one-line nudge appended to what a working
// conductor already runs. Empty when there is nothing waiting.
//
// It is a REMINDER, not a gate: it blocks nothing, and it names the exact
// command that clears it. The reason it exists is the delivery rule — mail
// nobody reads is mail lost, and the agent that needs to know is the one busy
// enough not to have looked. `sprint show` already reports this, but an agent
// deep in a task is not running `sprint show`; it is running `checkpoint`.
func sprintUnreadReminder(owner string) string {
	n, oldest := sprintOwnerUnanswered(owner)
	if n == 0 {
		return ""
	}
	return formatUnreadReminder(n, oldest, owner)
}

// formatUnreadReminder is the wording, split out so it is testable without a
// bus: the count and the command are the contract, not the store.
func formatUnreadReminder(n int, oldest time.Duration, owner string) string {
	return fmt.Sprintf(" — %d unread (oldest %s): read it with `bashy inbox --as %s`",
		n, oldest.Round(time.Minute), owner)
}

// validateSprintOwner refuses a conductor name that names nobody.
//
// The error points to the one canonical source of assignable identities.
func validateSprintOwner(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return fmt.Errorf("a sprint manager (the sprint owner) is required: pass --owner NAME from `bashy agent list`")
	}
	if isPlaceholderConductorName(n) {
		return fmt.Errorf("%q is a placeholder, not an agent — it addresses nobody and collides "+
			"across every sprint on this host.\n"+
			"  pass --owner NAME from `bashy agent list`", n)
	}
	_, kind, err := fleetCatalog().ResolvePrincipal(n)
	if err == nil && kind == fleet.KindAgent {
		return nil
	}
	if err == nil {
		return fmt.Errorf("sprint manager %q is a registered person, not an agent.\n"+
			"  humans manage sprints by steering a registered agent through Apps, Meet, MB, or Inbox\n"+
			"  pass --owner NAME from `bashy agent list`", n)
	}
	if errors.Is(err, fleet.ErrPrincipalAmbiguous) {
		return fmt.Errorf("sprint manager %q is ambiguous — more than one registered principal answers to it; qualify it", n)
	}
	return fmt.Errorf("sprint manager %q owns nothing here, so mb/chat/inbox cannot reach it.\n"+
		"  choose an agent from `bashy agent list`\n"+
		"  then re-run with --owner %s", n, n)
}

// isPlaceholderConductorName catches the generic fallbacks that used to be
// persisted as an owner. They are not addresses: two sprints on one host would
// both claim "conductor" and every message to it would be ambiguous.
func isPlaceholderConductorName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "conductor", "steward", "agent", "unknown", "":
		return true
	}
	return false
}

// sprintReachability is the reported state of the invariant for one sprint.
type sprintReachability struct {
	Open       bool   `json:"open"`
	Owner      string `json:"owner,omitempty"`
	Registered bool   `json:"owner_registered"`
	Live       bool   `json:"owner_live"`
	Room       string `json:"room,omitempty"`
	// Unanswered counts messages addressed to the owner that nobody has read.
	Unanswered       int      `json:"unanswered,omitempty"`
	OldestUnanswered string   `json:"oldest_unanswered,omitempty"`
	Problems         []string `json:"problems,omitempty"`
}

// sprintCheckReachability reports — never mutates — whether an open sprint can
// actually be reached. A sprint that is not open is trivially fine.
func sprintCheckReachability(s *weaveStory) sprintReachability {
	r := sprintReachability{}
	if s == nil {
		return r
	}
	r.Open = sprintColumnOpen(s.Column)
	r.Owner = strings.TrimSpace(s.Owner)
	if s.Contact != nil {
		r.Room = s.Contact.String()
	}
	if !r.Open {
		return r
	}
	switch {
	case r.Owner == "":
		r.Problems = append(r.Problems, "no owner — nobody is accountable and no name can be addressed")
	case isPlaceholderConductorName(r.Owner):
		r.Problems = append(r.Problems, fmt.Sprintf("owner %q is a placeholder, not an agent", r.Owner))
	default:
		r.Registered = sprintOwnerRegistered(r.Owner)
		// Live is REPORTED, never a problem on its own. An attached stream is a
		// proxy for attention, and an agent between turns has none while being
		// perfectly reachable — flagging that produced a board where a working
		// conductor read UNREACHABLE on a timer. The problem below is the thing
		// that actually hurts somebody: mail nobody has read.
		r.Live = sprintInboxDeliveryLive(r.Owner)
		if !r.Registered {
			r.Problems = append(r.Problems, fmt.Sprintf(
				"owner %q is not in `bashy agent` — mb/chat/inbox cannot reach it", r.Owner))
		}
		// UNANSWERED MAIL IS THE REAL FAILURE. Everything above is about
		// whether somebody COULD answer; this is whether anybody DID. A sender
		// blocked on a question nobody read waits forever, and until now
		// nothing on any surface said so.
		if n, oldest := sprintOwnerUnanswered(r.Owner); n > 0 {
			r.Unanswered, r.OldestUnanswered = n, oldest.Round(time.Minute).String()
			if oldest > sprintUnansweredAge {
				r.Problems = append(r.Problems, fmt.Sprintf(
					"%d unanswered message(s) for %q, oldest %s — somebody is waiting; read with `bashy inbox --as %s`",
					n, r.Owner, oldest.Round(time.Minute), r.Owner))
			}
		}
	}
	if s.Contact == nil {
		r.Problems = append(r.Problems, "no room — there is nowhere to raise a request")
	}
	sort.Strings(r.Problems)
	return r
}

// renderSprintReachability prints the invariant's problems, or nothing at all
// when it holds. Silence means healthy; it never prints a reassuring line,
// because a surface that says "ok" is one more thing to keep true.
func renderSprintReachability(w interface{ Write([]byte) (int, error) }, s *weaveStory) {
	r := sprintCheckReachability(s)
	for _, p := range r.Problems {
		fmt.Fprintf(w, "  UNREACHABLE: %s\n", p)
	}
}

// ownerErr reports why a name does not resolve to one owner, for the message
// only — the decision itself stays with sprintOwnerRegistered.
func ownerErr(name string) error {
	_, _, err := fleetCatalog().ResolvePrincipal(name)
	return err
}
