package weave

// Conductors are addresses on the board, the same way the steward seat is.
//
//	bashy mb send conductor:22 "ready for an assignment"
//
// The address is the SPRINT, never the agent conducting it. A lease changes
// hands; sent to the holder's name, mail would follow the agent rather than the
// responsibility and a handover would silently lose it. sprintTopic already
// derives the address from the sprint id for exactly this reason.

import (
	"strconv"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/role"
)

func init() {
	// RegisterHostRoles COMPOSES — pkg/steward registers the seat from its own
	// init, and a bare assignment here would silently replace it depending on
	// link order.
	bus.RegisterHostRoles(conductorRoles)
}

// conductorRoles lists the sprints on this HOST that currently have a conductor.
//
// The store is the user-global sprint board (sprintStoreDir), and that is the
// whole correction here. It first shipped reading the per-repo weave queue
// instead — weaveRepoRoot(cwd) then weaveQueueDir(root) — so it enumerated a
// store the board does not live in, and resolved zero addresses everywhere. A
// sprint holding a live, freshly-taken lease still answered "names nothing on
// this host": the seat existed, was correctly designed, was registered, and
// addressed nobody. A sprint is not a property of the checkout you happen to
// stand in — it spans repos by definition, which is what makes it a sprint
// rather than a run.
//
// Only LEASED sprints. An unleased sprint has nobody accountable for it, so an
// address would accept mail on behalf of no one — the opposite of the steward
// seat, which is a standing address for a host whether or not it is claimed.
// The distinction is real: there is always exactly one steward seat per host,
// while sprints come and go by the dozen, and minting an address per backlog
// item would bury the ones that mean something.
//
// Read-only and failure-tolerant. Resolving an address must never be the thing
// that reports a broken board: with no board there are simply no conductor
// addresses here, and no directory is created by asking.
func conductorRoles() []bus.HostRole {
	dir, err := sprintStoreDir()
	if err != nil {
		return nil
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return nil
	}
	return rolesFromQueue(q, time.Now())
}

// rolesFromQueue is the decision, split out so it is testable without a repo.
//
// It was not, when this first shipped: the queue read and the lease rule were
// one function that needed a real repo, a real queue and a live lease to
// exercise — so it went out with no test, and on this host every lease is
// weeks stale, meaning it resolved zero addresses and nothing said so. A rule
// that cannot be tested without the exact conditions it is meant to handle is
// one that ships unverified.
func rolesFromQueue(q *weaveQueue, now time.Time) []bus.HostRole {
	if q == nil {
		return nil
	}
	var out []bus.HostRole
	for _, s := range q.Stories {
		if s == nil || s.Lease == nil || s.Lease.Holder == "" {
			continue
		}
		// An expired lease is not a conductor. Accepting mail for one would
		// promise an owner that walked away half an hour ago.
		//
		// Ask the seat rather than subtracting: only LIVE earns an address.
		// A bare `now.Sub(at) > TTL` also admits the two states it cannot
		// name — a heartbeat ahead of us is negative and so never expires,
		// which would hand this sprint a permanent address.
		if s.seat().Live(now) != role.LivenessLive {
			continue
		}
		out = append(out, bus.HostRole{
			Label:  "conductor:" + strconv.FormatInt(s.ID, 10),
			Topic:  sprintTopic(s.ID),
			Holder: s.Lease.Holder,
		})
	}
	return out
}
