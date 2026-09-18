package weave

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/role"
)

// SprintClaimIdentity resolves the exact durable address a start/take command
// will claim. Bashy's external-harness adapter uses it before mutation so the
// foreground inbox stream is already live when claim validation runs.
func SprintClaimIdentity(id int64, explicit string, _ bool) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit == "" {
		return "", fmt.Errorf("--owner is required, and there are two cases.\n" +
			"  YOU are the manager (you were told to take this sprint): use your OWN name.\n" +
			"    `bashy agent list` to find it, or register one:\n" +
			"    `bashy agent add <name> --tool <tool> --model <model>`\n" +
			"    then re-run with --owner <name>. That is not a guess; it is your identity.\n" +
			"  You are appointing SOMEONE ELSE: choose a NAME from `bashy agent list`\n" +
			"    and ask the user rather than guessing on their behalf.")
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return "", err
	}
	q, err := readWeaveQueue(dir)
	if err != nil {
		return "", err
	}
	s := findWeaveStory(q, id)
	if s == nil {
		return "", fmt.Errorf("sprint #%d not found", id)
	}
	if err := validateSprintOwner(explicit); err != nil {
		return "", err
	}
	canonical, _, err := fleetCatalog().ResolvePrincipal(explicit)
	if errors.Is(err, fleet.ErrPrincipalAmbiguous) {
		return "", fmt.Errorf("sprint manager %q is ambiguous — more than one registered principal answers to it; qualify it", explicit)
	}
	if err != nil {
		return "", fmt.Errorf("sprint manager %q owns nothing here — %s", explicit, fleet.UnknownPrincipalHint(explicit))
	}
	return canonical, nil
}

// RefreshSprintManagerLease records proven activity by the current sprint
// manager. Attached inbox acknowledgement uses this so a manager that is
// actively consuming its delivery stream cannot simultaneously age stale.
func RefreshSprintManagerLease(id int64, owner string) error {
	return writeSprintManagerLease(id, owner, 0)
}

// HoldSprintManagerLease is the beat of a process that is holding the seat
// open in the foreground, and it records that process. Use it instead of
// RefreshSprintManagerLease whenever the caller will still be running when the
// heartbeat it just wrote is read back — see weaveStoryLease.AttachedPID for
// why the difference is load-bearing rather than cosmetic.
func HoldSprintManagerLease(id int64, owner string, pid int) error {
	if pid < 0 {
		pid = 0
	}
	return writeSprintManagerLease(id, owner, pid)
}

// ReleaseSprintManagerLease stands the seat down when the stream that was
// holding it open detaches.
//
// WITHOUT THIS, DETACHING WAS INVISIBLE. The attached watch is documented as
// the seat being held for as long as the process runs, but ending it wrote
// nothing: the last beat stayed on the lease, so `bashy agent` kept reporting
// a healthy conductor for the remainder of the TTL with the process provably
// gone. Symmetry is the fix — a beat that claims the seat on attach must be
// answered by a stand-down on detach.
//
// The HOLDER is deliberately kept. Clearing it would erase who had the seat
// and hand a successor a blank record; what is retracted is the CLAIM TO BE
// BREATHING, which is the heartbeat alone. A conductor that detaches the
// stream but keeps working simply reappears on its next inbox read — the same
// evidence rule as everywhere else.
//
// Holder-checked like every other lease write, so a watch that exits because
// somebody TOOK the seat cannot stand down the new occupant on its way out.
func ReleaseSprintManagerLease(id int64, owner string) error {
	dir, err := sprintStoreDir()
	if err != nil {
		return err
	}
	owner = strings.TrimSpace(owner)
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		if s.Lease == nil || !strings.EqualFold(s.Lease.Holder, owner) {
			return fmt.Errorf("sprint #%d is not held by %s", id, owner)
		}
		s.Lease.At = time.Time{}
		s.Lease.AttachedPID = 0
		s.UpdatedAt = time.Now().UTC()
		return nil
	})
}

func writeSprintManagerLease(id int64, owner string, pid int) error {
	dir, err := sprintStoreDir()
	if err != nil {
		return err
	}
	owner = strings.TrimSpace(owner)
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		if s.Lease == nil || !strings.EqualFold(s.Lease.Holder, owner) {
			return fmt.Errorf("sprint #%d is not held by %s", id, owner)
		}
		now := time.Now().UTC()
		s.Lease.At = now
		s.Lease.AttachedPID = pid
		s.UpdatedAt = now
		return nil
	})
}

