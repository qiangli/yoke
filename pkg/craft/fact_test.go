package craft

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func workshop() Entity { return Entity{Kind: EntityHost, Name: "workshop"} }

func TestFacts_RecordAndRead(t *testing.T) {
	fs := OpenFacts(t.TempDir())
	if err := fs.Record(Fact{Entity: workshop(), Key: "remote_user", Value: "svc-build", Source: "remote-shell"}); err != nil {
		t.Fatal(err)
	}
	if err := fs.Record(Fact{Entity: workshop(), Key: "address", Value: "10.0.0.41"}); err != nil {
		t.Fatal(err)
	}

	got := fs.For(workshop())
	if len(got) != 2 {
		t.Fatalf("got %d facts, want 2: %+v", len(got), got)
	}
	if got[0].Key != "address" || got[1].Key != "remote_user" {
		t.Errorf("facts are not key-sorted: %+v", got)
	}
	if got[1].Value != "svc-build" || got[1].Source != "remote-shell" {
		t.Errorf("fact lost data: %+v", got[1])
	}
}

// Correction is expressed by writing again, never by editing — so the previous
// value is CLOSED OFF rather than removed, and one slot never shows two live
// values for a reader to guess between.
func TestFacts_SupersedeNotEdit(t *testing.T) {
	fs := OpenFacts(t.TempDir())
	_ = fs.Record(Fact{Entity: workshop(), Key: "address", Value: "10.0.0.41"})
	_ = fs.Record(Fact{Entity: workshop(), Key: "address", Value: "10.0.0.99"})

	live := fs.For(workshop())
	if len(live) != 1 {
		t.Fatalf("got %d live facts, want 1 — the old value should be closed, not duplicated: %+v", len(live), live)
	}
	if live[0].Value != "10.0.0.99" {
		t.Errorf("value = %q, want the newer one", live[0].Value)
	}
	// Nothing was rewritten: the log still carries the history.
	if n := len(fs.All()); n < 3 {
		t.Errorf("log has %d records; superseding must APPEND (old, close, new), not edit", n)
	}
}

