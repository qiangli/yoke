package weave

// WHICH INSTANCE holds conductor:N.
//
// The lease already records a holder NAME and a token hash. A name is not
// enough once a family can have several live conversations: two instances of
// one configuration both answer to the same name, so "held by claude-opus5"
// does not say which conversation is conducting, and a second one could take
// the seat while the first believed it still had it. The lease therefore
// records the holding INSTANCE's UUID and the digest of its owning session.
//
// # The two acceptances and the one refusal
//
// Taking a seat you already hold is a HEARTBEAT — the same instance, the same
// owning session, possibly a different pid because every turn shells out a new
// command. That is accepted, and it has to be: refusing it would make the
// conductor lose its own seat on its second command.
//
// A DIFFERENT owning session on the same instance is a competing driver for
// one conversation. That is refused, and the refusal is the point of recording
// the session at all.
//
// A different INSTANCE is a handoff. It goes through the ordinary takeover
// path (stale, free, or --force) rather than being quietly allowed here: who
// may take a seat from whom is a lease question that this file does not get to
// re-answer.
//
// # Migration
//
// A lease with no instance recorded is a LEGACY lease, and it is accepted. The
// conductors running right now took their seats before this field existed, and
// a guard that evicted them in order to enforce a newer notion of identity
// would be the one change in this story that loses live work.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/steward"
)

// sprintInstanceConflict reports a competing owning session on the instance
// holding a sprint seat.
type sprintInstanceConflict struct {
	SprintID int64
	Instance string
	Holder   string
}

func (e *sprintInstanceConflict) Error() string {
	return fmt.Sprintf(
		"sprint #%d conductor is already driven by another live session of instance %s (holder %s); hand off, or open a new instance and take the seat with --force",
		e.SprintID, e.Instance, e.Holder)
}

// sprintLeaseAccepts decides whether a caller identifying itself as
// (instance, session) may act on an existing lease without a takeover.
//
// nil lease, no recorded instance, or a caller that names no instance → the
// pre-instance behaviour, unchanged. A caller claiming the SAME instance with
// a different non-empty session digest, while that seat is LIVE → refused.
//
// `live` is why the parameter exists. The refusal is "there is a second driver
// of this conversation", and a lapsed seat has no driver at all: the conductor
// that recorded the incumbent session is gone. Refusing on the digest alone
// made a CRASHED conductor unrecoverable by the only party entitled to recover
// it — the same instance, resumed, which the story contract requires to keep
// its UUID and therefore cannot present a new one to get past the check. That
// is a worse failure than the one being prevented: the competing-driver case
// has a remedy (open another instance), while the locked-out case has none.
func sprintLeaseAccepts(id int64, lease *weaveStoryLease, live bool, instance, session string) error {
	if lease == nil || !live {
		return nil
	}
	held := strings.TrimSpace(lease.Instance)
	want := strings.TrimSpace(instance)
	if held == "" || want == "" {
		return nil
	}
	if !strings.EqualFold(held, want) {
		// A different conversation entirely — a handoff, decided by the lease
		// rules (fresh/stale/--force), not here.
		return nil
	}
	heldSession := strings.TrimSpace(lease.Session)
	callerSession := strings.TrimSpace(session)
	if heldSession == "" || callerSession == "" || heldSession == callerSession {
		return nil
	}
	return &sprintInstanceConflict{SprintID: id, Instance: held, Holder: lease.Holder}
}

// vetSprintConductor refuses a conductor appointment (take/start) for a holder
// that is an active deputy over the sprint: a deputy may activate, fence,
// judge and gate inside its scope, but it never conducts what it supervises.
// A var so tests can state the verdict without a seat.
var vetSprintConductor = defaultVetSprintConductor

// defaultVetSprintConductor is the MayConduct lookup the lease acquisition
// takes. Anything it cannot establish is an ALLOW, not a refusal: an
// unresolvable holder matches no occupancy (occupancies key on instance UUIDs,
// and take/start already refused unregistered names as claimants), and a host
// with no steward seat has no deputies to refuse.
func defaultVetSprintConductor(sprintID int64, holder, epic string) error {
	uuid, ok := sprintConductorInstanceUUID(holder)
	if !ok {
		return nil
	}
	dir, err := steward.DefaultSeatDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		return nil // no seat, no deputies
	}
	// The membership answers for the sprint being taken, which is the only
	// sprint MayConduct's single-target lookup can ask about.
	member := steward.EpicMembershipFunc(func(n int) (string, error) {
		if int64(n) == sprintID {
			return epic, nil
		}
		return "", nil
	})
	st, err := steward.Open(dir, steward.WithEpicMembership(member))
	if err != nil {
		return nil
	}
	if err := st.MayConduct(steward.InstanceRef(uuid), steward.ActTarget{Sprint: int(sprintID)}, time.Now()); err != nil {
		return fmt.Errorf("sprint #%d: %w", sprintID, err)
	}
	return nil
}

// sprintConductorInstanceUUID maps a conductor name to its live instance UUID,
// mirroring the deputy grant resolver: a UUID names its record, anything else
// must resolve to exactly one live instance.
func sprintConductorInstanceUUID(holder string) (string, bool) {
	holder = strings.TrimSpace(holder)
	if holder == "" {
		return "", false
	}
	is := fleet.NewInstanceStore("")
	if id, err := fleet.ParseInstanceUUID(holder); err == nil {
		inst, gerr := is.Get(id)
		if gerr != nil || !inst.Active() {
			return "", false
		}
		return inst.UUID, true
	}
	inst, ok, err := is.ResolveHandle(holder)
	if err != nil || !ok || !inst.Active() {
		return "", false
	}
	return inst.UUID, true
}

// sprintLeaseIdentity is the instance identity a conductor command runs under,
// resolved once from the environment so a sprint verb never has to ask for it
// again. Behind a var so a test can state it without a claimed session.
var sprintLeaseIdentity = func() (instance, session string) {
	for _, k := range []string{"BASHY_INSTANCE_UUID", "BASHY_INSTANCE"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			instance = v
			break
		}
	}
	return instance, strings.TrimSpace(os.Getenv("BASHY_SESSION_CLAIM"))
}

// stampSprintLeaseInstance records the holding instance on a lease it is
// taking. A caller with no instance identity leaves the fields empty rather
// than writing a placeholder: an empty field means "legacy or unattributed",
// and a placeholder would later compare equal to another placeholder and make
// two sessions look like one.
func stampSprintLeaseInstance(lease *weaveStoryLease, instance, session string) {
	if lease == nil {
		return
	}
	lease.Instance = strings.TrimSpace(instance)
	lease.Session = strings.TrimSpace(session)
}
