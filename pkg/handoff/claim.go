// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package handoff

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

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
// with nothing else on the host. Lease mode only — a take is a detached agent
// hold, not a child-scoped one, and it is never merely announced.
const ClaimKind = "handoff"

// ClaimTTL bounds a take that never finishes. It matches coord's own lease TTL
// and for the same reason: the window is the few seconds between acquiring and
// stamping, but a process killed inside it must not wedge the seat forever.
const ClaimTTL = 30 * time.Minute

func init() {
	coord.RegisterKind(coord.Kind{
		Name:  ClaimKind,
		Match: coord.MatchName,
		Modes: []string{coord.ModeLease},
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
	ref := ClaimRef(rec.ID)
	g, err := coord.AcquireRef(ctx, coord.Request{
		Ref:    ref,
		Holder: holder,
		Mode:   coord.ModeLease,
		Intent: "resume --claim " + rec.ID,
	})
	if err != nil {
		return nil, err
	}
	// Release on every path, cancellation included: a take that was abandoned
	// mid-flight must not leave the seat unclaimable.
	defer func() { _ = coord.ReleaseRef(context.WithoutCancel(ctx), ref, holder, g.Epoch) }()

	// Under the claim the stamp on disk is the truth. rec may have been read
	// before the claim was taken — long enough ago for a winner to have stamped
	// it and released in between.
	if cur, ok := stored(dir, rec.ID); ok && cur.ResumedAt != nil {
		if cur.ResumedBy != nil && coord.SameHolder(*cur.ResumedBy, holder) {
			return cur, nil
		}
		return nil, fmt.Errorf("%s was already claimed by %s at %s — a handoff is taken once; "+
			"`bashy resume --all` for the register", rec.ID, claimantName(cur), cur.ResumedAt.UTC().Format(time.RFC3339))
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

// claimantName names whoever stamped a record, including the honest answer when
// an older record recorded the time but not the taker.
func claimantName(r *Record) string {
	if r.ResumedBy == nil {
		return "an unrecorded holder"
	}
	return refName(*r.ResumedBy)
}
