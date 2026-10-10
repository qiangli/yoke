package weave

// The sprint conductor lease as a coord claim.
//
// The card's Lease stays the record of truth: it is read and written under the
// existing queue.lock transaction, and nothing new is stored beside it except
// LeaseEpoch, the fencing token. This file only teaches the coord engine to
// see that lease as a claim of kind "sprint", so take, start, handoff, end,
// abort and refresh share one rule for who may act and one epoch that moves
// only when the seat changes hands.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/role"
	"github.com/qiangli/yoke/pkg/room"
)

const sprintKind = "sprint"

func init() {
	coord.RegisterKind(coord.Kind{
		Name: sprintKind, Match: coord.MatchName, TTL: SprintLeaseTTL,
		Modes: []string{coord.ModeLease},
	})
	coord.RegisterBackend(sprintKind, sprintLeaseBackend{})
}

// sprintCards are the cards inside an open queue.lock transaction. The backend
// holds no queue of its own: it reaches the card the caller already has under
// the lock, so the claim and the card can never disagree.
var sprintCards = struct {
	sync.Mutex
	m map[int64]*weaveStory
}{m: map[int64]*weaveStory{}}

func sprintLeaseRef(id int64) coord.Ref {
	return coord.Ref{Kind: sprintKind, Name: strconv.FormatInt(id, 10)}
}

func withSprintCard(s *weaveStory, fn func() error) error {
	sprintCards.Lock()
	prev, had := sprintCards.m[s.ID]
	sprintCards.m[s.ID] = s
	sprintCards.Unlock()
	defer func() {
		sprintCards.Lock()
		if had {
			sprintCards.m[s.ID] = prev
		} else {
			delete(sprintCards.m, s.ID)
		}
		sprintCards.Unlock()
	}()
	return fn()
}

type sprintLeaseBackend struct{}

func sprintCardFor(key string) (*weaveStory, error) {
	_, name, _ := strings.Cut(key, ":")
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("sprint lease key %q: %w", key, err)
	}
	sprintCards.Lock()
	defer sprintCards.Unlock()
	s := sprintCards.m[id]
	if s == nil {
		return nil, fmt.Errorf("sprint #%d lease touched outside its queue transaction", id)
	}
	return s, nil
}

// sprintLeaseRev is the stored record's Rev: 0 when the seat is vacant. A card
// written before LeaseRev existed reads as rev 1, so a held seat never looks
// like a vacant one to a stale reader.
func sprintLeaseRev(s *weaveStory) uint64 {
	if s.Lease == nil {
		return 0
	}
	return max(s.LeaseRev, 1)
}

func (sprintLeaseBackend) Load(key string) (*coord.Claim, error) {
	s, err := sprintCardFor(key)
	if err != nil || s.Lease == nil {
		return nil, err
	}
	return &coord.Claim{
		SchemaVersion: coord.SchemaVersion,
		Kind:          sprintKind,
		Resource:      strconv.FormatInt(s.ID, 10),
		Mode:          coord.ModeLease,
		Holder:        principal.Ref{Name: s.Lease.Holder},
		Epoch:         s.LeaseEpoch,
		Rev:           sprintLeaseRev(s),
		// seat() folds a dead attached process into a zero heartbeat, which the
		// engine reads as UNKNOWN — held, nothing says it breathes.
		Heartbeat: s.seat().HeartbeatAt,
		PID:       s.Lease.AttachedPID,
	}, nil
}

// CommitIfRev writes the claim onto the card, which the caller already holds
// under queue.lock — that lock is what makes the compare and the write one step.
// LeaseEpoch and LeaseRev are high-water marks that survive a release, so a
// vacated seat never re-issues an epoch or a revision.
func (sprintLeaseBackend) CommitIfRev(key string, prevRev uint64, next *coord.Claim) error {
	s, err := sprintCardFor(key)
	if err != nil {
		return err
	}
	if sprintLeaseRev(s) != prevRev {
		return coord.ErrEpochMismatch
	}
	if next == nil {
		s.Lease = nil
		return nil
	}
	if s.Lease == nil && next.Epoch <= s.LeaseEpoch {
		next.Epoch = s.LeaseEpoch + 1
	}
	if s.Lease == nil || s.Lease.Holder != next.Holder.Name {
		s.Lease = &weaveStoryLease{Holder: next.Holder.Name}
	}
	s.Lease.At = next.Heartbeat
	s.LeaseEpoch = next.Epoch
	s.LeaseRev = max(s.LeaseRev, prevRev) + 1
	next.Rev = s.LeaseRev
	return nil
}

