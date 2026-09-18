package craft

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	dhntskills "github.com/dhnt/dhnt/skills"

	"github.com/qiangli/yoke/pkg/skills"
)

// Canonical faces used across these tests. buildTest and rustBuildTest make the
// SAME guarantee (build ∧ test, read-only) by different names — the whole point
// of capability keying. wired makes a different one.
const (
	goBuildTest   = "sokilili gosana efefecato reada fini enisure builida fini enisure gereeni fini fini"
	rustBuildTest = "sokilili rusotana efefecato reada fini enisure builida fini enisure gereeni fini fini"
	wiredForced   = "sokilili basoheyu efefecato reada fini enisure wireda fini enisure foroceda fini fini"
	noContract    = "sokilili sinakota efefecato reada fini fini"
)

func candidate(name, canon, license string) Candidate {
	return Candidate{
		Name:      name,
		Canonical: canon,
		Source:    Source{Origin: "github.com/example/skills", License: license, Ref: "abc123"},
	}
}

func dispositions(r Report) map[string]Disposition {
	out := map[string]Disposition{}
	for _, o := range r.Outcomes {
		out[o.Name] = o.Disposition
	}
	return out
}

// THE CLAIM. Absorbing skills that make the same guarantee must grow the
// catalog once, not once per skill. A 1:1 ratio means nothing was digested.
func TestStudy_DigestsRatherThanAccumulates(t *testing.T) {
	cands := []Candidate{
		candidate("go-repo-health", goBuildTest, "Apache-2.0"),
		candidate("rust-repo-health", rustBuildTest, "MIT"),
		candidate("force-agent-shell", wiredForced, "Apache-2.0"),
	}

	rep := Study(cands, nil, nil)

	if rep.Absorbed != 3 {
		t.Fatalf("Absorbed = %d, want 3", rep.Absorbed)
	}
	// Three skills, two distinct guarantees.
	if rep.Grew != 2 {
		t.Errorf("capabilities added = %d, want 2 — build+test is ONE capability with two implementations", rep.Grew)
	}
	if got := rep.DigestionRatio(); got >= 1.0 {
		t.Errorf("digestion ratio = %v; at 1.0 nothing was digested", got)
	}

	d := dispositions(rep)
	if d["go-repo-health"] != DispNovel {
		t.Errorf("go-repo-health = %s, want novel", d["go-repo-health"])
	}
	if d["rust-repo-health"] != DispAlternative {
		t.Errorf("rust-repo-health = %s, want alternative — same contract, different implementation", d["rust-repo-health"])
	}
	if d["force-agent-shell"] != DispNovel {
		t.Errorf("force-agent-shell = %s, want novel — a different guarantee", d["force-agent-shell"])
	}
}

// A candidate absorbed earlier in the same run must be visible to later ones,
// or two identical inputs would both report novel and the ratio would lie.
func TestStudy_WithinRunAccumulation(t *testing.T) {
	rep := Study([]Candidate{
		candidate("a", goBuildTest, "MIT"),
		candidate("b", goBuildTest, "MIT"),
	}, nil, nil)

	if rep.Grew != 1 {
		t.Errorf("capabilities added = %d, want 1", rep.Grew)
	}
	if d := dispositions(rep); d["b"] != DispDuplicate {
		t.Errorf("b = %s, want duplicate — byte-identical to a", d["b"])
	}
}

// Absorption must not mutate the caller's catalog view.
func TestStudy_DoesNotMutateKnown(t *testing.T) {
	known := map[string][]string{"kexisting": {"hsomething"}}
	Study([]Candidate{candidate("x", goBuildTest, "MIT")}, known, nil)

	if len(known) != 1 {
		t.Errorf("caller's map was mutated: %v", known)
	}
	if len(known["kexisting"]) != 1 {
		t.Errorf("caller's slice was mutated: %v", known["kexisting"])
	}
}

