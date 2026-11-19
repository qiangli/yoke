// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package handoff

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

// ClaimKind is the coord kind a take goes through, keyed by the handoff id.
//
// Stamping ResumedAt/ResumedBy was a check-then-write with nothing between the
// read and the write: two successors picking up the same parked seat both saw
// it unclaimed, both applied the work, and the second stamp erased the first.
// The record cannot arbitrate that — it is the OUTCOME of the take, written
// after the work has already been applied to someone's tree. So the take is
// serialized through the ledger that exists for exactly this, and the loser
// gets the standard coord refusal naming the winner instead of silence.
//
// MatchName in its own domain: a handoff id denotes one record and collides
// with nothing else on the host. Pickup and effect mutations use an attached
// kernel hold so even concurrent calls by one episode serialize and process
// death releases the lock. Lease mode remains available for existing callers.
const ClaimKind = "handoff"

// ClaimTTL bounds a take that never finishes. It matches coord's own lease TTL
// and for the same reason: the window is the few seconds between acquiring and
// stamping, but a process killed inside it must not wedge the seat forever.
const ClaimTTL = 30 * time.Minute

func init() {
	coord.RegisterKind(coord.Kind{
		Name:  ClaimKind,
		Match: coord.MatchName,
		Modes: []string{coord.ModeLease, coord.ModeAttached},
		TTL:   ClaimTTL,
	})
}

// ClaimRef is the ledger address of one handoff's take. Exported so a host can
// report it (`bashy claim list` shows it like any other claim).
func ClaimRef(id string) coord.Ref { return coord.Ref{Kind: ClaimKind, Name: id} }

// Claim TAKES rec for holder: it acquires the coord claim on rec's id, confirms
// under that claim that nobody stamped first, runs apply, writes the stamp, and
// releases. It returns the stamped record.
//
// apply is the caller's side effect — restoring the working state into a tree —
// and runs INSIDE the claim, so no second taker can be applying the same patch
// to another tree at the same time. A failing apply leaves no stamp.
//
// The claim is released as soon as the stamp is durable: the STAMP is the
// record of who holds the seat, and holding the lease past the write would only
// mean a crashed taker blocks its own successor for the TTL.
//
// Idempotent for the same holder: a replayed take finds its own stamp, applies
// nothing twice, and returns the record as stored. Another holder's stamp is a
// refusal.
func Claim(ctx context.Context, dir string, rec *Record, holder principal.Ref, apply func() error) (*Record, error) {
	return claimFrom(ctx, dir, dir, rec, holder, apply)
}