// sprintLeaseHolderGone is positive evidence that the holder has left: it named
// the process holding the seat open, and that process is dead.
func sprintLeaseHolderGone(s *weaveStory) bool {
	return s.Lease != nil && s.Lease.AttachedPID > 0 && !room.PidAlive(s.Lease.AttachedPID)
}

// weaveStoryLeaseHeld reports who holds the seat and whether that hold blocks
// a take by someone else. A LIVE holder blocks, and so does an UNKNOWN one:
// nothing says it is gone, so the answer is to look, or to --force. Only a
// lapsed or vacant seat — or a holder whose own process is provably dead — is
// free to take. weaveStoryLeaseState folds unknown into "stale" and so lets a
// take through; callers that gate a take must ask this instead.
func weaveStoryLeaseHeld(s *weaveStory) (holder string, blocks bool) {
	seat := s.seat()
	switch seat.Live(time.Now()) {
	case role.LivenessLive:
		return seat.Holder, true
	case role.LivenessUnknown:
		return seat.Holder, !sprintLeaseHolderGone(s)
	}
	return seat.Holder, false
}

// sprintLeaseAcquire takes the seat for who through coord. A holder change
// bumps LeaseEpoch; re-taking one's own seat is a heartbeat and does not.
func sprintLeaseAcquire(s *weaveStory, who string, force bool) error {
	force = force || sprintLeaseHolderGone(s)
	err := withSprintCard(s, func() error {
		_, err := coord.AcquireRef(context.Background(), coord.Request{
			Ref: sprintLeaseRef(s.ID), Holder: principal.Ref{Name: who},
			Intent: "sprint conductor", Mode: coord.ModeLease, Force: force,
		})
		return err
	})
	var conflict *coord.Conflict
	if errors.As(err, &conflict) {
		state := string(conflict.Claim.Liveness(time.Now()))
		if state == string(role.LivenessLive) {
			state = "fresh"
		}
		return fmt.Errorf("sprint #%d lease is held by %s (%s) — coordinate, or --force to take over",
			s.ID, conflict.Claim.Holder.Name, state)
	}
	return err
}

// sprintLeaseRefresh heartbeats the recorded holder's lease with the epoch it
// holds, then records the process (0 for none) holding the seat open.
func sprintLeaseRefresh(s *weaveStory, pid int) error {
	if s.Lease == nil {
		return fmt.Errorf("sprint #%d has no lease to refresh", s.ID)
	}
	err := withSprintCard(s, func() error {
		_, err := coord.Refresh(context.Background(), sprintLeaseRef(s.ID),
			principal.Ref{Name: s.Lease.Holder}, s.LeaseEpoch)
		return err
	})
	if err != nil {
		return err
	}
	s.Lease.AttachedPID = pid
	return nil
}

// sprintLeaseRelease vacates the seat through coord; LeaseEpoch is kept.
func sprintLeaseRelease(s *weaveStory) error {
	if s.Lease == nil {
		return nil
	}
	return withSprintCard(s, func() error {
		return coord.ReleaseRef(context.Background(), sprintLeaseRef(s.ID),
			principal.Ref{Name: s.Lease.Holder}, s.LeaseEpoch)
	})
}

func sprintLeaseHeldError(id int64, s *weaveStory) error {
	holder, _ := weaveStoryLeaseHeld(s)
	state := "fresh"
	if s.seat().Live(time.Now()) == role.LivenessUnknown {
		state = string(role.LivenessUnknown)
	}
	return fmt.Errorf("sprint #%d lease is held by %s (%s) — coordinate, or --force to take over", id, holder, state)
}
