// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package handoff

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

func taker(name, episode string) principal.Ref {
	return principal.Ref{Name: name, Episode: episode, Host: "h"}
}

// ledger isolates the coord registry: a test that takes a claim must never
// touch the developer's real one.
func ledger(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
}

// parked writes an unclaimed handoff and returns the store it lives in.
func parked(t *testing.T, root string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	now := time.Now().UTC()
	r := &Record{
		ID:         NewID(now, root),
		CreatedAt:  now,
		Project:    Project{Name: "p", Primary: root, Roots: []string{root}},
		Continuity: "mid-refactor",
		Role:       "steward",
		Work:       WorkingState{Repo: root, Clean: true},
		Dispatch:   Dispatch{Disposition: DispatchPark},
	}
	if _, err := Save(dir, r); err != nil {
		t.Fatal(err)
	}
	return dir, r.ID
}

// THE RACE THIS CLOSES. Two successors pick up the same parked seat at the same
// moment. Before the ledger, both read it unclaimed, both applied the patch, and
// the second stamp erased the first — the seat looked held by one agent while
// two were working. Exactly one may win, and the work may be applied exactly
// once.
func TestConcurrentClaimsOneWinner(t *testing.T) {
	ledger(t)
	dir, id := parked(t, "/w/bashy")

	var applied atomic.Int32
	results := make([]error, 2)
	holders := []principal.Ref{taker("claude-a", "ep-aaa"), taker("codex-b", "ep-bbb")}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range holders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, ok := stored(dir, id)
			if !ok {
				results[i] = errors.New("record vanished")
				return
			}
			<-start
			_, results[i] = Claim(context.Background(), dir, rec, holders[i], func() error {
				applied.Add(1)
				return nil
			})
		}()
	}
	close(start)
	wg.Wait()

	var winners []int
	for i, err := range results {
		if err == nil {
			winners = append(winners, i)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("winners = %v, want exactly 1 (errors: %v)", winners, results)
	}
	if n := applied.Load(); n != 1 {
		t.Fatalf("work applied %d times, want 1", n)
	}

	// The loser's refusal must NAME the other party — a bare failure teaches an
	// agent nothing about who to go talk to.
	loser := results[1-winners[0]]
	var conflict *coord.Conflict
	if !errors.As(loser, &conflict) && !strings.Contains(loser.Error(), "already claimed by") {
		t.Fatalf("loser refusal does not name the holder: %v", loser)
	}

	// The durable record is the stamp, and it says the winner holds the seat.
	cur, ok := stored(dir, id)
	if !ok || cur.Status() != "active" {
		t.Fatalf("stored record is not active: %+v", cur)
	}
	if cur.ResumedBy == nil || cur.ResumedBy.Episode != holders[winners[0]].Episode {
		t.Fatalf("stamp = %+v, want the winner %+v", cur.ResumedBy, holders[winners[0]])
	}
}

// A take is idempotent for its own holder: a retried `resume --claim` — a
// re-dispatched agent, a hook that ran twice — must not re-apply the patch onto
// the tree it already restored, and must not move the stamp.
func TestSameHolderReClaimIsIdempotent(t *testing.T) {
	ledger(t)
	dir, id := parked(t, "/w/bashy")
	me := taker("claude-a", "ep-aaa")

	var applied int
	rec, _ := stored(dir, id)
	first, err := Claim(context.Background(), dir, rec, me, func() error { applied++; return nil })
	if err != nil {
		t.Fatalf("first take: %v", err)
	}

	again, _ := stored(dir, id)
	second, err := Claim(context.Background(), dir, again, me, func() error { applied++; return nil })
	if err != nil {
		t.Fatalf("a holder's own take was refused on replay: %v", err)
	}
	if applied != 1 {
		t.Fatalf("work applied %d times, want 1", applied)
	}
	if !second.ResumedAt.Equal(*first.ResumedAt) {
		t.Fatalf("replay moved the stamp: %s -> %s", first.ResumedAt, second.ResumedAt)
	}
}

// The ledger is the arbiter, not the stamp: while a take is IN FLIGHT — claim
// acquired, stamp not yet written — a second holder is refused with the standard
// coord conflict, which carries the contact commands for reaching the holder.
func TestClaimRefusesWhileAnotherTakeIsInFlight(t *testing.T) {
	ledger(t)
	dir, id := parked(t, "/w/bashy")
	a, b := taker("claude-a", "ep-aaa"), taker("codex-b", "ep-bbb")

	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref: ClaimRef(id), Holder: a, Mode: coord.ModeLease, Intent: "taking the seat",
	}); err != nil {
		t.Fatal(err)
	}

	rec, _ := stored(dir, id)
	_, err := Claim(context.Background(), dir, rec, b, func() error {
		t.Error("applied work while another take held the claim")
		return nil
	})
	var conflict *coord.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want a *coord.Conflict", err)
	}
	if conflict.Claim.Holder.Name != "claude-a" || conflict.Claim.Ref() != ClaimRef(id) {
		t.Fatalf("conflict names the wrong claim: %+v", conflict.Claim)
	}
	if len(conflict.Contacts()) == 0 {
		t.Fatal("refusal carries no way to reach the holder")
	}
	if cur, _ := stored(dir, id); cur.ResumedAt != nil {
		t.Fatal("a refused take stamped the record")
	}

	// The holder of the in-flight claim completes its own take.
	if _, err := Claim(context.Background(), dir, rec, a, func() error { return nil }); err != nil {
		t.Fatalf("the claim holder could not finish its take: %v", err)
	}
}

// The claim is RELEASED once the stamp is durable: a take must not leave a lease
// behind that outlives it, or a seat that was legitimately handed on would be
// unclaimable for the TTL.
func TestClaimReleasesTheLease(t *testing.T) {
	ledger(t)
	dir, id := parked(t, "/w/bashy")
	me := taker("claude-a", "ep-aaa")

	rec, _ := stored(dir, id)
	if _, err := Claim(context.Background(), dir, rec, me, nil); err != nil {
		t.Fatal(err)
	}
	// Someone else can take the LEDGER claim now; the stamp — not the lease —
	// is what tells them the seat is held.
	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref: ClaimRef(id), Holder: taker("codex-b", "ep-bbb"), Mode: coord.ModeLease,
	}); err != nil {
		t.Fatalf("the take held its lease past the stamp: %v", err)
	}
}
