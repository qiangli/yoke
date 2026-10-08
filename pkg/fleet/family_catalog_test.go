package fleet

// FamilyOf reads a declared configuration out of the catalog. The cases here
// are the ones where the declaration is INCOMPLETE, because that is where the
// old behaviour was wrong: it skipped what it could not resolve, which made
// the family id depend on how complete the reading host's catalog happened to
// be.

import (
	"errors"
	"testing"
)

func saveAgents(t *testing.T, c *Catalog, agents ...Agent) {
	t.Helper()
	for _, a := range agents {
		if err := c.SaveAgent(a); err != nil {
			t.Fatal(err)
		}
	}
}

func cascade(name, base string, ladder ...string) Agent {
	return Agent{Name: name, Tool: "claude", Model: "opus5.5",
		BandSource: "cascade", Base: base, Escalation: ladder}
}

// A plain entry is a single family at its declared binding.
func TestFamilyOfASingleBinding(t *testing.T) {
	c := bareStore(t)
	saveAgents(t, c, Agent{Name: "esme", Tool: "claude", Model: "opus5.5"})

	f, ok, err := c.FamilyOf("esme")
	if err != nil || !ok {
		t.Fatalf("FamilyOf(esme) = %v, %v", ok, err)
	}
	if f.Policy != PolicySingle || len(f.Bindings) != 1 || f.Composite() {
		t.Fatalf("single family = %+v", f)
	}
}

// A cascade is the predefined composite, and its ORDER is its policy — so the
// bindings come back in declared order, base first.
func TestFamilyOfACascadeKeepsDeclaredOrder(t *testing.T) {
	c := bareStore(t)
	saveAgents(t, c,
		Agent{Name: "cheap", Tool: "glm", Model: "4.9"},
		Agent{Name: "dear", Tool: "codex", Model: "gpt6-sol"},
		cascade("ladder", "cheap", "dear"),
	)
	f, ok, err := c.FamilyOf("ladder")
	if err != nil || !ok {
		t.Fatalf("FamilyOf(ladder) = %v, %v", ok, err)
	}
	if f.Policy != PolicyCascade || !f.Composite() {
		t.Fatalf("cascade family = %+v", f)
	}
	if len(f.Bindings) != 2 || f.Bindings[0] != "glm:4.9" || f.Bindings[1] != "codex:gpt6-sol" {
		t.Fatalf("bindings = %v; want the base first, then the ladder", f.Bindings)
	}
}

// THE FINDING. A rung that names nothing resolvable is an ERROR, not a
// shortened ladder. Skipping it made ID() a function of the reader's catalog,
// so two hosts computed two identities for one declared family — and the
// instance that froze the short set was missing a model it was entitled to
// select, with nothing reporting the substitution.
func TestFamilyOfRefusesAnUnresolvedRung(t *testing.T) {
	c := bareStore(t)
	saveAgents(t, c,
		Agent{Name: "cheap", Tool: "glm", Model: "4.9"},
		cascade("ladder", "cheap", "nobody-has-this-agent"),
	)
	f, ok, err := c.FamilyOf("ladder")
	if !errors.Is(err, ErrFamilyUnresolved) {
		t.Fatalf("FamilyOf over a missing rung = %+v, %v, %v; want ErrFamilyUnresolved", f, ok, err)
	}
	if !ok {
		t.Error("ok=false says \"no such agent\"; the agent exists, its configuration does not resolve")
	}
	if len(f.Bindings) != 0 {
		t.Errorf("a refused configuration still returned bindings: %v", f.Bindings)
	}
}

// An unknown NAME is a question, not a malformed answer: ok=false, no error.
func TestFamilyOfAnUnknownAgent(t *testing.T) {
	c := bareStore(t)
	f, ok, err := c.FamilyOf("nobody")
	if ok || err != nil {
		t.Fatalf("FamilyOf(nobody) = %+v, %v, %v", f, ok, err)
	}
}

// A cascade whose base names nothing is refused for the same reason, and the
// error says which rung so the operator can fix the declaration.
func TestFamilyOfRefusesAnUnresolvedBase(t *testing.T) {
	c := bareStore(t)
	saveAgents(t, c, cascade("ladder", "nobody-has-this-agent"))
	if _, _, err := c.FamilyOf("ladder"); !errors.Is(err, ErrFamilyUnresolved) {
		t.Fatalf("got %v", err)
	}
}