// RefreshSprintOwnerActivity records that this agent just READ ITS INBOX, on
// every sprint it owns.
//
// This is the whole liveness model, and it replaces holding a process open.
// Reading your mail is the act that proves both things a seat needs to be
// true — the agent is running, and it is paying attention to this channel —
// and it is something an agent already does at a turn boundary rather than a
// ritual invented for the tool. A held foreground stream proved neither: it
// survived an agent that had stopped reading, and it died on an agent that had
// not.
//
// Best-effort and silent. An inbox read must never fail because of bookkeeping
// it did not ask for, and an agent that owns no sprint is the ordinary case.
// Only leases this exact name already holds are touched, so reading mail can
// never take a seat from somebody else.
func RefreshSprintOwnerActivity(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return
	}
	// Read first, without the lock: the overwhelmingly common case is an agent
	// that holds no lease, and that must cost nothing.
	q, err := readWeaveQueue(dir)
	if err != nil || q == nil {
		return
	}
	var held bool
	for _, s := range q.Stories {
		if s != nil && s.Lease != nil && strings.EqualFold(s.Lease.Holder, name) {
			held = true
			break
		}
	}
	if !held {
		return
	}
	now := time.Now().UTC()
	_ = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		for _, s := range q.Stories {
			if s != nil && s.Lease != nil && strings.EqualFold(s.Lease.Holder, name) {
				s.Lease.At = now
				// An inbox read is an EVENT, not a tenancy: this command exits
				// in a moment, so leaving a previous holder's pid on the lease
				// would make the seat die with a process the reader never was.
				s.Lease.AttachedPID = 0
				s.UpdatedAt = now
			}
		}
		return nil
	})
}

// SprintOwnerLastRead is when this owner last read its inbox, as recorded by
// the lease heartbeat. ok is false when the name holds no sprint lease.
func SprintOwnerLastRead(name string) (at time.Time, ok bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.Time{}, false
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return time.Time{}, false
	}
	q, err := readWeaveQueue(dir)
	if err != nil || q == nil {
		return time.Time{}, false
	}
	for _, s := range q.Stories {
		if s != nil && s.Lease != nil && strings.EqualFold(s.Lease.Holder, name) {
			if at.IsZero() || s.Lease.At.After(at) {
				at, ok = s.Lease.At, true
			}
		}
	}
	return at, ok
}

// LiveSprintManagers names every agent currently HOLDING a sprint manager
// seat on this host — the resolution behind `bashy mb send --role conductor`.
//
// It exists because every other mb selector (band, tool, provider, family,
// version) is a property of an agent's BINDING, and none of them can express
// "is managing a sprint right now". A manager wanting exactly its peers had to
// broadcast to everyone — over-broad, and capped for any reader who has not
// declared the concern — or hardcode names that rot the moment a lease moves.
//
// LIVE ONLY, and that is the whole point rather than a detail:
//
//   - an UNOWNED sprint names nobody;
//   - a STALE lease names a conductor that died without handing off, so its
//     holder is not managing anything and never reads what you send.
//
// Addressing either produces mail nobody answers, which is worse than a
// broadcast because the sender believes it reached someone. Staleness is
// judged by the same SprintLeaseTTL the board and `bashy agent` grade leases
// with — exported for exactly this reason, so a second opinion about who is
// live cannot drift from the first.
//
// Names come back deduplicated (one agent may hold several sprints) and
// sorted, so the delivery report is stable.
func LiveSprintManagers() ([]string, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return nil, err
	}
	q, err := readWeaveQueue(dir)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	seen := map[string]bool{}
	var out []string
	for _, s := range q.Stories {
		if s == nil || s.Lease == nil {
			continue // unowned: names nobody
		}
		holder := strings.TrimSpace(s.Lease.Holder)
		if holder == "" {
			continue
		}
		if s.seat().Live(now) != role.LivenessLive {
			continue // not live: the holder is not managing anything
		}
		if seen[holder] {
			continue
		}
		seen[holder] = true
		out = append(out, holder)
	}
	sort.Strings(out)
	return out, nil
}
