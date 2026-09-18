package craft

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/redact"
)

const winCoord = "cwindows-amd64"

func testFolds(t *testing.T) *FoldStore {
	t.Helper()
	return OpenFolds(t.TempDir(), redact.New(
		redact.WithHost("workshop"), redact.WithUser("svc-build")))
}

// THE ADMISSION GATE, and the reason this layer is safe: a note that names host
// identity is a FACT wearing a fold's clothes. Admitting it would put a
// machine's particulars into the one store meant to be shared, where they
// travel. Classification is CHECKED, not trusted.
func TestFolds_RefusesIdentityBearingNotes(t *testing.T) {
	fs := testFolds(t)

	tests := []struct {
		name, note string
	}{
		{"hostname", "ssh to workshop first, then run the build"},
		{"username", "run as svc-build or the cache is unwritable"},
		{"address", "the registry answers at 10.0.0.41, not localhost"},
		{"email", "ask alice@example.com before rotating the token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := fs.Record(Fold{Coordinate: winCoord, Note: tc.note})
			var notGen *ErrNotGeneralisable
			if !errors.As(err, &notGen) {
				t.Fatalf("err = %v, want ErrNotGeneralisable", err)
			}
			// The refusal must name where it DOES belong; a bare "no" just
			// makes the caller find a way around it.
			if !strings.Contains(err.Error(), "craft learn") {
				t.Errorf("the refusal should point at the right home: %v", err)
			}
		})
	}
	if got := fs.All(); len(got) != 0 {
		t.Errorf("a refused fold was still written: %+v", got)
	}
}

// The genuinely general thing is admitted — the gate must not be so tight that
// real knowledge cannot land.
func TestFolds_AdmitsGeneralisableNotes(t *testing.T) {
	fs := testFolds(t)
	note := "mDNS resolution is unreliable here; resolve the address first and connect by IP"

	if err := fs.Record(Fold{
		Coordinate: winCoord, Note: note,
		Evidence: "mdns lookup timed out after 30s on three consecutive runs",
		Source:   "remote-shell",
	}); err != nil {
		t.Fatalf("a generalisable note was refused: %v", err)
	}

	got := fs.For("", winCoord)
	if len(got) != 1 {
		t.Fatalf("got %d folds, want 1", len(got))
	}
	if got[0].Note != note || got[0].Evidence == "" {
		t.Errorf("fold lost data: %+v", got[0])
	}
}

// Coordinate keying is the point: one machine's discovery is useful on another
// LIKE IT, and useless everywhere else.
func TestFolds_ScopedToCoordinate(t *testing.T) {
	fs := testFolds(t)
	_ = fs.Record(Fold{Coordinate: winCoord, Note: "resolve by IP; mDNS is unreliable"})

	if got := fs.For("", winCoord); len(got) != 1 {
		t.Errorf("got %d folds at the recorded coordinate, want 1", len(got))
	}
	if got := fs.For("", "cdarwin-arm64"); len(got) != 0 {
		t.Errorf("a fold leaked to another coordinate: %+v", got)
	}
}

// A capability-scoped fold amends one skill; an unscoped one is an environment
// truth that applies to every skill at the coordinate.
func TestFolds_CapabilityScoping(t *testing.T) {
	fs := testFolds(t)
	_ = fs.Record(Fold{Coordinate: winCoord, Note: "an environment truth"})
	_ = fs.Record(Fold{Coordinate: winCoord, Capability: "kbuild", Note: "specific to the build capability"})

	all := fs.For("kbuild", winCoord)
	if len(all) != 2 {
		t.Errorf("got %d folds for kbuild, want 2 (its own plus the environment truth): %+v", len(all), all)
	}
	other := fs.For("kother", winCoord)
	if len(other) != 1 || other[0].Note != "an environment truth" {
		t.Errorf("got %+v for an unrelated capability, want only the environment truth", other)
	}
}

