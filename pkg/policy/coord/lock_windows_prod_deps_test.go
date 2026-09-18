package coord

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestWindowsBuildDoesNotLinkTesting pins a build-hygiene invariant that this
// repo's cross-OS gate structurally cannot see.
//
// scripts/crossvet.sh runs `go vet` for each target GOOS. Vet answers "does it
// compile", not "what does it link". A file whose name ends in a GOOS suffix but
// NOT in `_test` — coord_test_lock_windows.go is one — is an ordinary source
// file constrained to that GOOS. It compiles clean, vets clean, crossvets clean,
// and silently drags `testing` (and `flag`, and `flag`'s global CommandLine
// registration path) into the PRODUCTION package for that one platform.
//
// The blast radius is not local: pkg/steward imports pkg/policy/coord, so every
// Windows build reaching the steward seat inherits it. Windows is the shipping
// target — the whole premise of this repo is working where system tools do not —
// so a linux/darwin-clean result is not a clean result.
//
// This is asymmetric by construction and therefore invisible to a host-only
// suite: `go list -deps` on darwin and linux is clean; only windows is not.
func TestWindowsBuildDoesNotLinkTesting(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go tool")
	}
	const forbidden = "testing"

	// coord is where the defect lives; steward is the importer that proves it
	// propagates past the package boundary rather than staying contained.
	for _, pkg := range []string{
		"github.com/qiangli/yoke/pkg/policy/coord",
		"github.com/qiangli/yoke/pkg/steward",
	} {
		for _, goos := range []string{"windows", "linux", "darwin"} {
			cmd := exec.Command("go", "list", "-deps", pkg)
			cmd.Env = append(cmd.Environ(), "GOOS="+goos, "CGO_ENABLED=0")
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("go list -deps %s (GOOS=%s): %v", pkg, goos, err)
			}
			if slices.Contains(strings.Fields(string(out)), forbidden) {
				t.Errorf("GOOS=%s: production package %s links %q — a non-_test source "+
					"file is pulling the test framework into a shipped build; "+
					"GOOS=linux and GOOS=darwin do not, so this is a platform-only "+
					"regression that go vet and scripts/crossvet.sh both pass",
					goos, pkg, forbidden)
			}
		}
	}
}
