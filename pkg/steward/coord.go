package steward

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/policy/coord"
)

// SeatBackend projects this store's journal-backed seat into coord. Acquisition
// uses Request, so a caller must supply a grant that Store will verify. A
// missing or invalid grant is never replaced with coord's ordinary lease rule.
type SeatBackend struct {
	Store   *Store
	Request SeatRequest
}

func init() {
	coord.RegisterKind(coord.Kind{Name: "seat", Match: coord.MatchName, TTL: TTL, Modes: []string{coord.ModeLease}})
}

// RegisterSeatBackend exposes one store in coord. The scope is the sole seat key.
// Callers should unregister it when the store is no longer the active host store.
func RegisterSeatBackend(s *Store, req SeatRequest) *SeatBackend {
	b := &SeatBackend{Store: s, Request: req}
	coord.RegisterBackend("seat", b)
	return b
}

func (b *SeatBackend) key() string { return coord.Ref{Kind: "seat", Name: b.Store.Scope()}.String() }

func (b *SeatBackend) MissingEpoch(current uint64) error { return &ErrNoEpoch{Current: current} }

// AuditsForce marks the steward journal as the durable takeover audit.
func (b *SeatBackend) AuditsForce() {}

func (b *SeatBackend) checkKey(key string) error {
	if b == nil || b.Store == nil || key != b.key() {
		return fmt.Errorf("steward: seat key %q is not this store's scope", key)
	}
	return nil
}

// Load derives authority from the journal and liveness from the validated
// seat.json cache. An unknown cache leaves Heartbeat zero, so coord cannot
// mistake it for a lapsed seat.
func (b *SeatBackend) Load(key string) (*coord.Claim, error) {
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	v, err := b.Store.Status(time.Now())
	if err != nil || v.Authority.Vacant {
		return nil, err
	}
	return b.project(v), nil
}

func (b *SeatBackend) project(v View) *coord.Claim {
	c := &coord.Claim{
		SchemaVersion: coord.SchemaVersion,
		Kind:          "seat", Resource: b.Store.Scope(), Mode: coord.ModeLease,
		Holder: v.Authority.Holder, Epoch: v.Authority.Epoch,
		Rev:    (v.Authority.Epoch << 32) | v.Rev,
		Intent: v.Intent, AcquiredAt: v.Since, PID: v.PID,
	}
	if v.Liveness == LivenessLive || v.Liveness == LivenessLapsed {
		c.Heartbeat = v.Heartbeat
	}
	return c
}

// Claims lets coord.List include the one journal-backed seat.
func (b *SeatBackend) Claims() ([]*coord.Claim, error) {
	c, err := b.Load(b.key())
	if err != nil || c == nil {
		return nil, err
	}
	return []*coord.Claim{c}, nil
}

// CommitIfRev compares and mutates under steward.lock. The journal assigns the
// epoch; the heartbeat cache records revisions within one tenure.
func (b *SeatBackend) CommitIfRev(key string, prevRev uint64, next *coord.Claim) error {
	if err := b.checkKey(key); err != nil {
		return err
	}
	now := time.Now().UTC()
	return b.Store.withLock(func() error {
		v, err := b.Store.Status(now)
		if err != nil {
			return err
		}
		current := uint64(0)
		if !v.Authority.Vacant {
			current = (v.Authority.Epoch << 32) | v.Rev
		}
		if prevRev != current {
			return coord.ErrEpochMismatch
		}
		if next == nil {
			if v.Authority.Vacant {
				return &ErrNoEpoch{Current: v.Authority.Epoch}
			}
			return b.Store.releaseLocked(v.Authority.Holder, v.Authority.Epoch, "released through coord", now)
		}
		if next.Kind != "seat" || next.Resource != b.Store.Scope() || strings.TrimSpace(next.Holder.Name) == "" {
			return fmt.Errorf("steward: invalid seat claim")
		}
		if next.Epoch == 0 {
			return &ErrNoEpoch{Current: v.Authority.Epoch}
		}
		if !v.Authority.Vacant && SameHolder(v.Authority.Holder, next.Holder) && next.Epoch == v.Authority.Epoch {
			err = b.Store.heartbeatLocked(next.Holder, v.Authority.Epoch, now)
		} else {
			req := b.Request
			req.Intent = next.Intent
			if v.Authority.Vacant || v.Liveness == LivenessLapsed {
				_, err = b.Store.claimLocked(context.Background(), next.Holder, req, now)
			} else {
				_, err = b.Store.takeoverLocked(context.Background(), next.Holder, req, now)
			}
		}
		if err != nil {
			return err
		}
		stored, err := b.Load(key)
		if err != nil || stored == nil {
			return fmt.Errorf("steward: committed seat unavailable: %v", err)
		}
		*next = *stored
		return nil
	})
}
