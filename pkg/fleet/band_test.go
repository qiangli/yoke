package fleet

import (
	"testing"
	"testing/fstest"
)

// bareStore builds a catalog with an EMPTY baseline, so a test sees exactly the
// entries it wrote and nothing the shipped fleet happens to contain.
func bareStore(t *testing.T) *Catalog {
	t.Helper()
	return New(WithRoot(t.TempDir()), WithBaselineFS(fstest.MapFS{}))
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// The one that plain string ordering gets backwards, and the reason
		// this comparator exists at all.
		{"4.10", "4.8", 1},
		{"4.8", "4.10", -1},
		{"4.8", "4.8", 0},
		{"5", "4.9", 1},
		{"4", "4.1", -1}, // a shorter version is older than a longer prefix-match
		{"4.1", "4", 1},
		{"4", "4rc", 1}, // a release outranks its own candidate
		{"4rc", "4", -1},
		{"2.7", "2.6", 1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// The whole point of the family alias: ship a newer version and the bare name
// re-points itself, with nobody editing an alias list.
func TestFamilyAliasPicksHighestVersion(t *testing.T) {
	c := bareStore(t)
	for _, m := range []Model{
		{Name: "opus4.8", Family: "opus", Version: "4.8", Band: 3},
		{Name: "opus4.10", Family: "opus", Version: "4.10", Band: 3},
		{Name: "opus4.9", Family: "opus", Version: "4.9", Band: 3},
	} {
		if err := c.SaveModel(m); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := c.Model("opus")
	if !ok || got.Name != "opus4.10" {
		t.Fatalf("Model(opus) = %q, %v; want opus4.10 (4.10 > 4.9 > 4.8)", got.Name, ok)
	}
	// The versioned names keep working — the alias is an addition, not a move.
	if m, ok := c.Model("opus4.8"); !ok || m.Name != "opus4.8" {
		t.Fatalf("the exact version must still resolve: %q %v", m.Name, ok)
	}
	// And an agent bound to the newest gets the matching floating agent alias,
	// so `claude-opus` survives every release.
	if err := c.SaveAgent(Agent{Name: "claude-opus4.10", Tool: "claude", Model: "opus4.10"}); err != nil {
		t.Fatal(err)
	}
	a, ok := c.Agent("claude-opus")
	if !ok || a.MatrixKey() != "claude:opus4.10" {
		t.Fatalf("Agent(claude-opus) = %+v, %v; want the claude:opus4.10 binding", a, ok)
	}
}

// A name someone wrote down always beats a name we made up. Otherwise the
// derivation could shadow an operator's entry, and whois would answer with
// something nobody declared.
func TestDerivedNameNeverShadowsExplicit(t *testing.T) {
	c := bareStore(t)
	// A model literally CALLED `opus`, alongside a family that would derive it.
	if err := c.SaveModel(Model{Name: "opus", Band: 4}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveModel(Model{Name: "opus4.8", Family: "opus", Version: "4.8", Band: 3}); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Model("opus")
	if !ok || got.Name != "opus" || got.Band != 4 {
		t.Fatalf("Model(opus) = %+v, %v; the declared entry must win", got, ok)
	}
	// The derivation yielded rather than collided, so the catalog stays clean.
	if col := c.CheckAliases(); len(col) != 0 {
		t.Fatalf("a yielded derivation must leave no collision: %v", col)
	}
}

// Same binding, same name — on every host, every run, with no state file.
// A nickname that varied by machine would be worse than none: two operators
// would be talking about "Johnny" and meaning different agents.
func TestNicknamesAreDeterministicAndUnique(t *testing.T) {
	c := baseline(t)

	first, _ := c.Agents()
	if len(first) == 0 {
		t.Fatal("no baseline agents")
	}
	second, _ := c.Agents()

	seen := map[string]string{}
	for i, a := range first {
		nick := a.NickName()
		if nick == "" {
			t.Errorf("%s drew no nickname", a.Name)
			continue
		}
		if nick != second[i].NickName() {
			t.Errorf("%s: nickname is not stable across loads: %q then %q",
				a.Name, nick, second[i].NickName())
		}
		if prev, dup := seen[nick]; dup {
			t.Errorf("nickname %q claimed by both %s and %s", nick, prev, a.Name)
		}
		seen[nick] = a.Name
		// And it actually resolves — a name you cannot look up is decoration.
		if got, ok := c.Agent(nick); !ok || got.Name != a.Name {
			t.Errorf("Agent(%q) = %q, %v; want %s", nick, got.Name, ok, a.Name)
		}
	}
}

// An explicit nick wins over the drawn one, and the drawn one gets out of the
// way rather than colliding with it.
func TestExplicitNickWins(t *testing.T) {
	c := bareStore(t)
	if err := c.SaveModel(Model{Name: "m1", Band: 3}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveAgent(Agent{Name: "a1", Tool: "t", Model: "m1", Nick: "Bond"}); err != nil {
		t.Fatal(err)
	}
	a, ok := c.Agent("Bond")
	if !ok || a.Name != "a1" || a.NickName() != "Bond" {
		t.Fatalf("Agent(Bond) = %+v, %v", a, ok)
	}
	if col := c.CheckAliases(); len(col) != 0 {
		t.Fatalf("collisions: %v", col)
	}
}

// The identity invariant: MatrixKey is what gets written into attestations,
// judge verdicts, and the capability ledger, so it must never be built from a
// floating alias. Bind by any name you like; the catalog records the canonical
// one.
func TestBindingIsCanonicalizedHoweverItWasSpelled(t *testing.T) {
	c := bareStore(t)
	if err := c.SaveTool(Tool{Name: "claude", Kind: ToolKindCLI}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveModel(Model{Name: "fable5", Family: "fable", Version: "5", Band: 4}); err != nil {
		t.Fatal(err)
	}
	// Declared against the FAMILY alias, which is what a human would type.
	if err := c.SaveAgent(Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	a, ok := c.Agent("007")
	if !ok {
		t.Fatal("007 must resolve")
	}
	if a.Model != "fable5" || a.MatrixKey() != "claude:fable5" {
		t.Fatalf("MatrixKey = %q (model %q); a record must never key off the floating alias",
			a.MatrixKey(), a.Model)
	}
}

// The acceptance gate for the band vocabulary: every agent we ship must reach
// a model that is actually pegged. An unpegged model makes its agent invisible
// to every band roster, silently.
func TestEveryBaselineAgentResolvesToABandedModel(t *testing.T) {
	c := baseline(t)
	agents, errs := c.Agents()
	if len(errs) != 0 {
		t.Fatalf("agent parse errors: %v", errs)
	}
	if len(agents) == 0 {
		t.Fatal("no baseline agents")
	}
	for _, a := range agents {
		_, _, m, err := c.Binding(a.Name)
		if err != nil {
			t.Errorf("%s: %v", a.Name, err)
			continue
		}
		if m.Band < 1 || m.Band > MaxBand {
			t.Errorf("%s binds model %s, which is pegged at band %d (want 1-%d)",
				a.Name, m.Name, m.Band, MaxBand)
		}
	}
}

// Bands are only useful if they are not all the same. A baseline that pegged
// everything L3 would make --min-band a no-op that still looked like it worked.
func TestBaselineBandsAreSpread(t *testing.T) {
	c := baseline(t)
	models, _ := c.Models()
	bands := map[int]int{}
	for _, m := range models {
		bands[m.Band]++
	}
	for b := 1; b <= MaxBand; b++ {
		if bands[b] == 0 {
			t.Errorf("no baseline model is pegged at band L%d", b)
		}
	}
}

func TestSaveModelRejectsBandOutOfRange(t *testing.T) {
	c := bareStore(t)
	if err := c.SaveModel(Model{Name: "x", Band: 5}); err == nil {
		t.Fatal("band 5 must be rejected")
	}
	if err := c.SaveModel(Model{Name: "y", Band: 0}); err != nil {
		t.Fatalf("band 0 means unpegged, which is legal: %v", err)
	}
}

func TestBandLabel(t *testing.T) {
	for in, want := range map[int]string{0: "-", 1: "L1", 4: "L4"} {
		if got := BandLabel(in); got != want {
			t.Errorf("BandLabel(%d) = %q, want %q", in, got, want)
		}
	}
}
