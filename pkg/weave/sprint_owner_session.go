package weave

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// StartSprintOwner brings up a durable session for a sprint's owner, so the
// seat is REACHABLE from the moment the sprint starts rather than from whenever
// somebody first happens to write to it.
//
// # Why a seam and not an import
//
// The session is a foreman session (pkg/foreman), and foreman pulls pkg/dag.
// meet already solves this exact shape with injected host seams —
// StartRoomSecretary, StartPermanentRole, ValidateRoomSecretary — wired once in
// bashy's agentos. Following that keeps weave's import graph where it is and
// puts the composition in the one place that already does composition.
//
// A nil seam means the host did not wire managed delivery. It is an error only
// when the operator explicitly supplied an instruction.
var StartSprintOwner func(context.Context, SprintOwnerRequest) (SprintOwnerSession, error)

// StopSprintOwner tears that session down. An owner session outliving its
// sprint is a leak with a model attached.
var StopSprintOwner func(context.Context, SprintOwnerRequest) error

// SprintOwnerRequest is what the host needs to run, or stop, an owner.
type SprintOwnerRequest struct {
	Sprint int64
	// Owner is the fleet agent name. It is already validated as registered by
	// the time this is called.
	Owner string
	// Brief is the operator's opening instruction. Empty means no managed
	// session is requested.
	Brief    string
	Cwd      string
	Duration time.Duration
}

// SprintOwnerSession identifies the managed session that accepted the
// instruction. Transport is re-measured by weave before success is reported.
type SprintOwnerSession struct {
	ID        string
	Reused    bool
	Transport room.OwnerTransport
}

// ensureSprintOwnerSession delivers one explicit instruction to a managed
// owner session. An instruction is a requested side effect, so failure is an
// error rather than an advisory attached to an otherwise-successful start.
func ensureSprintOwnerSession(ctx context.Context, id int64, owner, brief, cwd string, duration time.Duration) (note string, launched bool, err error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || strings.TrimSpace(brief) == "" {
		return "", false, nil
	}
	if StartSprintOwner == nil {
		return "", false, fmt.Errorf("managed sprint-manager launch is not wired")
	}
	req := SprintOwnerRequest{Sprint: id, Owner: owner, Brief: brief, Cwd: cwd, Duration: duration}
	session, err := StartSprintOwner(ctx, req)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(session.ID) == "" {
		return "", !session.Reused, fmt.Errorf("manager launcher returned no managed session id")
	}
	transport, why := room.OwnerTransportFor(owner)
	if transport != room.TransportManaged {
		return "", !session.Reused, fmt.Errorf("manager session %s did not publish managed inbox delivery for %s: %s", session.ID, owner, why)
	}
	verb := "started"
	if session.Reused {
		verb = "reused"
	}
	return fmt.Sprintf("; %s manager session %s (%s)", verb, session.ID, transport), !session.Reused, nil
}

// releaseSprintOwnerSession stops the owner's session when the sprint is over.
// Best effort by design: a sprint must be able to end even if the teardown
// cannot, and a failure to stop is reported rather than blocking the end.
func releaseSprintOwnerSession(ctx context.Context, id int64, owner, cwd string) string {
	owner = strings.TrimSpace(owner)
	if owner == "" || StopSprintOwner == nil {
		return ""
	}
	if err := retireSprintOwnerSession(ctx, id, owner, cwd); err != nil {
		return fmt.Sprintf("; could not stop %s's session (%v)", owner, err)
	}
	return fmt.Sprintf("; stopped %s's session", owner)
}

// retireSprintOwnerSession is the ownership-transfer gate. A deterministic
// sprint session cannot remain authoritative under the old identity after the
// record names a new one, so transfer refuses unless the old session is stopped.
func retireSprintOwnerSession(ctx context.Context, id int64, owner, cwd string) error {
	owner = strings.TrimSpace(owner)
	if owner == "" || StopSprintOwner == nil {
		return nil
	}
	return StopSprintOwner(ctx, SprintOwnerRequest{Sprint: id, Owner: owner, Cwd: cwd})
}
