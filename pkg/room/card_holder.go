// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package room

// ONE LIVENESS RULE for every card, because the board has four entry points
// into the same files.
//
// A card names two processes. PID is whoever wrote it last, which for an agent
// session is the per-turn child command and is therefore dead between turns.
// OwnerPID is the stable harness that owns the conversation. Members, Join and
// LeavePID each judged a card on PID alone while ClaimSession judged it on
// OwnerPID, so the gap between two turns of a live conversation was a window in
// which the board declared the conversation dead: Members DELETED the claim,
// Join overwrote it, and a departing child's Leave evicted it. A claim that is
// gone refuses nobody, which is how one instance came to have two live drivers.
//
// The holder is therefore the OWNER when one is recorded, and only otherwise
// the writer. Every path below asks these three functions and nothing else.

// holderPID is the process whose liveness decides whether a card is held.
func holderPID(c Card) int {
	if c.OwnerPID != 0 {
		return c.OwnerPID
	}
	return c.PID
}

// cardAlive reports whether a card is still held.
//
// A card with neither pid is NOT live. That is the safe direction: it cannot
// prove an owner, and an unprovable owner must not fence out a newcomer
// forever.
func cardAlive(c Card) bool {
	pid := holderPID(c)
	return pid != 0 && PidAlive(pid)
}

// heldByOtherThan reports whether a LIVE card is held by somebody other than
// pid — the single question "may I write or remove this card?" reduces to.
//
// A stale card is not held by anybody, so it stays reclaimable: the repair must
// not turn "fence the living" into "never reclaim the dead".
func heldByOtherThan(c Card, pid int) bool {
	return cardAlive(c) && holderPID(c) != pid
}
