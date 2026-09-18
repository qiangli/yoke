package weave

import (
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestSprintClaimIdentityReturnsCanonicalAgentForAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	if err := fleet.New().SaveAgent(fleet.Agent{
		Name: "canonical-manager", Aliases: []string{"ManagerAlias"}, Tool: "codex", Model: "gpt5.6-sol",
	}); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "add", "external alias watch"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}

	got, err := SprintClaimIdentity(1, "ManagerAlias", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "canonical-manager" {
		t.Fatalf("claim identity = %q, want canonical-manager", got)
	}
}
