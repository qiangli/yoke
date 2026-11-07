package fleet

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The verbs' defaults are seeded rosters, not Go literals: the embedded ring
// carries them, a local record reshapes them, and with seeds off the reader's
// fallback applies.
func TestRosterSeedsAndOverride(t *testing.T) {
	root := t.TempDir()
	c := New(WithRoot(root), WithoutCloudOverlay())

	weave, ok := c.Roster(RosterWeave)
	if !ok || len(weave.Members) == 0 {
		t.Fatalf("seeded weave roster missing: %+v", weave)
	}
	if got := c.RosterRole(RosterRoles, "conductor", ""); got == "" {
		t.Fatal("seeded roles roster has no conductor")
	}

	dir := filepath.Join(root, dirRosters)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "weave.yaml"), []byte("name: weave\nmembers: [genie, muse]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.RosterMembers(RosterWeave, []string{"x"}); !slices.Equal(got, []string{"genie", "muse"}) {
		t.Fatalf("local roster did not win: %v", got)
	}
}

func TestRosterSeedsOffFallsBack(t *testing.T) {
	t.Setenv(SeedsEnv, "off")
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())
	if _, ok := c.Roster(RosterWeave); ok {
		t.Fatal("seeds off still resolved the seeded weave roster")
	}
	if got := c.RosterMembers(RosterWeave, []string{"a", "b"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("fallback = %v", got)
	}
	if got := c.RosterRole(RosterRoles, "qa", "codex"); got != "codex" {
		t.Fatalf("role fallback = %q", got)
	}
}

func TestRosterValidate(t *testing.T) {
	if _, err := ParseRoster("x", []byte("members: [a, a]\n"), nil); err == nil {
		t.Fatal("duplicate member accepted")
	}
	if _, err := ParseRoster("x", []byte("members: [a, '']\n"), nil); err == nil {
		t.Fatal("empty member accepted")
	}
}