// Already holding the capability means an incoming implementation is an
// alternative, not growth.
func TestStudy_AgainstExistingCatalog(t *testing.T) {
	sk, err := dhntskills.ParseDhnt(goBuildTest)
	if err != nil {
		t.Fatal(err)
	}
	key, err := capabilityOf(sk)
	if err != nil {
		t.Fatal(err)
	}

	rep := Study([]Candidate{candidate("rust-repo-health", rustBuildTest, "MIT")},
		map[string][]string{key: {"hpre-existing"}}, nil)

	if rep.Grew != 0 {
		t.Errorf("capabilities added = %d, want 0 — the guarantee was already held", rep.Grew)
	}
	if d := dispositions(rep); d["rust-repo-health"] != DispAlternative {
		t.Errorf("got %s, want alternative", d["rust-repo-health"])
	}
}

// FAIL-CLOSED. Absence of a license is not permission.
func TestStudy_LicenseGate(t *testing.T) {
	tests := []struct {
		license string
		want    Disposition
	}{
		{"MIT", DispNovel},
		{"Apache-2.0", DispNovel},
		{"BSD-3-Clause", DispNovel},
		{"ISC", DispNovel},
		{"", DispRefused},
		{"GPL-3.0", DispRefused},
		{"AGPL-3.0", DispRefused},
		{"proprietary", DispRefused},
		{"UNKNOWN", DispRefused},
	}
	for _, tc := range tests {
		t.Run("license="+tc.license, func(t *testing.T) {
			rep := Study([]Candidate{candidate("s", goBuildTest, tc.license)}, nil, nil)
			got := rep.Outcomes[0]
			if got.Disposition != tc.want {
				t.Errorf("disposition = %s, want %s", got.Disposition, tc.want)
			}
			if tc.want == DispRefused {
				if got.Reason == "" {
					t.Error("a refusal must carry a reason; a silent exclusion is unauditable")
				}
				// A refused candidate is never counted as absorbed, or the
				// digestion ratio would be diluted by content we never took.
				if rep.Absorbed != 0 {
					t.Errorf("Absorbed = %d for a refused candidate, want 0", rep.Absorbed)
				}
			}
		})
	}
}

// Without a normaliser, prose is held — never guessed at. Inventing a contract
// would file a skill under a promise nobody verified.
func TestStudy_ProseQuarantinedWithoutNormaliser(t *testing.T) {
	c := Candidate{
		Name:        "prose-only",
		Description: "does a thing",
		Body:        "# prose-only\n\nSome instructions.\n",
		Source:      Source{License: "MIT"},
	}

	rep := Study([]Candidate{c}, nil, nil)
	got := rep.Outcomes[0]
	if got.Disposition != DispQuarantined {
		t.Errorf("disposition = %s, want quarantined", got.Disposition)
	}
	if got.Reason == "" {
		t.Error("quarantine must carry a reason")
	}
	if rep.Grew != 0 {
		t.Errorf("a quarantined candidate grew the catalog by %d", rep.Grew)
	}
}

// With a normaliser, prose becomes typed and resolves like any other candidate.
func TestStudy_NormaliserPromotesProse(t *testing.T) {
	norm := func(c Candidate) (dhntskills.Skill, error) {
		return dhntskills.ParseDhnt(goBuildTest)
	}
	c := Candidate{Name: "prose-only", Body: "prose", Source: Source{License: "MIT"}}

	rep := Study([]Candidate{c}, nil, norm)
	if got := rep.Outcomes[0].Disposition; got != DispNovel {
		t.Errorf("disposition = %s, want novel", got)
	}
	if rep.Grew != 1 {
		t.Errorf("capabilities added = %d, want 1", rep.Grew)
	}
}

// A normaliser that fails quarantines the candidate — it never fabricates.
func TestStudy_NormaliserFailureQuarantines(t *testing.T) {
	norm := func(c Candidate) (dhntskills.Skill, error) {
		return dhntskills.Skill{}, errors.New("model produced no parsable face")
	}
	c := Candidate{Name: "prose-only", Body: "prose", Source: Source{License: "MIT"}}

	rep := Study([]Candidate{c}, nil, norm)
	if got := rep.Outcomes[0].Disposition; got != DispQuarantined {
		t.Errorf("disposition = %s, want quarantined", got)
	}
}

