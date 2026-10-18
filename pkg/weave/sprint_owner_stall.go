package weave

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/role"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/steward"
)

// sprintOwnerStalled reports whether a delivery failure means the managed
// session is HUNG rather than absent or refused.
//
// Sprint 457: the manager's control socket kept accepting connections while
// nothing answered them, so the send-once instruction died on the ack read
// deadline ("read unix …/ctl.sock: i/o timeout") and the lease went stale with
// no alert. A dial failure is a dead session and the launcher already handles
// it; a deadline after a successful dial is the one shape no other path
// reports. foreman's control socket answers mid-turn by design, so a busy
// manager is not mistaken for a hung one.
func sprintOwnerStalled(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// recoverStalledSprintOwner reports a stalled manager and recovers it through
// the existing owner lifecycle — StopSprintOwner, then StartSprintOwner with
// the same instruction — under the per-sprint lifecycle lock the caller holds.
//
// The report goes to the sprint thread as a stage entry (so every seated
// conductor sees it on the board and the host event log carries it) and to the
// supervisor that was instructing the manager, because the manager itself is
// the one party that cannot raise the alarm. A relaunch happens only after the
// hung session has provably stopped, so two managers never hold the seat.
func recoverStalledSprintOwner(ctx context.Context, s *weaveStory, owner, brief, cwd string, duration time.Duration, cause error) (string, error) {
	id := s.ID
	stalled := fmt.Sprintf("manager %s stalled: its managed session accepted the connection but did not acknowledge the instruction (%v); %s",
		owner, cause, sprintOwnerHeartbeat(s))
	if isCurrentOwnerManagedSession(id, s, owner) {
		reportSprintOwnerStall(id, owner, stalled+"; not restarted: the caller is inside that session")
		return "", fmt.Errorf("%w; the managed session is stalled and cannot restart itself", cause)
	}
	if StopSprintOwner == nil {
		reportSprintOwnerStall(id, owner, stalled+"; not restarted: managed stop is not wired")
		return "", fmt.Errorf("%w; the managed session is stalled and this host cannot stop it", cause)
	}
	if err := retireSprintOwnerSession(ctx, id, owner, cwd); err != nil {
		reportSprintOwnerStall(id, owner, fmt.Sprintf("%s; recovery failed: the stalled session could not be stopped (%v); no replacement was launched", stalled, err))
		return "", fmt.Errorf("%w; recovery refused: the stalled session could not be stopped: %v", cause, err)
	}
	note, _, err := ensureSprintOwnerSession(ctx, id, owner, brief, cwd, duration)
	if err != nil {
		reportSprintOwnerStall(id, owner, fmt.Sprintf("%s; recovery failed: stopped, but the relaunch failed (%v)", stalled, err))
		return "", fmt.Errorf("stalled manager session stopped but relaunch failed: %w", err)
	}
	reportSprintOwnerStall(id, owner, stalled+"; recovered: stopped and relaunched"+note)
	return "; recovered stalled manager session" + note, nil
}

// sprintOwnerHeartbeat states the lease heartbeat the report is judged
// against, so a reader can tell a fresh stall from a seat that went silent
// hours ago.
func sprintOwnerHeartbeat(s *weaveStory) string {
	at := s.seat().HeartbeatAt
	if at.IsZero() {
		return "lease heartbeat unknown"
	}
	return fmt.Sprintf("last lease heartbeat %s ago", roundDur(time.Since(at)))
}

// reportSprintOwnerStall is best effort: the delivery error is the primary
// signal, and a failed report must not hide it.
//
// The notice goes to the sprint's SUPERVISOR as the steward registry names it,
// not to whoever happened to run instruct; with none registered the thread
// says so instead of guessing a recipient.
func reportSprintOwnerStall(id int64, owner, message string) {
	reportSprintOwnerStallIf(id, owner, message, nil)
}

// reportSprintOwnerStallIf reports only when claim, run under the queue lock,
// accepts the current card; it returns whether a report was recorded.
func reportSprintOwnerStallIf(id int64, owner, message string, claim func(*weaveStory) bool) bool {
	dir, err := sprintStoreDir()
	if err != nil {
		return false
	}
	var to string
	reported := false
	_ = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil || (claim != nil && !claim(s)) {
			return nil
		}
		reported = true
		var ok bool
		if to, ok = sprintSupervisorAddress(id, s.Epic, owner); !ok {
			to = ""
			message += fmt.Sprintf("; no supervisor notified: no steward or deputy seat is held over sprint #%d", id)
		}
		weaveStoryAppend(s, "supervisor", kindStage, message)
		return nil
	})
	if to == "" {
		return reported
	}
	message = fmt.Sprintf("sprint #%d %s", id, message)
	if len(message) > 1000 {
		message = message[:1000]
	}
	_ = weaveSupervisionSend(to, message)
	return reported
}

