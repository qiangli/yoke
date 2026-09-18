package craft

import (
	"errors"
	"strings"
	"testing"

	dhntskills "github.com/dhnt/dhnt/skills"
)

func impl(t *testing.T, name, canon, desc string, bindings map[string]string) Implementation {
	t.Helper()
	sk, err := dhntskills.ParseDhnt(canon)
	if err != nil {
		t.Fatalf("ParseDhnt(%q): %v", canon, err)
	}
	return Implementation{Name: name, Skill: sk, Description: desc, Bindings: bindings}
}

// A fully-bound skill runs with NO model. That is the claim, so it is the first
// thing asserted.
func TestCompose_FullyBoundRendersBandZero(t *testing.T) {
	im := impl(t, "go-repo-health", goBuildTest, "verify a Go repo is healthy",
		map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})

	c, err := Compose(im, ComposeOptions{Band: -1})
	if err != nil {
		t.Fatal(err)
	}
	if c.Floor != BandScript {
		t.Errorf("floor = %d, want 0 — every predicate is bound", c.Floor)
	}
	if c.Band != BandScript {
		t.Errorf("band = %d; the default policy is the artifact's FLOOR, not the model's ceiling", c.Band)
	}
	if c.DeterminismRatio != 1.0 {
		t.Errorf("determinism ratio = %v, want 1.0", c.DeterminismRatio)
	}
	for _, want := range []string{"go build ./...", "go test ./...", "set -euo pipefail"} {
		if !strings.Contains(c.Body, want) {
			t.Errorf("band-0 body is missing %q:\n%s", want, c.Body)
		}
	}
}

// THE RULE. A lower band is never synthesized — a model writing a script at
// render time would put a model back on the read path and destroy
// reproducibility.
func TestCompose_RefusesBandBelowFloor(t *testing.T) {
	unbound := impl(t, "prose-ish", goBuildTest, "", nil)

	c, err := Compose(unbound, ComposeOptions{Band: -1})
	if err != nil {
		t.Fatal(err)
	}
	if c.Floor == BandScript {
		t.Fatal("an unbound skill reported a band-0 floor")
	}

	_, err = Compose(unbound, ComposeOptions{Band: BandScript})
	var bandErr *ErrBandUnavailable
	if !errors.As(err, &bandErr) {
		t.Fatalf("err = %v, want ErrBandUnavailable", err)
	}
	if bandErr.Floor != c.Floor {
		t.Errorf("error floor = %d, want %d", bandErr.Floor, c.Floor)
	}
	if !strings.Contains(err.Error(), "not synthesized") {
		t.Errorf("the refusal should say WHY it will not fabricate: %v", err)
	}
}

// Every skill renders at band 4, because intent always exists.
func TestCompose_BandFourAlwaysAvailable(t *testing.T) {
	im := impl(t, "prose-ish", goBuildTest, "a description", nil)
	c, err := Compose(im, ComposeOptions{Band: BandIntent})
	if err != nil {
		t.Fatalf("band 4 must always render: %v", err)
	}
	if !strings.Contains(c.Body, "a description") {
		t.Errorf("band-4 body should carry the intent:\n%s", c.Body)
	}
	if !strings.Contains(c.Body, "builida") {
		t.Errorf("band-4 body should carry the contract:\n%s", c.Body)
	}
}