// An author's own canonical face beats normalisation — re-deriving it through a
// model could only lose fidelity.
func TestStudy_ShippedFaceBeatsNormaliser(t *testing.T) {
	called := false
	norm := func(c Candidate) (dhntskills.Skill, error) {
		called = true
		return dhntskills.ParseDhnt(wiredForced)
	}

	rep := Study([]Candidate{candidate("s", goBuildTest, "MIT")}, nil, norm)
	if called {
		t.Error("the normaliser ran even though the candidate ships its own face")
	}
	sk, _ := dhntskills.ParseDhnt(goBuildTest)
	want, _ := capabilityOf(sk)
	if got := rep.Outcomes[0].Capability; got != want {
		t.Errorf("capability = %s, want the shipped face's %s", got, want)
	}
}

// A typed skill with no contract states no guarantee, and must be held rather
// than merged into a shared "no contract" bucket.
func TestStudy_ContractlessSkillQuarantined(t *testing.T) {
	rep := Study([]Candidate{
		candidate("a", noContract, "MIT"),
		candidate("b", noContract, "MIT"),
	}, nil, nil)

	for _, o := range rep.Outcomes {
		if o.Disposition != DispQuarantined {
			t.Errorf("%s = %s, want quarantined", o.Name, o.Disposition)
		}
	}
	if rep.Grew != 0 {
		t.Errorf("contract-less skills grew the catalog by %d", rep.Grew)
	}
}

func TestStudy_RefusesUnparsableFace(t *testing.T) {
	rep := Study([]Candidate{candidate("bad", "this is not dhnt at all!!", "MIT")}, nil, nil)
	got := rep.Outcomes[0]
	if got.Disposition != DispQuarantined {
		t.Errorf("disposition = %s, want quarantined", got.Disposition)
	}
	if got.Reason == "" {
		t.Error("missing reason")
	}
}

func TestStudy_RefusesNamelessCandidate(t *testing.T) {
	rep := Study([]Candidate{{Source: Source{License: "MIT"}}}, nil, nil)
	if got := rep.Outcomes[0].Disposition; got != DispRefused {
		t.Errorf("disposition = %s, want refused", got)
	}
}

func TestDigestionRatio_EmptyIsZeroNotNaN(t *testing.T) {
	if got := (Report{}).DigestionRatio(); got != 0 {
		t.Errorf("DigestionRatio on an empty report = %v, want 0", got)
	}
}

func TestLoadDir(t *testing.T) {
	root := t.TempDir()

	write := func(name, skillmd, canon string) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillmd), 0o644); err != nil {
			t.Fatal(err)
		}
		if canon != "" {
			if err := os.WriteFile(filepath.Join(dir, "skill.dhnt"), []byte(canon), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	write("alpha", "---\nname: alpha\ndescription: the first\n---\nbody\n", goBuildTest)
	write("beta", "---\nname: beta\ndescription: the second\n---\nbody\n", "")
	// A dot-directory is tooling, not a skill.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory with no SKILL.md is not a candidate.
	if err := os.MkdirAll(filepath.Join(root, "notaskill"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDir(root, Source{Origin: "example", License: "MIT", Path: "skills"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d candidates, want 2: %+v", len(got), got)
	}
	if got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Errorf("names = %q/%q, want alpha/beta in sorted order", got[0].Name, got[1].Name)
	}
	if got[0].Canonical == "" {
		t.Error("alpha's shipped canonical face was not loaded")
	}
	if got[1].Canonical != "" {
		t.Error("beta has no canonical face but one was reported")
	}
	if got[0].Description != "the first" {
		t.Errorf("description = %q", got[0].Description)
	}
	if got[0].Source.License != "MIT" || got[0].Source.Path != filepath.Join("skills", "alpha") {
		t.Errorf("source = %+v", got[0].Source)
	}
}

// capabilityOf is a test helper mirroring what studyOne does.
func capabilityOf(sk dhntskills.Skill) (string, error) {
	return skills.CapabilityKey(sk)
}
