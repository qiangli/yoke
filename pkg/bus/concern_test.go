package bus

// A CONCERN IS A ROUTE, NOT AN ADDRESS. The defect these tests pin: Unseen
// capped everything that did not name the reader, so a shared-baseline
// announcement on a busy board scrolled out of the default view and the reader
// got a COUNT instead of the message. Declaring the concern — the topic field
// the bus subscriptions always had — lifts the cap; not declaring it must not
// be silently promoted, or tagging becomes a megaphone.

import (
	"context"
	"strings"
	"testing"
)

// declare gives a reader a subscription whose topics are its board concerns.
func declare(t *testing.T, reader string, topics ...string) {
	t.Helper()
	if err := SaveSubscription(Subscription{Subscriber: reader, Topics: topics}); err != nil {
		t.Fatal(err)
	}
}

// THE GATE, half one: a post on a concern the reader DECLARED is never capped,
// however old it is and however busy the board.
func TestBoard_DeclaredConcernIsUncapped(t *testing.T) {
	boardInTempHome(t)
	declare(t, "me", "shared-baseline")
	for range 3 { // oldest — exactly what a cap keeping the newest would drop
		if err := PostMessage(Post{From: "steward", Topic: "shared-baseline", Body: "sh/ is frozen"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 9 {
		if err := PostMessage(Post{From: "a", Topic: "mb", Body: "chatter"}); err != nil {
			t.Fatal(err)
		}
	}
	directed, other, older, err := Unseen("me", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(directed) != 0 {
		t.Fatalf("a concern post carries no reply obligation, directed = %d", len(directed))
	}
	// All 3 concern posts plus the capped newest 5 of the chatter.
	if len(other) != 8 {
		t.Fatalf("other = %d, want 3 concern + 5 capped", len(other))
	}
	for _, want := range []int64{1, 2, 3} {
		found := false
		for _, p := range other {
			if p.Seq == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("concern post %d was capped out of the view", want)
		}
	}
	if older != 4 {
		t.Fatalf("older = %d, want 4 — the cap reports only what it actually hid", older)
	}
	// Board order is preserved: the exemption changes what is shown, not when.
	for i := 1; i < len(other); i++ {
		if other[i].Seq <= other[i-1].Seq {
			t.Fatalf("view out of board order at %d: %d after %d", i, other[i].Seq, other[i-1].Seq)
		}
	}
}

// THE GATE, half two: an UNDECLARED concern is not silently promoted. The tag
// alone lifts no cap — otherwise every sender tags and the cap is gone.
func TestBoard_UndeclaredConcernIsNotSilentlyPromoted(t *testing.T) {
	boardInTempHome(t)
	for range 3 {
		if err := PostMessage(Post{From: "steward", Topic: "posix-cert", Body: "cert news"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 9 {
		if err := PostMessage(Post{From: "a", Topic: "mb", Body: "chatter"}); err != nil {
			t.Fatal(err)
		}
	}
	_, other, older, err := Unseen("stranger", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 5 || older != 7 {
		t.Fatalf("other = %d, older = %d — an undeclared tag must be capped like anything else (want 5/7)", len(other), older)
	}
}

// announce is this board's wall: the one concern every reader is subscribed to
// by default, subscription or none.
func TestBoard_AnnounceIsEveryReadersConcern(t *testing.T) {
	boardInTempHome(t)
	if err := PostMessage(Post{From: "steward", Topic: "announce", Body: "baseline moved"}); err != nil {
		t.Fatal(err)
	}
	for range 9 {
		if err := PostMessage(Post{From: "a", Topic: "mb", Body: "chatter"}); err != nil {
			t.Fatal(err)
		}
	}
	_, other, older, err := Unseen("never-subscribed", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 6 || other[0].Seq != 1 {
		t.Fatalf("other = %d (first seq %d) — the announcement must survive the cap", len(other), other[0].Seq)
	}
	if older != 4 {
		t.Fatalf("older = %d, want 4", older)
	}
}

// The route works both ways: a declared concern surfaces a matching post even
// when it is addressed to somebody else. A standing interest in the subject is
// exactly what the declaration says — and the board is public anyway.
func TestBoard_ConcernRoutesAPostAddressedToAnother(t *testing.T) {
	boardInTempHome(t)
	declare(t, "watcher", "posix-cert")
	if err := PostMessage(Post{From: "a", To: "agent-x", Topic: "posix-cert", Body: "cert gate moved"}); err != nil {
		t.Fatal(err)
	}
	if directed, other, _, _ := Unseen("watcher", 0); len(directed) != 0 || len(other) != 1 {
		t.Fatalf("watcher sees %d directed / %d other, want 0/1 — routed, not obligated to reply", len(directed), len(other))
	}
	if directed, other, _, _ := Unseen("stranger", 0); len(directed)+len(other) != 0 {
		t.Fatal("a post to somebody else on nothing you declared is not your business")
	}
	if directed, _, _, _ := Unseen("agent-x", 0); len(directed) != 1 {
		t.Fatal("the named recipient still owns the obligation")
	}
}

// A work offer is never concern-routed: it belongs to its pool and its claim
// rule. A declarer outside the pool must not be handed — much less claim —
// work that was not offered to it.
func TestBoard_WorkOffersAreNeverConcernRouted(t *testing.T) {
	boardInTempHome(t)
	declare(t, "watcher", "posix-cert")
	FleetSelect = func(a Audience) ([]string, error) { return []string{"a1"}, nil }
	t.Cleanup(func() { FleetSelect = nil })

	if err := PostMessage(Post{
		From: "steward", Audience: &Audience{Band: 3}, Mode: ModeAny, Topic: "posix-cert", Body: "take the cert gate",
	}); err != nil {
		t.Fatal(err)
	}
	if directed, other, _, _ := Unseen("watcher", 0); len(directed)+len(other) != 0 {
		t.Fatal("an offer to a pool the declarer is not in must not be routed to it")
	}
	if _, other, _, _ := Unseen("a1", 0); len(other) != 1 {
		t.Fatal("the pool member must still see the offer")
	}
}

// Declarations match the way subscription topics always have: segment-anchored,
// so `cert.*` covers `cert.posix` but never `certainly`.
func TestBoard_ConcernPatternsAreSegmentAnchored(t *testing.T) {
	boardInTempHome(t)
	declare(t, "me", "cert.*")
	if err := PostMessage(Post{From: "a", To: "agent-x", Topic: "cert.posix", Body: "in scope"}); err != nil {
		t.Fatal(err)
	}
	if err := PostMessage(Post{From: "a", To: "agent-x", Topic: "certainly-not", Body: "out of scope"}); err != nil {
		t.Fatal(err)
	}
	_, other, _, err := Unseen("me", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].Topic != "cert.posix" {
		t.Fatalf("routed %d post(s) — the wildcard must cover dotted segments only", len(other))
	}
}

// ConcernDeclarers is the denominator for "did everyone concerned read it" —
// the readers who OWED the post a read, judged against the counted views.
func TestConcernDeclarers(t *testing.T) {
	boardInTempHome(t)
	declare(t, "a", "harness")
	declare(t, "b", "*")
	declare(t, "c", "posix-cert")
	got := ConcernDeclarers("harness")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("declarers of harness = %v, want [a b]", got)
	}
}

// Reading a concern post leaves the same view record a ModeAll announcement
// does — the receipt the declaration promised — and the render says which
// concern routed it.
func TestMessageBoard_ConcernReadIsCountedAndLabeled(t *testing.T) {
	boardInTempHome(t)
	declare(t, "reader-a", "shared-baseline")
	if err := PostMessage(Post{From: "steward", Topic: "shared-baseline", Body: "the baseline moved"}); err != nil {
		t.Fatal(err)
	}
	out, _, err := runMessageBoard(t, context.Background(), "--as", "reader-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the baseline moved") {
		t.Fatalf("the routed post is missing:\n%s", out)
	}
	if !strings.Contains(out, "concern shared-baseline") {
		t.Fatalf("the label does not say what routed it:\n%s", out)
	}
	if v := Viewers(1); len(v) != 1 || v[0] != "reader-a" {
		t.Fatalf("viewers = %v — a concern read must be counted, or nobody can ask who was reached", v)
	}
}
