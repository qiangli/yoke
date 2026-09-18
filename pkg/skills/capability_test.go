package skills

import (
	"errors"
	"strings"
	"testing"

	dhntskills "github.com/dhnt/dhnt/skills"
)

// ref builds a glossary-reference expression.
func ref(s string) dhntskills.Expr { return dhntskills.NewRef(s) }

// buildSkill is a terse constructor for the shapes these tests compare.
func buildSkill(name string, caps []dhntskills.Effect, contract []dhntskills.Check, steps ...dhntskills.Step) dhntskills.Skill {
	return dhntskills.Skill{Name: name, EffectCap: caps, Contract: contract, Steps: steps}
}

func mustKey(t *testing.T, s dhntskills.Skill) string {
	t.Helper()
	k, err := CapabilityKey(s)
	if err != nil {
		t.Fatalf("CapabilityKey(%s): %v", s.Name, err)
	}
	if !strings.HasPrefix(k, "k") {
		t.Fatalf("CapabilityKey(%s) = %q, want a \"k\" prefix", s.Name, k)
	}
	return k
}

// The core claim: what a skill guarantees is independent of how it does it, so
// name and steps must not move the key while contract and cap must.
func TestCapabilityKey_ProjectsAwayImplementation(t *testing.T) {
	contract := []dhntskills.Check{{Predicate: "builida"}, {Predicate: "gereeni"}}
	caps := []dhntskills.Effect{dhntskills.EffRead}

	base := buildSkill("gosana", caps, contract,
		dhntskills.Step{Name: "wana", Primitive: "reada"},
	)

	cases := []struct {
		name  string
		skill dhntskills.Skill
		same  bool
	}{
		{
			name:  "different name",
			skill: buildSkill("rusotana", caps, contract, dhntskills.Step{Name: "wana", Primitive: "reada"}),
			same:  true,
		},
		{
			name:  "different steps",
			skill: buildSkill("gosana", caps, contract, dhntskills.Step{Name: "tuwa", Primitive: "wurite"}),
			same:  true,
		},
		{
			name:  "no steps at all",
			skill: buildSkill("gosana", caps, contract),
			same:  true,
		},
		{
			name: "reordered contract",
			skill: buildSkill("gosana", caps,
				[]dhntskills.Check{{Predicate: "gereeni"}, {Predicate: "builida"}}),
			same: true,
		},
		{
			name: "duplicated contract clause",
			skill: buildSkill("gosana", caps,
				[]dhntskills.Check{{Predicate: "builida"}, {Predicate: "gereeni"}, {Predicate: "builida"}}),
			same: true,
		},
		{
			name:  "reordered effect cap",
			skill: buildSkill("gosana", []dhntskills.Effect{dhntskills.EffRead}, contract),
			same:  true,
		},
		{
			name: "ADDED contract clause",
			skill: buildSkill("gosana", caps,
				[]dhntskills.Check{{Predicate: "builida"}, {Predicate: "gereeni"}, {Predicate: "wireda"}}),
			same: false,
		},
		{
			name: "REMOVED contract clause",
			skill: buildSkill("gosana", caps,
				[]dhntskills.Check{{Predicate: "builida"}}),
			same: false,
		},
		{
			name: "WIDENED effect cap",
			skill: buildSkill("gosana",
				[]dhntskills.Effect{dhntskills.EffRead, dhntskills.EffWrite}, contract),
			same: false,
		},
	}

	want := mustKey(t, base)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustKey(t, tc.skill)
			if tc.same && got != want {
				t.Errorf("key changed but the guarantee did not:\n base = %s\n got  = %s", want, got)
			}
			if !tc.same && got == want {
				t.Errorf("key unchanged but the guarantee did: both = %s", got)
			}
		})
	}
}

// Contract args are NAMED bindings, so their written order carries no meaning —
// but linearisation emits them positionally, which is why they are sorted.
func TestCapabilityKey_ArgOrderIrrelevant(t *testing.T) {
	a := buildSkill("gosana", nil, []dhntskills.Check{{
		Predicate: "builida",
		Args:      []dhntskills.Arg{{Name: "wana", Value: ref("reada")}, {Name: "tuwa", Value: ref("neto")}},
	}})
	b := buildSkill("gosana", nil, []dhntskills.Check{{
		Predicate: "builida",
		Args:      []dhntskills.Arg{{Name: "tuwa", Value: ref("neto")}, {Name: "wana", Value: ref("reada")}},
	}})

	if mustKey(t, a) != mustKey(t, b) {
		t.Error("arg order changed the capability key; named bindings are a set")
	}
}

// Same arg name, different value, must not collide.
func TestCapabilityKey_ArgValuesDistinguish(t *testing.T) {
	a := buildSkill("gosana", nil, []dhntskills.Check{{
		Predicate: "builida",
		Args:      []dhntskills.Arg{{Name: "wana", Value: ref("reada")}},
	}})
	b := buildSkill("gosana", nil, []dhntskills.Check{{
		Predicate: "builida",
		Args:      []dhntskills.Arg{{Name: "wana", Value: ref("neto")}},
	}})

	if mustKey(t, a) == mustKey(t, b) {
		t.Error("different arg values produced one capability key")
	}
}