// A workaround for a bug that has been fixed is worse than no advice: it sends
// the next agent down what is now the slow path.
func TestFolds_Retire(t *testing.T) {
	fs := testFolds(t)
	note := "resolve by IP; mDNS is unreliable"
	_ = fs.Record(Fold{Coordinate: winCoord, Note: note})

	if err := fs.Retire("", winCoord, note, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := fs.For("", winCoord); len(got) != 0 {
		t.Errorf("a retired fold is still being served: %+v", got)
	}
	// Nothing was rewritten.
	if len(fs.All()) != 2 {
		t.Errorf("retire should APPEND a closing record, not edit; log has %d", len(fs.All()))
	}
}

func TestFolds_RejectsIncomplete(t *testing.T) {
	fs := testFolds(t)
	if err := fs.Record(Fold{Coordinate: winCoord}); err == nil {
		t.Error("a fold with no note was accepted")
	}
	if err := fs.Record(Fold{Note: "something"}); err == nil {
		t.Error("a fold with no coordinate was accepted — it would hold nowhere")
	}
	if err := (&FoldStore{}).Record(Fold{Coordinate: "c", Note: "n"}); err == nil {
		t.Error("a store with no path accepted a write")
	}
}

func TestFolds_MissingStoreIsEmpty(t *testing.T) {
	fs := OpenFolds(t.TempDir()+"/nope", nil)
	if got := fs.For("", winCoord); len(got) != 0 {
		t.Errorf("got %+v from a missing store", got)
	}
	if got := fs.Coordinates(); len(got) != 0 {
		t.Errorf("got %v coordinates from a missing store", got)
	}
}

// THE FULL ssh CASE, both halves. The workaround GENERALISES and travels with
// the coordinate; the login is PARTICULAR and binds to the host. A composed
// skill carries both, from two different stores, without the caller knowing
// there were two.
func TestCompose_CarriesBothFoldsAndFacts(t *testing.T) {
	dir := t.TempDir()
	facts := OpenFacts(dir)
	folds := OpenFolds(dir, redact.New(redact.WithHost("workshop")))

	if err := folds.Record(Fold{
		Coordinate: winCoord,
		Note:       "mDNS is unreliable here; resolve the address first and connect by IP",
		Evidence:   "three consecutive lookup timeouts",
	}); err != nil {
		t.Fatal(err)
	}
	if err := facts.Record(Fact{
		Entity: workshop(), Key: "address", Value: "10.0.0.41", Source: "remote-shell",
	}); err != nil {
		t.Fatal(err)
	}

	im := impl(t, "remote-shell", goBuildTest, "connect to a host",
		map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})
	c, err := Compose(im, ComposeOptions{
		Band:       BandScript,
		Coordinate: winCoord,
		Entity:     workshop(),
		Facts:      facts.For(workshop()),
		Folds:      folds.For(c2capability(t, im), winCoord),
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(c.Body, "connect by IP") {
		t.Errorf("the composition lost the generalisable workaround:\n%s", c.Body)
	}
	if !strings.Contains(c.Body, "10.0.0.41") {
		t.Errorf("the composition lost the particular fact:\n%s", c.Body)
	}
	if c.Folds != 1 || c.Facts != 1 {
		t.Errorf("Folds=%d Facts=%d, want 1 and 1", c.Folds, c.Facts)
	}
}

// Folds change the stamp, or it would claim a reproducibility it lacks.
func TestCompose_FoldsChangeTheStamp(t *testing.T) {
	im := impl(t, "s", goBuildTest, "d", map[string]string{"check-build": "b", "check-tests": "t"})
	base := ComposeOptions{Band: BandScript, Coordinate: winCoord}

	none, _ := Compose(im, base)
	withFold := base
	withFold.Folds = []Fold{{Coordinate: winCoord, Note: "resolve by IP"}}
	one, _ := Compose(im, withFold)

	if none.Stamp == one.Stamp {
		t.Error("adding a fold did not change the stamp")
	}
}

func c2capability(t *testing.T, im Implementation) string {
	t.Helper()
	k, err := capabilityOf(im.Skill)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The gate's REAL strength and its REAL limit, pinned so neither is folklore.
//
// redact.FromHost knows only the local machine, so a note naming someone else's
// box sails through — "workshop" is just an English word to a scrubber that has
// never heard of it. The fact store is the missing vocabulary: once this host
// has learned that workshop IS a host, the same note is refused. The gate
// therefore gets stricter as the host learns more, which is the right direction
// — and a name nobody here has ever recorded remains invisible, which is the
// honest limit.
func TestHostScrubber_LearnsIdentityFromFacts(t *testing.T) {
	dir := t.TempDir()

	// Before: an unknown host's name is not recognised as identity.
	before := OpenFolds(dir, HostScrubber(dir))
	if err := before.Record(Fold{Coordinate: winCoord, Note: "ssh to workshop as svc-build"}); err != nil {
		t.Fatalf("with no facts recorded the gate cannot know the name; got %v", err)
	}

	// Teach it.
	facts := OpenFacts(dir)
	if err := facts.Record(Fact{Entity: workshop(), Key: "remote_user", Value: "svc-build"}); err != nil {
		t.Fatal(err)
	}

	// After: the SAME note is refused.
	after := OpenFolds(dir, HostScrubber(dir))
	err := after.Record(Fold{Coordinate: winCoord, Note: "ssh to workshop as svc-build"})
	var notGen *ErrNotGeneralisable
	if !errors.As(err, &notGen) {
		t.Fatalf("after learning the entity the note should be refused; got %v", err)
	}
}

// Shape-detected identity needs no learning at all: an address is an address on
// any host, so this half of the gate is never blind.
func TestHostScrubber_CatchesAddressesWithoutLearning(t *testing.T) {
	dir := t.TempDir()
	fs := OpenFolds(dir, HostScrubber(dir))

	for _, note := range []string{
		"the registry answers at 10.0.0.41",
		"mail alice@example.com before rotating",
	} {
		var notGen *ErrNotGeneralisable
		if err := fs.Record(Fold{Coordinate: winCoord, Note: note}); !errors.As(err, &notGen) {
			t.Errorf("%q was admitted; shape detection needs no prior knowledge (err=%v)", note, err)
		}
	}
}

// The note and the evidence are held to different standards, and conflating
// them rejected general claims for citing their sources — which is what
// happened the first time promotion ran for real.
func TestFolds_EvidenceIsScrubbedNotRefused(t *testing.T) {
	fs := testFolds(t)

	err := fs.Record(Fold{
		Coordinate: winCoord,
		Note:       "the protocol is https on every service here",
		Evidence:   "observed on workshop and two others",
	})
	if err != nil {
		t.Fatalf("a clean NOTE was refused because its evidence named a host: %v", err)
	}

	got := fs.For("", winCoord)
	if len(got) != 1 {
		t.Fatalf("got %d folds", len(got))
	}
	if strings.Contains(got[0].Evidence, "workshop") {
		t.Errorf("the evidence still names the host: %q", got[0].Evidence)
	}
	if !strings.Contains(got[0].Evidence, "‹host:") {
		t.Errorf("the evidence should keep a co-reference-preserving tag: %q", got[0].Evidence)
	}
	// A dirty NOTE is still refused — the standard did not slacken.
	var notGen *ErrNotGeneralisable
	if err := fs.Record(Fold{Coordinate: winCoord, Note: "ssh to workshop"}); !errors.As(err, &notGen) {
		t.Errorf("a note naming a host was admitted: %v", err)
	}
}
