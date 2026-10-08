package role

import (
	"slices"
	"testing"
)

func TestGlossaryMatchesVocabulary(t *testing.T) {
	var names []string
	for _, e := range Glossary() {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"steward", "deputy", "conductor", "worker"}) {
		t.Fatalf("official names = %v", names)
	}
	for _, k := range Kinds() {
		e, ok := GlossaryByName(string(k))
		if !ok || e.Kind != k || e.Occupancy {
			t.Fatalf("%s entry = %+v", k, e)
		}
	}
	dep, _ := GlossaryByName("Deputy")
	if dep.Kind != Steward || !dep.Occupancy || dep.Address != "deputy:<scope>" {
		t.Fatalf("deputy must be an occupancy of steward, got %+v", dep)
	}
	if w, _ := GlossaryByName("worker"); w.Kind != "" {
		t.Fatalf("worker holds no Kind: %+v", w)
	}
	if c, _ := GlossaryByName("conductor"); c.Address != (Assignment{Kind: Conductor, Ref: "<sprint>"}).Label() {
		t.Fatalf("conductor address drifted from Assignment.Label: %q", c.Address)
	}
	if s, _ := GlossaryByName("steward"); s.Address != (Assignment{Kind: Steward}).Label() {
		t.Fatalf("steward address drifted: %q", s.Address)
	}
	for _, e := range Glossary() {
		if len(e.AliasContexts()) == 0 {
			t.Fatalf("%s has no everyday words", e.Name)
		}
	}
	if _, ok := GlossaryByName("director"); ok {
		t.Fatal("no fourth role")
	}
}
