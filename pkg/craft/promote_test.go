package craft

import (
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/redact"
)

func svc(name string) Entity { return Entity{Kind: EntityService, Name: name} }

// Three is the threshold: two is a coincidence.
func TestPromotion_NeedsThreeEntities(t *testing.T) {
	dir := t.TempDir()
	facts := OpenFacts(dir)
	folds := OpenFolds(dir, redact.New())

	_ = facts.Record(Fact{Entity: svc("loom"), Key: "port", Value: "3000"})
	_ = facts.Record(Fact{Entity: svc("zot"), Key: "port", Value: "3000"})

	if got := facts.PromotionCandidates(3, folds); len(got) != 0 {
		t.Fatalf("two entities produced a candidate: %+v", got)
	}

	_ = facts.Record(Fact{Entity: svc("registry"), Key: "port", Value: "3000"})
	got := facts.PromotionCandidates(3, folds)
	if len(got) != 1 {
		t.Fatalf("got %d candidates at three entities, want 1: %+v", len(got), got)
	}
	if !got[0].Promotable() {
		t.Errorf("a port number is not identity; it should be promotable: %s", got[0].Blocked)
	}
	if !strings.Contains(got[0].Note, "3 known services") {
		t.Errorf("the note should carry the evidence count: %q", got[0].Note)
	}
}

// Matching is on (key, VALUE). Three services on three DIFFERENT ports is three
// facts sharing a field name, not a regularity — calling it one would be reading
// structure into noise.
func TestPromotion_DistinctValuesAreNotAPattern(t *testing.T) {
	dir := t.TempDir()
	facts := OpenFacts(dir)
	folds := OpenFolds(dir, redact.New())

	_ = facts.Record(Fact{Entity: svc("loom"), Key: "port", Value: "3000"})
	_ = facts.Record(Fact{Entity: svc("zot"), Key: "port", Value: "5000"})
	_ = facts.Record(Fact{Entity: svc("registry"), Key: "port", Value: "7000"})

	if got := facts.PromotionCandidates(3, folds); len(got) != 0 {
		t.Errorf("differing values were treated as a pattern: %+v", got)
	}
}

// THE INTERLOCK. A repeated pattern that names identity is a widespread LOCAL
// fact, not a shareable truth — and promotion earns no exemption from the gate
// that would refuse it if written by hand.
func TestPromotion_IdentityPatternIsBlockedNotHidden(t *testing.T) {
	dir := t.TempDir()
	facts := OpenFacts(dir)

	hosts := []string{"alpha", "beta", "gamma"}
	for _, h := range hosts {
		_ = facts.Record(Fact{Entity: Entity{Kind: EntityHost, Name: h}, Key: "remote_user", Value: "svc-build"})
	}
	// The scrubber knows svc-build is a user (it would, via HostScrubber).
	folds := OpenFolds(dir, redact.New(redact.WithUser("svc-build")))

	got := facts.PromotionCandidates(3, folds)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Promotable() {
		t.Error("a pattern naming a username was marked promotable; widespread does not mean shareable")
	}
	// Reported, not hidden: "this repeats but cannot travel" is worth knowing.
	if got[0].Blocked == "" || !strings.Contains(got[0].Blocked, "craft learn") {
		t.Errorf("a blocked candidate should say why and where it belongs: %q", got[0].Blocked)
	}

	// And the gate holds if promotion is attempted anyway — one door.
	if err := Promote(got[0], "ccoord", folds); err == nil {
		t.Error("Promote bypassed the admission gate")
	}
}

func TestPromotion_RecordsTheFold(t *testing.T) {
	dir := t.TempDir()
	facts := OpenFacts(dir)
	folds := OpenFolds(dir, redact.New())

	for _, n := range []string{"loom", "zot", "registry"} {
		_ = facts.Record(Fact{Entity: svc(n), Key: "protocol", Value: "https"})
	}
	cands := facts.PromotionCandidates(3, folds)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates", len(cands))
	}
	if err := Promote(cands[0], "ccoord", folds); err != nil {
		t.Fatal(err)
	}

	live := folds.For("", "ccoord")
	if len(live) != 1 {
		t.Fatalf("got %d folds, want 1", len(live))
	}
	// The evidence records what the claim rested on — the COUNT survives so a
	// reader can weigh it. Entity names are scrubbed rather than refused: a
	// general claim must not be rejected for citing its sources, and tags keep
	// the shape without the identity.
	if !strings.Contains(live[0].Evidence, "3 entities") {
		t.Errorf("evidence lost the count: %q", live[0].Evidence)
	}
	if live[0].Source != "promotion" {
		t.Errorf("source = %q, want promotion", live[0].Source)
	}
}

// Strongest evidence first: a pattern seen on five entities outranks one seen
// on three.
func TestPromotion_OrderedByEvidence(t *testing.T) {
	dir := t.TempDir()
	facts := OpenFacts(dir)
	folds := OpenFolds(dir, redact.New())

	for _, n := range []string{"a", "b", "c"} {
		_ = facts.Record(Fact{Entity: svc(n), Key: "weak", Value: "v"})
	}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		_ = facts.Record(Fact{Entity: svc(n), Key: "strong", Value: "v"})
	}

	got := facts.PromotionCandidates(3, folds)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	if got[0].Key != "strong" {
		t.Errorf("order = %s then %s; strongest evidence should come first", got[0].Key, got[1].Key)
	}
}

// An invalidated fact is no longer believed, so it cannot support a pattern.
func TestPromotion_IgnoresInvalidatedFacts(t *testing.T) {
	dir := t.TempDir()
	facts := OpenFacts(dir)
	folds := OpenFolds(dir, redact.New())

	for _, n := range []string{"a", "b", "c"} {
		_ = facts.Record(Fact{Entity: svc(n), Key: "port", Value: "3000"})
	}
	if got := facts.PromotionCandidates(3, folds); len(got) != 1 {
		t.Fatalf("setup: got %d", len(got))
	}

	_ = facts.Invalidate(svc("c"), "port", time.Now().UTC())
	if got := facts.PromotionCandidates(3, folds); len(got) != 0 {
		t.Errorf("an invalidated fact still supported a pattern: %+v", got)
	}
}

func TestPromote_RequiresCoordinate(t *testing.T) {
	folds := OpenFolds(t.TempDir(), redact.New())
	err := Promote(PromotionCandidate{Note: "something"}, "", folds)
	if err == nil || !strings.Contains(err.Error(), "coordinate") {
		t.Errorf("err = %v, want a coordinate requirement", err)
	}
}
