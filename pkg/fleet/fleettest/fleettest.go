// Package fleettest mounts the test ring: the models and agents that used to
// ship compiled into pkg/fleet, now kept under pkg/fleet/testdata/ring so a
// test that needs a known tool:model binding still has one after the
// embedded baseline shrank to tools only.
//
// It is deliberately stdlib-only and does not import pkg/fleet, so fleet's
// own internal tests can use it without an import cycle. It reaches the
// catalog the same way an operator's overlay does — through the read-only
// shared-dir PATH lists — so it exercises the production ring order rather
// than a test-only seam.
package fleettest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ringDir is the test ring's root, resolved from this file's own location so
// a test in any package finds it regardless of its working directory.
func ringDir(t testing.TB) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("fleettest: cannot locate the test ring")
	}
	dir := filepath.Join(filepath.Dir(self), "..", "testdata", "ring")
	for _, noun := range []string{"models", "agents"} {
		if _, err := os.Stat(filepath.Join(dir, noun)); err != nil {
			t.Fatalf("fleettest: test ring is missing %s: %v", noun, err)
		}
	}
	return dir
}

// Ring points $BASHY_MODELS_PATH and $BASHY_AGENTS_PATH at the test ring for
// the duration of t and fences the local store onto a fresh t.TempDir(), which
// it returns. Call it before any per-test store override: it clears the
// per-noun $BASHY_*_DIR redirects so ambient environment can never route a
// test's writes into the operator's real store.
//
// It also switches the seeded roster off ($BASHY_FLEET_SEEDS=off), so the
// fixture roster is the WHOLE roster: a family alias like `fable` resolves to
// the fixture's fable5 no matter what the shipped seeds add above it. The tool
// launch contracts stay embedded — the switch never drops those.
func Ring(t testing.TB) string {
	t.Helper()
	dir := ringDir(t)
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	t.Setenv("BASHY_MODELS_PATH", filepath.Join(dir, "models"))
	t.Setenv("BASHY_AGENTS_PATH", filepath.Join(dir, "agents"))
	root := t.TempDir()
	t.Setenv("BASHY_FLEET_DIR", root)
	t.Setenv("BASHY_COMMANDS_PATH", "")
	for _, key := range []string{"BASHY_TOOLS_DIR", "BASHY_MODELS_DIR", "BASHY_AGENTS_DIR", "BASHY_PEOPLE_DIR", "BASHY_HOSTS_DIR", "BASHY_COMMANDS_DIR"} {
		t.Setenv(key, "")
	}
	return root
}

// Dir returns the test ring's directory for one noun ("models" or "agents"),
// for tests that read the ring's files directly rather than through a catalog.
func Dir(t testing.TB, noun string) string {
	t.Helper()
	return filepath.Join(ringDir(t), noun)
}