func claimFrom(ctx context.Context, dir, sidecarDir string, rec *Record, holder principal.Ref, apply func() error) (*Record, error) {
	// A retired handoff is never taken: a cancelled one was explicitly killed
	// by the human, a superseded one was replaced by a newer seat of the same
	// role. Claiming either would fork the recovery — two live seats where the
	// record says there is one.
	if rec.CancelledAt != nil {
		return nil, fmt.Errorf("%s was cancelled — it must not be picked up; "+
			"`bashy resume --all` shows what replaced it", rec.ID)
	}
	if rec.SupersededAt != nil {
		return nil, fmt.Errorf("%s was superseded by %s — take the newer seat instead",
			rec.ID, rec.SupersededBy)
	}
	ref := ClaimRef(rec.ID)
	g, lock, err := acquireHandoff(ctx, coord.Request{
		Ref:    ref,
		Holder: holder,
		Mode:   coord.ModeAttached,
		Intent: "resume --claim " + rec.ID,
	})
	if err != nil {
		return nil, err
	}
	// Release on every path, cancellation included: a take that was abandoned
	// mid-flight must not leave the seat unclaimable.
	defer func() { _ = coord.ReleaseAttached(coord.DefaultDir(), g.Claim, lock) }()

	// Under the claim the stamp on disk is the truth. rec may have been read
	// before the claim was taken — long enough ago for a winner to have stamped
	// it, or for the human to have retired it, in between.
	if cur, ok := stored(dir, rec.ID); ok && retiredReason(cur) != "" {
		return nil, retiredErr(cur)
	}
	if cur, ok := stored(dir, rec.ID); ok && cur.ResumedAt != nil {
		if cur.ResumedBy != nil && coord.SameHolder(*cur.ResumedBy, holder) {
			return cur, nil
		}
		return nil, fmt.Errorf("%s was already claimed by %s at %s — a handoff is taken once; "+
			"`bashy resume --all` for the register", rec.ID, claimantName(cur), cur.ResumedAt.UTC().Format(time.RFC3339))
	}

	// Refresh the declaration under the same coordination used by CLI effect
	// mutations. A pre-lock snapshot can have missed a newly uncertain effect.
	if cur, ok := stored(dir, rec.ID); ok {
		rec = cur
	}
	tr, err := ComposeChecked(sidecarDir, rec)
	if err != nil {
		return nil, err
	}
	if len(tr.UncertainEffects()) > 0 {
		return nil, fmt.Errorf("pickup refused: external effects uncertain — reconcile before retry")
	}

	if apply != nil {
		if err := apply(); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	who := holder
	rec.ResumedAt, rec.ResumedBy = &now, &who
	if _, err := Save(dir, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// acquireHandoff tolerates the publication gap between the kernel hold and
// its diagnostic holder/ledger record. Retry only an unnamed contention error;
// never bypass the kernel hold or infer ownership from diagnostic metadata.
func acquireHandoff(ctx context.Context, req coord.Request) (coord.Grant, *lockfile.Lock, error) {
	return acquireHandoffWith(ctx, req, coord.AcquireAttachedRef)
}

func acquireHandoffWith(ctx context.Context, req coord.Request, acquire func(context.Context, coord.Request) (coord.Grant, *lockfile.Lock, error)) (coord.Grant, *lockfile.Lock, error) {
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return coord.Grant{}, nil, err
		}
		g, lock, err := acquire(ctx, req)
		if !errors.Is(err, lockfile.ErrHeld) {
			return g, lock, err
		}
		// A populated lock diagnostic is already actionable.
		if _, ok := lockfile.HeldBy(err); ok {
			return g, lock, err
		}
		select {
		case <-ctx.Done():
			return coord.Grant{}, nil, ctx.Err()
		case <-deadline.C:
			return coord.Grant{}, nil, fmt.Errorf("handoff %s is busy; holder metadata unavailable, retry later: %w", req.Ref.Name, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// stored reads the store's copy of a record. A record that arrived by scp and
// was never filed here has none — the take is still serialized by the ledger,
// and the stamp Save writes files it. A record we cannot read is likewise no
// evidence of a prior take: the ledger, not this read, keeps two takers apart.
func stored(dir, id string) (*Record, bool) {
	r, err := Load(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, false
	}
	return r, true
}

// retiredReason names why a record must never be taken, or "" when it is
// still takeable. Checked both before the take (the caller's copy) and under
// the claim (the stored copy, which may have been retired in between).
func retiredReason(r *Record) string {
	switch {
	case r.CancelledAt != nil:
		return "cancelled"
	case r.SupersededAt != nil:
		return "superseded"
	default:
		return ""
	}
}

func retiredErr(r *Record) error {
	switch retiredReason(r) {
	case "cancelled":
		return fmt.Errorf("%s was cancelled — it must not be picked up; "+
			"`bashy resume --all` shows what replaced it", r.ID)
	default:
		return fmt.Errorf("%s was superseded by %s — take the newer seat instead",
			r.ID, r.SupersededBy)
	}
}

// claimantName names whoever stamped a record, including the honest answer when
// an older record recorded the time but not the taker.
func claimantName(r *Record) string {
	if r.ResumedBy == nil {
		return "an unrecorded holder"
	}
	return refName(*r.ResumedBy)
}
