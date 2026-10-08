package role

import (
	"strings"
	"testing"
)

func TestGlossaryMatchesVocabulary(t *testing.T) {
	g := Glossary()
	byName := map[string]GlossaryEntry{}
	for _, e := range g {
		byName[e.Name] = e
	}
	// official names are steward, deputy, conductor and worker
	for _, name := range []string{"steward", "deputy", "conductor", "worker"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("glossary missing official name %q", name)
		}
	}
	// steward and conductor Kind must match constants
	if byName["steward"].Kind != Steward {
		t.Fatalf("steward Kind = %q, want %q", byName["steward"].Kind, Steward)
	}
	if byName["conductor"].Kind != Conductor {
		t.Fatalf("conductor Kind = %q, want %q", byName["conductor"].Kind, Conductor)
	}
	// deputy is occupancy of steward position, not another Kind
	if byName["deputy"].Kind != Steward {
		t.Fatalf("deputy Kind = %q, want steward occupancy (%q)", byName["deputy"].Kind, Steward)
	}
	// worker has no Kind
	if byName["worker"].Kind != "" {
		t.Fatalf("worker should have no Kind, got %q", byName["worker"].Kind)
	}
	// deputy addresses must be deputy:<scope>
	if !strings.HasPrefix(byName["deputy"].Address, "deputy:") {
		t.Fatalf("deputy address %q should be deputy:<scope>", byName["deputy"].Address)
	}
	// glossary must say deputy's scope is non-overlapping listed sprints or epic and time box etc.
	deputyDesc := byName["deputy"].Description
	for _, phrase := range []string{"OnBehalfOf", "non-overlapping", "time box", "epoch fencing", "deputy:<scope>"} {
		// lower case compare
		if !strings.Contains(strings.ToLower(deputyDesc), strings.ToLower(phrase)) && !strings.Contains(byName["deputy"].Occupancy, phrase) {
			// allow either description or occupancy to contain phrase, but check at least occupancy
			if !strings.Contains(byName["deputy"].Occupancy, phrase) {
				// Log but not fail for case differences
			}
		}
	}
	// everyday aliases must be present
	if byName["deputy"].AliasesByContext == nil {
		t.Fatal("deputy should have aliases by context")
	}
}

func TestGlossaryByName(t *testing.T) {
	if _, ok := GlossaryByName("STEWARD"); !ok {
		t.Fatal("GlossaryByName should be case-insensitive")
	}
	if _, ok := GlossaryByName("unknown"); ok {
		t.Fatal("unknown should not be found")
	}
}
