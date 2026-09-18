package graphcmd

import (
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/execlog"
	"github.com/qiangli/yoke/pkg/skills"
)

// TestStorePathsResolveThroughOwners pins the two graph stores to the
// packages that OWN them. Until 2026-09-12 spaceStoreDir carried its own
// ladder ending in ~/.bashy/skills while the writer used the skills store —
// so `graph space` read an empty directory on every host. One ladder each.
func TestStorePathsResolveThroughOwners(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SKILLS_DIR", "")
	t.Setenv("BASHY_EXECHIST", "")

	if got, want := spaceStoreDir(), skills.DefaultStoreDir(); got != want {
		t.Fatalf("spaceStoreDir=%q, skills.DefaultStoreDir=%q", got, want)
	}
	if got, want := spaceStoreDir(), filepath.Join(home, "skills"); got != want {
		t.Fatalf("spaceStoreDir under BASHY_HOME=%q, want %q", got, want)
	}
	if got, want := execStoreRoot(), execlog.DefaultRoot(); got != want {
		t.Fatalf("execStoreRoot=%q, execlog.DefaultRoot=%q", got, want)
	}

	// The specific overrides still win over the relocated home.
	t.Setenv("BASHY_SKILLS_DIR", "/x/skills")
	t.Setenv("BASHY_EXECHIST", "/x/exec")
	if spaceStoreDir() != "/x/skills" || execStoreRoot() != "/x/exec" {
		t.Fatalf("overrides not honoured: %q %q", spaceStoreDir(), execStoreRoot())
	}
	// A boolean BASHY_EXECHIST is the on/off switch, never a path.
	t.Setenv("BASHY_EXECHIST", "1")
	if got, want := execStoreRoot(), filepath.Join(home, "exec"); got != want {
		t.Fatalf("BASHY_EXECHIST=1 treated as a path: %q", got)
	}
}