// THE FAILURE THIS PREVENTS. A learned address goes stale, the next agent is
// handed it with full confidence, and the living skill teaches something false
// rather than teaching nothing.
func TestFacts_InvalidateStopsTheClaim(t *testing.T) {
	fs := OpenFacts(t.TempDir())
	_ = fs.Record(Fact{Entity: workshop(), Key: "address", Value: "10.0.0.41"})

	if len(fs.For(workshop())) != 1 {
		t.Fatal("setup")
	}
	if err := fs.Invalidate(workshop(), "address", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := fs.For(workshop()); len(got) != 0 {
		t.Errorf("an invalidated fact is still being served: %+v", got)
	}
}

// "This is now wrong" and "this is now X" are different claims. A failed run has
// learned only the first, and forcing it to invent a replacement is how a guess
// enters the store.
func TestFacts_InvalidateAssertsNoReplacement(t *testing.T) {
	fs := OpenFacts(t.TempDir())
	_ = fs.Record(Fact{Entity: workshop(), Key: "address", Value: "10.0.0.41"})
	_ = fs.Invalidate(workshop(), "address", time.Now().UTC())

	for _, f := range fs.All() {
		if f.ValidUntil != nil {
			continue
		}
		if f.Key == "address" && f.Value != "10.0.0.41" {
			t.Errorf("invalidation invented a value: %+v", f)
		}
	}
	// Invalidating something never believed is a no-op, not an error.
	if err := fs.Invalidate(workshop(), "never-known", time.Now().UTC()); err != nil {
		t.Errorf("invalidating an unknown key errored: %v", err)
	}
}

// Bi-temporal: what did this host BELIEVE at a past instant? A single-timestamp
// store cannot answer that, and it is the only way to explain after the fact why
// an agent did what it did.
func TestFacts_AsOfAnswersThePast(t *testing.T) {
	fs := OpenFacts(t.TempDir())
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	t1 := time.Now().UTC().Add(-1 * time.Hour)

	_ = fs.Record(Fact{Entity: workshop(), Key: "address", Value: "10.0.0.41", ValidFrom: t0})
	_ = fs.Invalidate(workshop(), "address", t1)

	if got := fs.AsOf(workshop(), t0.Add(time.Minute)); len(got) != 1 || got[0].Value != "10.0.0.41" {
		t.Errorf("AsOf(before invalidation) = %+v, want the old belief", got)
	}
	if got := fs.AsOf(workshop(), time.Now().UTC()); len(got) != 0 {
		t.Errorf("AsOf(now) = %+v, want nothing", got)
	}
}

// Facts are identity. The default 0644 would put a machine's logins into every
// backup and every image built from the home directory.
func TestFacts_FileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := t.TempDir()
	fs := OpenFacts(dir)
	_ = fs.Record(Fact{Entity: workshop(), Key: "remote_user", Value: "svc-build"})

	info, err := os.Stat(filepath.Join(dir, "facts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("fact log mode = %o, want 600 — facts are identity", perm)
	}
}

// A store never written to is empty, not broken: reads happen on the compose
// path and must not fail because nothing has been learned yet.
func TestFacts_MissingStoreIsEmpty(t *testing.T) {
	fs := OpenFacts(filepath.Join(t.TempDir(), "nope"))
	if got := fs.For(workshop()); len(got) != 0 {
		t.Errorf("got %+v from a missing store", got)
	}
	if got := fs.Entities(); len(got) != 0 {
		t.Errorf("got %+v entities from a missing store", got)
	}
}

func TestFacts_RejectsIncompleteRecords(t *testing.T) {
	fs := OpenFacts(t.TempDir())
	if err := fs.Record(Fact{Key: "k", Value: "v"}); err == nil {
		t.Error("a fact with no entity was accepted")
	}
	if err := fs.Record(Fact{Entity: workshop(), Value: "v"}); err == nil {
		t.Error("a fact with no key was accepted")
	}
	if err := (&FactStore{}).Record(Fact{Entity: workshop(), Key: "k"}); err == nil {
		t.Error("a store with no path accepted a write")
	}
}

func TestFacts_Entities(t *testing.T) {
	fs := OpenFacts(t.TempDir())
	_ = fs.Record(Fact{Entity: workshop(), Key: "a", Value: "1"})
	_ = fs.Record(Fact{Entity: Entity{Kind: EntityService, Name: "loom"}, Key: "port", Value: "3000"})

	got := fs.Entities()
	if len(got) != 2 {
		t.Fatalf("got %d entities, want 2: %+v", len(got), got)
	}
	if got[0].ID() != "host:workshop" || got[1].ID() != "service:loom" {
		t.Errorf("entities = %+v, want sorted by id", got)
	}
}

// THE SSH CASE, which is what the whole fact layer exists for: one agent learns
// a host's particulars, and the NEXT agent — a different tool entirely —
// inherits them without anyone writing anything down.
func TestCompose_InheritsWhatAnotherAgentLearned(t *testing.T) {
	dir := t.TempDir()
	store := OpenFacts(dir)

	// Agent A works the host and records what it found.
	if err := store.Record(Fact{Entity: workshop(), Key: "remote_user", Value: "svc-build", Source: "remote-shell"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(Fact{Entity: workshop(), Key: "address", Value: "10.0.0.41", Source: "remote-shell"}); err != nil {
		t.Fatal(err)
	}

	// Agent B composes for the same host and gets them.
	im := impl(t, "remote-shell", goBuildTest, "connect to a host",
		map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})
	c, err := Compose(im, ComposeOptions{
		Band:   BandScript,
		Entity: workshop(),
		Facts:  store.For(workshop()),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"svc-build", "10.0.0.41", "remote-shell"} {
		if !strings.Contains(c.Body, want) {
			t.Errorf("the composition did not inherit %q:\n%s", want, c.Body)
		}
	}
	// Even at band 0. A fact is a VALUE the procedure needs, not guidance a
	// model interprets — a script that must ask for the login is not runnable
	// without a model, which defeats the band.
	if c.Band != BandScript {
		t.Fatalf("band = %d, want 0", c.Band)
	}
	if c.Facts != 2 {
		t.Errorf("Facts = %d, want 2", c.Facts)
	}

	// A DIFFERENT host inherits nothing: particulars are entity-bound.
	other := Entity{Kind: EntityHost, Name: "elsewhere"}
	c2, err := Compose(im, ComposeOptions{Band: BandScript, Entity: other, Facts: store.For(other)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c2.Body, "svc-build") {
		t.Error("one host's particulars leaked into another's composition")
	}
}

// A Composition is logged, marshalled, and passed around. Fact VALUES must not
// travel with it — only the count.
func TestComposition_CarriesFactCountNeverValues(t *testing.T) {
	im := impl(t, "s", goBuildTest, "d", map[string]string{"check-build": "b", "check-tests": "t"})
	c, err := Compose(im, ComposeOptions{
		Band:   BandScript,
		Entity: workshop(),
		Facts:  []Fact{{Entity: workshop(), Key: "remote_user", Value: "svc-build"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Facts != 1 {
		t.Errorf("Facts = %d, want 1", c.Facts)
	}
	// The struct itself (excluding the rendered Body, which is the artifact the
	// caller asked for) must not hold the value.
	if strings.Contains(c.Stamp+c.Name+c.Identity+c.Capability, "svc-build") {
		t.Error("a fact value leaked into the composition's metadata")
	}
}

// Different facts must stamp differently, or the stamp would claim a
// reproducibility it does not have.
func TestCompose_FactsChangeTheStamp(t *testing.T) {
	im := impl(t, "s", goBuildTest, "d", map[string]string{"check-build": "b", "check-tests": "t"})
	base := ComposeOptions{Band: BandScript, Entity: workshop()}

	none, _ := Compose(im, base)

	withFact := base
	withFact.Facts = []Fact{{Entity: workshop(), Key: "remote_user", Value: "svc-build"}}
	one, _ := Compose(im, withFact)

	changed := base
	changed.Facts = []Fact{{Entity: workshop(), Key: "remote_user", Value: "other"}}
	two, _ := Compose(im, changed)

	if none.Stamp == one.Stamp {
		t.Error("adding a fact did not change the stamp")
	}
	if one.Stamp == two.Stamp {
		t.Error("changing a fact's value did not change the stamp")
	}
}
