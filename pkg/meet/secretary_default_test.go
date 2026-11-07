package meet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
)

// isolatedMeetCatalog pins the secretary default to an embedded-baseline
// catalog rooted at an empty temp dir: no developer-local or cloud overlay.
func isolatedMeetCatalog(t *testing.T) *fleet.Catalog {
	t.Helper()
	t.Setenv("BASHY_ROSTERS_PATH", "")
	return fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
}

func TestMeetSecretaryDefaultFromSeededRoster(t *testing.T) {
	prev := meetCatalog
	meetCatalog = func() *fleet.Catalog { return isolatedMeetCatalog(t) }
	defer func() { meetCatalog = prev }()
	if got := meetSecretaryDefault(); got != "claude" {
		t.Fatalf("meetSecretaryDefault=%q, want claude (seeded roles/meet-secretary)", got)
	}
}

func TestMeetSecretaryDefaultSeedsOffFallsBack(t *testing.T) {
	t.Setenv(fleet.SeedsEnv, "off")
	prev := meetCatalog
	meetCatalog = func() *fleet.Catalog { return isolatedMeetCatalog(t) }
	defer func() { meetCatalog = prev }()
	if got := meetSecretaryDefault(); got != meetSecretaryFallback {
		t.Fatalf("meetSecretaryDefault=%q, want fallback %q with seeds off", got, meetSecretaryFallback)
	}
}

func TestMeetSecretaryDefaultReshapedRosterWins(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_ROSTERS_PATH", "")
	dir := filepath.Join(root, "rosters")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roles.yaml"),
		[]byte("name: roles\nroles:\n  meet-secretary: opencode\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := meetCatalog
	meetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root), fleet.WithoutCloudOverlay()) }
	defer func() { meetCatalog = prev }()
	if got := meetSecretaryDefault(); got != "opencode" {
		t.Fatalf("meetSecretaryDefault=%q, want opencode (reshaped local roster)", got)
	}
}

// resolveSecretaryDefault applies the roster only when the caller left the
// flag at its default; an explicit spelling — even one equal to the fallback
// — and an explicit empty (a room that keeps no minutes) are kept as given.
func TestResolveSecretaryDefault(t *testing.T) {
	prev := meetCatalog
	meetCatalog = func() *fleet.Catalog { return isolatedMeetCatalog(t) }
	defer func() { meetCatalog = prev }()

	// Each case binds a bare command: binding the real `meet open` tree
	// twice would panic on flag redefinition.
	setup := func(t *testing.T, set string, setIt bool) sessionFlags {
		t.Helper()
		open := &cobra.Command{Use: "open"}
		var sf sessionFlags
		sf.bind(open)
		if setIt {
			if err := open.Flags().Set("secretary", set); err != nil {
				t.Fatal(err)
			}
		}
		sf.resolveSecretaryDefault(open)
		return sf
	}

	if sf := setup(t, "", false); sf.secretary != "claude" {
		t.Fatalf("default secretary=%q, want claude", sf.secretary)
	}
	if sf := setup(t, "codex", true); sf.secretary != "codex" {
		t.Fatalf("explicit secretary=%q, want codex kept", sf.secretary)
	}
	if sf := setup(t, "", true); sf.secretary != "" {
		t.Fatalf("explicit empty secretary=%q, want empty kept (no minutes)", sf.secretary)
	}
}