// silentSprintOwnerMarker tags the heartbeat report so one lapse is reported once.
const silentSprintOwnerMarker = "silent:"

// checkSilentSprintManagers is the heartbeat half of stall detection, run by
// the same immune tick as the quota watchdog: a doing sprint whose managed
// manager session is still running (its room card is live) while the sprint
// lease heartbeat has lapsed is a hung manager that nobody is instructing —
// the Sprint 457 shape, where the lease went stale for two hours unnoticed.
// It is reported once per heartbeat to the thread and the supervisor; recovery
// stays with the owner lifecycle (sprint instruct restarts a stalled session).
func checkSilentSprintManagers(now time.Time, dryRun bool) ([]quotaImmuneAction, error) {
	store, err := sprintStoreDir()
	if err != nil {
		return nil, err
	}
	q, err := loadWeaveQueue(store)
	if err != nil {
		return nil, err
	}
	var actions []quotaImmuneAction
	for _, snapshot := range q.Stories {
		if snapshot.Column != "doing" || snapshot.Lease == nil {
			continue
		}
		live := snapshot.seat().Live(now)
		owner := snapshot.Lease.Holder
		if live == role.LivenessLive || !sprintOwnerManagedCardLive(owner, snapshot.ID) || silentSprintOwnerReported(snapshot) {
			continue
		}
		action := quotaImmuneAction{Kind: "manager-silent", Sprint: snapshot.ID, Level: string(live), Detail: sprintOwnerHeartbeat(snapshot), DryRun: dryRun}
		if !dryRun {
			message := fmt.Sprintf("manager %s %s its managed session is running but the sprint lease is %s (%s); restart it through the owner lifecycle: sprint instruct recovers a stalled session",
				owner, silentSprintOwnerMarker, live, sprintOwnerHeartbeat(snapshot))
			if !reportSprintOwnerStallIf(snapshot.ID, owner, message, func(s *weaveStory) bool {
				return s.Lease != nil && s.Lease.Holder == owner && !silentSprintOwnerReported(s)
			}) {
				continue
			}
		}
		actions = append(actions, action)
	}
	return actions, nil
}

// silentSprintOwnerReported reports whether the current lease heartbeat has
// already been reported silent.
func silentSprintOwnerReported(s *weaveStory) bool {
	for i := len(s.Thread) - 1; i >= 0; i-- {
		c := s.Thread[i]
		if s.Lease != nil && c.At.Before(s.Lease.At) {
			return false
		}
		if c.Author == "supervisor" && c.Kind == kindStage && strings.Contains(c.Body, silentSprintOwnerMarker) {
			return true
		}
	}
	return false
}

// sprintOwnerManagedCardLive mirrors the host's managed-session proof: the
// holder's room card is a live Foreman session for exactly this sprint.
var sprintOwnerManagedCardLive = func(owner string, sprint int64) bool {
	card, live, err := room.Find(room.AgentClaimID(owner))
	return err == nil && live && card.Mode == "foreman" && card.Task == fmt.Sprintf("sprint-%d-manager", sprint)
}

// sprintSupervisorAddress is the board address of whoever supervises a sprint:
// the held deputy:<scope> seat that covers it, else the held steward seat. A
// var so tests can name the supervisor without a seat.
var sprintSupervisorAddress = defaultSprintSupervisorAddress

// defaultSprintSupervisorAddress reads the steward registry; it never creates
// a seat. A seat held by the stalled owner itself is not a supervisor — the
// hung party cannot answer its own alarm.
func defaultSprintSupervisorAddress(sprintID int64, epic, owner string) (string, bool) {
	dir, err := steward.DefaultSeatDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return "", false
	}
	if _, err := os.Stat(dir); err != nil {
		return "", false
	}
	st, err := steward.Open(dir)
	if err != nil {
		return "", false
	}
	now := time.Now()
	ownerUUID, _ := sprintConductorInstanceUUID(owner)
	if occs, err := st.DeputyOccupancies(now); err == nil {
		for _, o := range occs {
			if o.Vacant || (ownerUUID != "" && o.Holder == ownerUUID) {
				continue
			}
			if slices.Contains(o.Scope.Sprints, int(sprintID)) ||
				(strings.TrimSpace(epic) != "" && strings.EqualFold(strings.TrimSpace(o.Scope.Epic), strings.TrimSpace(epic))) {
				return o.Address, true
			}
		}
	}
	view, err := st.Status(now)
	if err != nil || view.Authority.Vacant || strings.EqualFold(strings.TrimSpace(view.Authority.Holder.Name), strings.TrimSpace(owner)) {
		return "", false
	}
	return "steward", true
}