// A contract-less skill has not said what it is for. Returning a key would merge
// every such skill into one bucket — the exact opposite of the intent — so the
// refusal is load-bearing, not a convenience.
func TestCapabilityKey_RefusesWithoutContract(t *testing.T) {
	s := buildSkill("gosana", []dhntskills.Effect{dhntskills.EffRead}, nil,
		dhntskills.Step{Name: "wana", Primitive: "reada"},
	)

	got, err := CapabilityKey(s)
	if !errors.Is(err, ErrNoContract) {
		t.Fatalf("CapabilityKey with no contract: err = %v, want ErrNoContract", err)
	}
	if got != "" {
		t.Errorf("CapabilityKey with no contract returned %q, want empty", got)
	}
}

// Two skills that guarantee nothing in common must not share a key just because
// neither declares a contract. This is the failure the refusal above prevents;
// asserting it separately keeps the reasoning visible if anyone relaxes the rule.
func TestCapabilityKey_ContractlessSkillsDoNotMerge(t *testing.T) {
	a := buildSkill("gosana", nil, nil, dhntskills.Step{Name: "wana", Primitive: "reada"})
	b := buildSkill("rusotana", nil, nil, dhntskills.Step{Name: "tuwa", Primitive: "wurite"})

	ka, errA := CapabilityKey(a)
	kb, errB := CapabilityKey(b)
	if errA == nil || errB == nil {
		t.Fatalf("contract-less skills produced keys %q / %q; they must be refused", ka, kb)
	}
}

// Identity and CapabilityKey answer different questions and must never be
// confused for one another — same guarantee, different implementation.
func TestCapabilityKey_DivergesFromIdentity(t *testing.T) {
	contract := []dhntskills.Check{{Predicate: "builida"}}
	a := buildSkill("gosana", nil, contract, dhntskills.Step{Name: "wana", Primitive: "reada"})
	b := buildSkill("gosana", nil, contract, dhntskills.Step{Name: "tuwa", Primitive: "wurite"})

	idA, err := dhntskills.Identity(a)
	if err != nil {
		t.Fatalf("Identity(a): %v", err)
	}
	idB, err := dhntskills.Identity(b)
	if err != nil {
		t.Fatalf("Identity(b): %v", err)
	}

	if idA == idB {
		t.Fatal("differing steps produced one Identity; the fixture no longer tests anything")
	}
	if mustKey(t, a) != mustKey(t, b) {
		t.Error("differing steps produced two capability keys; the contract is the spine")
	}
}

// Two canonical dual-bundle reference faces. go-repo-health ensures build+test
// under a read-only cap; force-agent-shell ensures wired+forced. They are
// genuinely different capabilities and must key apart — if this ever collides,
// election would silently offer one where the other was asked for.
func TestCapabilityKey_ReferenceSkillsAreDistinct(t *testing.T) {
	const (
		goRepoHealth    = "sokilili gosana efefecato reada fini enisure builida fini enisure gereeni fini fini"
		forceAgentShell = "sokilili basoheyu efefecato reada fini enisure wireda fini enisure foroceda fini fini"
	)

	parse := func(src string) dhntskills.Skill {
		t.Helper()
		s, err := dhntskills.ParseDhnt(src)
		if err != nil {
			t.Fatalf("ParseDhnt(%q): %v", src, err)
		}
		return s
	}

	a, b := parse(goRepoHealth), parse(forceAgentShell)
	ka, kb := mustKey(t, a), mustKey(t, b)
	if ka == kb {
		t.Errorf("the two shipped skills share a capability key %s", ka)
	}

	// The payoff, on real data: a hypothetical rust-repo-health guaranteeing the
	// same build+test under the same cap IS go-repo-health's capability, and must
	// collide exactly. Election by coordinate (has=go vs has=cargo) then picks
	// between them instead of the model choosing from a menu.
	rust := parse("sokilili rusotana efefecato reada fini enisure builida fini enisure gereeni fini fini")
	if got := mustKey(t, rust); got != ka {
		t.Errorf("same contract and cap under a different name did not collide:\n go-repo-health = %s\n rust-repo-health = %s", ka, got)
	}
}

// parseDhntInfo must surface the capability alongside the identity, and must not
// treat a missing one as a broken face.
func TestParseDhntInfo_CarriesCapability(t *testing.T) {
	t.Run("with contract", func(t *testing.T) {
		info := parseDhntInfo([]byte("sokilili gosana efefecato reada fini enisure builida fini fini"))
		if !info.Valid() {
			t.Fatalf("face did not parse: %s", info.Err)
		}
		if info.Capability == "" {
			t.Error("contracted skill reported no capability key")
		}
		if info.Capability == info.Identity {
			t.Error("capability key equals identity; the projection did nothing")
		}
	})

	t.Run("without contract", func(t *testing.T) {
		info := parseDhntInfo([]byte("sokilili gosana efefecato reada fini fini"))
		if !info.Valid() {
			t.Fatalf("a contract-less face is still a VALID face; got err %q", info.Err)
		}
		if info.Capability != "" {
			t.Errorf("contract-less skill reported capability %q, want empty", info.Capability)
		}
	})
}