// "Less script, more prose" as the band rises — and the property is about
// CONCRETE COMMAND CONTENT, not byte size.
//
// Raw length is NOT monotone here, and that is correct rather than a defect:
// band 1 is the LARGEST rendering, because a small local model needs the script
// *plus* an error-recovery table, whereas band 0 carries no explanation at all —
// nothing reads it, it executes. Asserting byte length would encode a
// convenient-sounding claim the design does not make.
func TestCompose_CommandContentFallsWithBand(t *testing.T) {
	im := impl(t, "go-repo-health", goBuildTest, "verify a Go repo",
		map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})

	countCommands := func(body string) int {
		n := 0
		for _, cmd := range []string{"go build ./...", "go test ./..."} {
			n += strings.Count(body, cmd)
		}
		return n
	}

	var prev int
	for band := BandScript; band <= BandIntent; band++ {
		c, err := Compose(im, ComposeOptions{Band: band})
		if err != nil {
			t.Fatalf("band %d: %v", band, err)
		}
		got := countCommands(c.Body)
		if band > BandScript && got > prev {
			t.Errorf("band %d carries %d concrete commands, band %d carried %d — "+
				"command content must fall as latitude rises", band, got, band-1, prev)
		}
		prev = got
	}

	// The two ends, stated directly.
	low, err := Compose(im, ComposeOptions{Band: BandScript})
	if err != nil {
		t.Fatal(err)
	}
	if countCommands(low.Body) == 0 {
		t.Error("band 0 carries no concrete command; it is supposed to be runnable")
	}
	high, err := Compose(im, ComposeOptions{Band: BandIntent})
	if err != nil {
		t.Fatal(err)
	}
	if countCommands(high.Body) != 0 {
		t.Errorf("band 4 still carries concrete commands:\n%s", high.Body)
	}
	if !strings.Contains(high.Body, "verify a Go repo") {
		t.Error("band 4 dropped the intent, which is the one thing it is for")
	}
}

// Every band renders from ONE identity: it is a cut point, not a variant.
func TestCompose_AllBandsShareOneIdentity(t *testing.T) {
	im := impl(t, "go-repo-health", goBuildTest, "d",
		map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})

	var id, capKey string
	for band := BandScript; band <= BandIntent; band++ {
		c, err := Compose(im, ComposeOptions{Band: band})
		if err != nil {
			t.Fatal(err)
		}
		if id == "" {
			id, capKey = c.Identity, c.Capability
			continue
		}
		if c.Identity != id {
			t.Errorf("band %d has a different identity; bands are cuts, not variants", band)
		}
		if c.Capability != capKey {
			t.Errorf("band %d has a different capability key", band)
		}
	}
}

// Reproducibility: same inputs, same bytes and same stamp; a changed input
// changes the stamp.
func TestCompose_Reproducible(t *testing.T) {
	im := impl(t, "go-repo-health", goBuildTest, "d",
		map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})
	opts := ComposeOptions{Band: 2, Coordinate: "cdarwin", GraphVersion: "g1"}

	a, err := Compose(im, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Compose(im, opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.Stamp != b.Stamp || a.Body != b.Body {
		t.Error("two compositions of the same inputs disagreed")
	}

	moved := opts
	moved.GraphVersion = "g2"
	c, err := Compose(im, moved)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stamp == a.Stamp {
		t.Error("a changed graph version did not change the stamp")
	}

	other := opts
	other.Coordinate = "clinux"
	d, err := Compose(im, other)
	if err != nil {
		t.Fatal(err)
	}
	if d.Stamp == a.Stamp {
		t.Error("a changed coordinate did not change the stamp")
	}
}

func TestCompose_PartialBindingsRatio(t *testing.T) {
	im := impl(t, "half", goBuildTest, "", map[string]string{"check-build": "go build ./..."})
	c, err := Compose(im, ComposeOptions{Band: -1})
	if err != nil {
		t.Fatal(err)
	}
	if c.DeterminismRatio != 0.5 {
		t.Errorf("ratio = %v, want 0.5 (one of two predicates bound)", c.DeterminismRatio)
	}
	if c.Floor == BandScript {
		t.Error("a half-bound skill must not claim a band-0 floor")
	}
}

// A contract-less skill states no guarantee, so there is nothing to compose
// against.
func TestCompose_RefusesContractlessSkill(t *testing.T) {
	im := impl(t, "nothing", noContract, "", nil)
	if _, err := Compose(im, ComposeOptions{Band: BandIntent}); err == nil {
		t.Error("a contract-less skill was composed")
	}
}
