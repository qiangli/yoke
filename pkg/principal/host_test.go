// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package principal

import (
	"os"
	"path/filepath"
	"testing"
)

// A host paired before the outpost rename keeps its config under
// ~/.config/matrix; reading only the new path reported it unpaired (found by
// the apps console's Cloud section, sprint 220).
func TestDefaultEnvSeesALegacyMatrixPairing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "matrix"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "matrix", "agent.json"), []byte(`{"agent_name":"oldhost"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := DefaultEnv()
	if !e.Paired || e.PairedName != "oldhost" {
		t.Fatalf("legacy pairing not seen: paired=%v name=%q", e.Paired, e.PairedName)
	}
	// The outpost path wins when both exist.
	if err := os.MkdirAll(filepath.Join(home, ".config", "outpost"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "outpost", "agent.json"), []byte(`{"agent_name":"newhost"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if e := DefaultEnv(); e.PairedName != "newhost" {
		t.Fatalf("outpost path did not win: %q", e.PairedName)
	}
}
