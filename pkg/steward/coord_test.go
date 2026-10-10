package steward

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/policy/coord"
)

func TestCoordSeatProjectsJournalAndFences(t *testing.T) {
	// Guard reads under the host ledger's claims.lock; keep that out of ~/.bashy.
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
	s := newStore(t)
	b := RegisterSeatBackend(s, SeatRequest{})
	t.Cleanup(func() { coord.RegisterBackend("seat", nil) })
	now := time.Now().UTC()
	g := mustGrant(t, s, agent("a"), ActionClaim, now)
	v, err := s.Claim(context.Background(), agent("a"), SeatRequest{GrantID: g.ID, Attended: true, Intent: "host care"}, now)
	if err != nil {
		t.Fatal(err)
	}
	key := coord.Ref{Kind: "seat", Name: s.Scope()}.String()
	claim, err := b.Load(key)
	if err != nil || claim == nil || claim.Epoch != v.Authority.Epoch || claim.Holder != agent("a") || claim.Intent != "host care" {
		t.Fatalf("Load = %+v, %v", claim, err)
	}
	all, err := coord.List(t.TempDir())
	if err != nil || len(all) != 1 || all[0].Kind != "seat" {
		t.Fatalf("List = %+v, %v", all, err)
	}
	if err := coord.Guard(context.Background(), agent("b"), coord.Use{Kind: "seat", Name: s.Scope()}); err == nil {
		t.Fatal("other holder passed seat guard")
	}
	if err := coord.Guard(context.Background(), agent("a"), coord.Use{Kind: "seat", Name: s.Scope()}); err != nil {
		t.Fatalf("holder blocked by guard: %v", err)
	}
	if err := s.Heartbeat(agent("a"), claim.Epoch, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stale := *claim
	if err := b.CommitIfRev(key, claim.Rev, &stale); !errors.Is(err, coord.ErrEpochMismatch) {
		t.Fatalf("refresh under stale revision = %v, want CAS failure", err)
	}
	claim, err = b.Load(key)
	if err != nil || claim.Rev <= stale.Rev {
		t.Fatalf("heartbeat revision = %+v, %v", claim, err)
	}
	if err := b.CommitIfRev(key, claim.Rev, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := b.CommitIfRev(key, claim.Rev, nil); err == nil {
		t.Fatal("stale epoch accepted")
	} else if !errors.Is(err, coord.ErrEpochMismatch) {
		t.Fatalf("stale revision = %T %v, want CAS failure", err, err)
	}
	if got, err := b.Load(key); err != nil || got != nil {
		t.Fatalf("released seat = %+v, %v", got, err)
	}
}

func TestCoordSeatAcquisitionKeepsStewardGrantAndEpoch(t *testing.T) {
	s := newStore(t)
	b := RegisterSeatBackend(s, SeatRequest{})
	t.Cleanup(func() { coord.RegisterBackend("seat", nil) })
	ref := coord.Ref{Kind: "seat", Name: s.Scope()}
	request := coord.Request{Ref: ref, Holder: agent("a"), Intent: "care"}
	if _, err := coord.AcquireRef(context.Background(), request); err == nil {
		t.Fatal("coord acquired the seat without a steward grant")
	}
	g := mustGrant(t, s, agent("a"), ActionClaim, time.Now().UTC())
	b.Request = SeatRequest{GrantID: g.ID, Attended: true}
	first, err := coord.AcquireRef(context.Background(), request)
	if err != nil || first.Epoch == 0 {
		t.Fatalf("first acquisition = %+v, %v", first, err)
	}
	var noEpoch *ErrNoEpoch
	if _, err := coord.AcquireRef(context.Background(), request); !errors.As(err, &noEpoch) {
		t.Fatalf("reclaim without epoch = %v, want steward ErrNoEpoch", err)
	}
	if _, err := coord.Refresh(context.Background(), ref, agent("a"), 0); !errors.As(err, &noEpoch) {
		t.Fatalf("refresh without epoch = %v, want steward ErrNoEpoch", err)
	}
	if err := coord.ReleaseRef(context.Background(), ref, agent("a"), 0); !errors.As(err, &noEpoch) {
		t.Fatalf("release without epoch = %v, want steward ErrNoEpoch", err)
	}
	if _, err := coord.Refresh(context.Background(), ref, agent("a"), first.Epoch); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := coord.ReleaseRef(context.Background(), ref, agent("a"), first.Epoch); err != nil {
		t.Fatalf("release: %v", err)
	}
	g = mustGrant(t, s, agent("a"), ActionClaim, time.Now().UTC())
	b.Request = SeatRequest{GrantID: g.ID, Attended: true}
	second, err := coord.AcquireRef(context.Background(), request)
	if err != nil || second.Epoch <= first.Epoch {
		t.Fatalf("journal epoch after release = %+v, %v", second, err)
	}
	if _, err := coord.Refresh(context.Background(), ref, agent("a"), first.Epoch); err == nil {
		t.Fatal("old epoch heartbeated the new tenure")
	}
	g = mustGrant(t, s, agent("b"), ActionTakeover, time.Now().UTC())
	b.Request = SeatRequest{GrantID: g.ID, Attended: true}
	third, err := coord.AcquireRef(context.Background(), coord.Request{Ref: ref, Holder: agent("b"), Force: true})
	if err != nil || third.Epoch <= second.Epoch || third.Claim.Holder != agent("b") {
		t.Fatalf("authorized takeover = %+v, %v", third, err)
	}
	if err := b.CommitIfRev(ref.String(), second.Claim.Rev, nil); err == nil {
		t.Fatal("takeover did not fence the prior tenure")
	} else if !errors.Is(err, coord.ErrEpochMismatch) {
		t.Fatalf("takeover revision = %T %v", err, err)
	}
}
