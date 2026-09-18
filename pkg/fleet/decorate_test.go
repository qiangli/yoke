package fleet_test

import (
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// TestFamilyAliasSkipsCascades — a cascade agent carries its BASE's model,
// so it competes for the floating family alias on a model it does not run:
// `ycode-glm` must mean "the newest glm on ycode", never a cascade whose
// base happens to be glm and which escalates elsewhere.
//
// The regression this pins (measured before glm-5.3 landed): whois
// ycode-glm resolved to ycode-cascade-claude-x3 (base glm-5.2, escalates to
// opus) instead of the plain ycode-glm binding, because the cascade sorted
// ahead of it in canonical-name order and the family-alias pass had no
// cascade exclusion.
func TestFamilyAliasSkipsCascadesAndClones(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))

	for _, m := range []fleet.Model{
		{Name: "glm-5.2", Family: "glm", Version: "5.2"},
		{Name: "glm-5.3", Family: "glm", Version: "5.3"},
	} {
		if err := cat.SaveModel(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.SaveTool(fleet.Tool{Name: "ycode"}); err != nil {
		t.Fatal(err)
	}
	// Cascade, alphabetically AHEAD of the plain agent, carrying the same
	// newest model as its base. The buggy pass handed it the alias.
	if err := cat.SaveAgent(fleet.Agent{
		Name: "ycode-cascade-claude-x3", Tool: "ycode", Model: "glm-5.3",
		Base: "ycode-glm-5.3", Band: 3, BandSource: "cascade",
	}); err != nil {
		t.Fatal(err)
	}
	// An ephemeral evaluation clone also sorts ahead of the plain binding.
	// It must not make the floating family alias point at a temporary seat.
	if err := cat.SaveAgent(fleet.Agent{
		Name: "glm53-lead-judge", Tool: "ycode", Model: "glm-5.3",
		ClonedFrom: "ycode-glm-5.3", Ephemeral: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Plain binding on the newest glm — where the alias belongs.
	if err := cat.SaveAgent(fleet.Agent{
		Name: "ycode-glm-5.3", Tool: "ycode", Model: "glm-5.3",
	}); err != nil {
		t.Fatal(err)
	}

	agents, errs := cat.Agents()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	for _, a := range agents {
		if (a.IsCascade() || a.ClonedFrom != "" || a.Ephemeral) && len(a.Derived) != 0 {
			t.Errorf("non-plain agent %s derived %v; want none", a.Name, a.Derived)
		}
		if a.Name == "ycode-glm-5.3" &&
			(len(a.Derived) != 1 || a.Derived[0] != "ycode-glm") {
			t.Errorf("plain agent derived %v; want [ycode-glm]", a.Derived)
		}
	}

	// The resolution whois performs: the alias must land on the plain agent.
	got, ok := cat.Agent("ycode-glm")
	if !ok {
		t.Fatal("ycode-glm does not resolve")
	}
	if got.Name != "ycode-glm-5.3" {
		t.Errorf("ycode-glm resolves to %s; want ycode-glm-5.3", got.Name)
	}
}
