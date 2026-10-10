// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package room

// ONE LIVE OWNING SESSION per claim id, decided on the session digest rather
// than on a pid.
//
// # Why Join is not enough
//
// Join refuses a second claimant whose pid is different and alive. That is the
// right rule for a process that OWNS its card for its whole life, and the
// wrong rule for an agent session, because an agent session is not one
// process. A Claude or Codex harness runs a long-lived conversation and shells
// out a short-lived `bashy` command per turn; every one of those commands has
// its own pid and every one of them is the SAME owning session. Under Join's
// rule the first such command claims the identity and the second is told
// somebody else already is it.
//
// The fix is to claim on the thing that actually identifies the session. Cards
// already carry SessionClaim — a one-way digest of the stable tool-session
// identifier the harness and its children inherit (see bus.HashSessionClaim).
// Equal digest means same session: accept, and let the child command refresh
// the card. Different digest while the incumbent is live means a COMPETING
// session: refuse.
//
// # What this makes true across repositories
//
// The room is host-global (Dir()), so the claim does not live in a checkout.
// Two sessions in two clones of two different repos that try to drive one
// instance land on one card here, and the second is refused — which is the
// collision reported in #1245. A per-repo claim would have let both believe
// they were sole owner, and two owners of one conversation is how a context
// gets interleaved by two models at once.
//
// # What it is not
//
// Not a credential. The digest is inherited through the process environment,
// so the OS boundary remains the trust boundary exactly as Card.OwnerPID
// documents. It proves continuity, not authorization.

import (
	"errors"
	"fmt"
	"os"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// ErrNoSessionClaim reports a claim attempt with no session digest.
//
// It is refused rather than defaulted. An empty digest would compare equal to
// every other empty digest, so defaulting would silently make all unattributed
// sessions one session — the opposite of what a singleton claim is for.
var ErrNoSessionClaim = errors.New("room: a session claim requires Card.SessionClaim")

// ClaimSession claims an id for ONE owning session.
//
// Granted when the id is free, when its incumbent card is stale (the owning
// process is gone — reading is the reconciliation here as everywhere else in
// this package), or when the incumbent carries the SAME session digest. In
// that last case the card is updated in place and Joined is preserved, so a
// per-turn child command does not keep resetting the session's start time.
//
// Refused with *ErrLive when a live incumbent carries a different digest.
func ClaimSession(c Card) error {
	if c.SessionClaim == "" {
		return ErrNoSessionClaim
	}
	if c.ID == "" {
		return fmt.Errorf("room: a session claim requires Card.ID")
	}
	if c.PID == 0 {
		c.PID = os.Getpid()
	}
	dir, err := membersDir()
	if err != nil {
		return err
	}
	claimLock, err := lockMemberClaims(lockfile.Holder{
		Name: c.ID, PID: c.PID, Intent: "claim owning session",
	}, false)
	if err != nil {
		return fmt.Errorf("room: serialize session claim: %w", err)
	}
	defer claimLock.Release()

	path := memberPath(dir, c.ID)
	prior, ok := readCard(path)
	legacy := ""
	if !ok {
		if candidate, safe := legacyMemberPath(dir, c.ID); safe {
			legacy = candidate
			prior, ok = readCard(candidate)
		}
	}
	if ok && sessionCardAlive(prior) {
		// A live incumbent with a DIFFERENT digest is the competing session
		// this function exists to refuse. A live incumbent with no digest at
		// all is also a refusal: it predates this contract, so there is no
		// evidence it is us, and guessing in favour of the newcomer would hand
		// an in-flight conversation to a second driver.
		if prior.SessionClaim != c.SessionClaim {
			// Name the HOLDER, not the writer. A refusal that reports the
			// incumbent's dead per-turn child reads as a stale-card bug to
			// whoever was refused, so the operator kills a card that is
			// legitimately held instead of looking for the other session.
			return &ErrLive{ID: c.ID, PID: holderPID(prior)}
		}
		if c.Joined == "" {
			c.Joined = prior.Joined
		}
		if c.OwnerPID == 0 {
			c.OwnerPID = prior.OwnerPID
			// Normalize the writer pid onto the inherited owner. A child
			// command that does not know the harness pid would otherwise leave
			// a card whose PID is about to exit, and every pre-contract reader
			// on the host judges a card by PID alone.
			if c.OwnerPID != 0 {
				c.PID = c.OwnerPID
			}
		}
	}
	if c.Joined == "" {
		c.Joined = now()
	}
	c.Updated = now()
	if err := writeCardFile(path, c); err != nil {
		return err
	}
	if legacy != "" && legacy != path {
		_ = os.Remove(legacy)
	}
	return Emit(Event{Type: EventJoin, Actor: c.Principal, Target: c.ID, Body: c.Binding})
}

// sessionCardAlive reports whether an incumbent session card is still held by
// a live owner. It is the shared holder rule (card_holder.go) under the name
// this file's reasoning uses; the two must not be allowed to drift, because
// every time a reader and the claim disagreed about who was alive, the claim
// lost.
func sessionCardAlive(c Card) bool { return cardAlive(c) }

// ReleaseSession gives up a claim held by this session.
//
// It checks the DIGEST rather than the pid, for the same reason ClaimSession
// does: the process that releases a session's claim is usually not the process
// that took it. Releasing a claim you do not hold is a no-op, not an error —
// the same forgiving shape as Leave, and for the same reason: every caller
// pairs claim with a deferred release, and that defer runs even when the claim
// was refused.
//
// It takes the SAME lock as ClaimSession, and re-reads under it. Without that,
// the read that proved the card was ours and the remove that acted on it
// straddled a window in which another session could legitimately claim the id
// — a stale incumbent is reclaimable, so this is a real sequence — and the
// release then deleted the NEW owner's card, leaving the id unowned while that
// owner believed it held the claim.
func ReleaseSession(id, sessionClaim string) {
	if id == "" || sessionClaim == "" {
		return
	}
	dir, err := membersDir()
	if err != nil {
		return
	}
	claimLock, err := lockMemberClaims(lockfile.Holder{
		Name: id, PID: os.Getpid(), Intent: "release owning session",
	}, false)
	if err != nil {
		return
	}
	defer claimLock.Release()

	path := memberPath(dir, id)
	if _, ok := readCard(path); !ok {
		if legacy, safe := legacyMemberPath(dir, id); safe {
			path = legacy
		}
	}
	prior, ok := readCard(path)
	if !ok || prior.SessionClaim != sessionClaim {
		return
	}
	_ = os.Remove(path)
	_ = Emit(Event{Type: EventLeave, Target: id})
}

// SessionOwner reports the live owning session's card for an id.
func SessionOwner(id string) (Card, bool) {
	dir, err := membersDir()
	if err != nil {
		return Card{}, false
	}
	path := memberPath(dir, id)
	c, ok := readCard(path)
	if !ok {
		if legacy, safe := legacyMemberPath(dir, id); safe {
			c, ok = readCard(legacy)
		}
	}
	// Same liveness rule as the claim itself. If these two disagreed, a
	// refusal would report no incumbent to explain itself with — which is
	// exactly what a caller sees when it cannot find the session it is
	// competing against.
	if !ok || !sessionCardAlive(c) {
		return Card{}, false
	}
	return c, true
}
