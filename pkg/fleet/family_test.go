package fleet

import (
	"errors"
	"testing"
)

// Two bindings a human calls "claude" are two families. If they were one, an
// instance opened on Sonnet could be resumed on Opus with its conversation
// intact and nothing would report the substitution.
func TestFamilyIDSeparatesModelsOfOneVendor(t *testing.T) {
	opus := Family{Name: "esme", Policy: PolicySingle, Bindings: []string{"claude:opus5.5"}}
	sonnet := Family{Name: "esme", Policy: PolicySingle, Bindings: []string{"claude:sonnet5"}}
	if opus.ID() == sonnet.ID() {
		t.Fatalf("claude:opus5.5 and claude:sonnet5 share family id %s", opus.ID())
	}
}

// The ID is a function of the configuration, so two hosts reading the same
// configuration agree without syncing anything, and a set written in two
// orders is one set.
func TestFamilyIDIsDeterministicAndOrderIndependent(t *testing.T) {
	a := Family{Name: "ladder", Policy: PolicyCascade, Bindings: []string{"claude:opus5.5", "codex:gpt6-sol"}}
	b := Family{Name: "ladder", Policy: PolicyCascade, Bindings: []string{"codex:gpt6-sol", "claude:opus5.5"}}
	if a.ID() != b.ID() {
		t.Fatalf("binding order changed the family id: %s vs %s", a.ID(), b.ID())
	}
	if a.ID() != a.ID() {
		t.Fatal("family id is not stable")
	}
}

// Changing the model set, the policy or the declared version each MINT a new
// family. This is the mechanism behind "binding changes require handoff": the
// instance's frozen FamilyID simply stops matching.
func TestFamilyIDChangesWithEveryConfigurationChange(t *testing.T) {
	base := Family{Name: "ladder", Policy: PolicyCascade, Bindings: []string{"claude:opus5.5", "codex:gpt6-sol"}}
	for name, changed := range map[string]Family{
		"model set added":   {Name: "ladder", Policy: PolicyCascade, Bindings: []string{"claude:opus5.5", "codex:gpt6-sol", "glm:4.9"}},
		"model set removed": {Name: "ladder", Policy: PolicyCascade, Bindings: []string{"claude:opus5.5"}},
		"policy":            {Name: "ladder", Policy: "roundrobin", Bindings: []string{"claude:opus5.5", "codex:gpt6-sol"}},
		"declared version":  {Name: "ladder", Policy: PolicyCascade, Bindings: []string{"claude:opus5.5", "codex:gpt6-sol"}, Version: "2"},
	} {
		if changed.ID() == base.ID() {
			t.Errorf("%s left the family id unchanged (%s)", name, base.ID())
		}
	}
}

// Selecting INSIDE a predefined composite is allowed — that is the whole
// reason a composite is one family and not several. Selecting outside it is a
// reconfiguration and gets the immutability refusal.
func TestFamilySelectAllowsOnlyTheFrozenSet(t *testing.T) {
	f := Family{Name: "ladder", Policy: PolicyCascade, Bindings: []string{"claude:opus5.5", "codex:gpt6-sol"}}
	if got, err := f.Select("codex:gpt6-sol"); err != nil || got != "codex:gpt6-sol" {
		t.Fatalf("selecting a member of the set: got %q, %v", got, err)
	}
	if _, err := f.Select("glm:4.9"); !errors.Is(err, ErrBindingImmutable) {
		t.Fatalf("selecting outside the set: want ErrBindingImmutable, got %v", err)
	}
}

func TestFamilyEmptyIsRefused(t *testing.T) {
	if _, err := (Family{Name: "nothing"}).Select(""); !errors.Is(err, ErrFamilyEmpty) {
		t.Fatalf("want ErrFamilyEmpty, got %v", err)
	}
}
