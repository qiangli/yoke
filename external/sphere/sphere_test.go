package sphere

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestSubVerbs(t *testing.T) {
	for _, v := range []string{"mesh", "shard", "pool", "peers"} {
		if _, ok := subVerbs[v]; !ok {
			t.Errorf("sphere is missing sub-verb %q", v)
		}
	}
	if _, ok := subVerbs["cluster"]; ok {
		t.Error("cluster is tier 5 — must not be a sphere sub-verb")
	}
}

func TestResolveOutpostReExport(t *testing.T) {
	// Delegates to meshagent.Resolve; a clean env with no outpost resolves false.
	t.Setenv("OUTPOST_BIN", "")
	// (Can't assert absence portably since a host may have outpost on PATH; just
	// ensure the call is wired and returns without panic.)
	_, _ = ResolveOutpost()
}

func TestPeerHelpTeachesCanonicalSpellings(t *testing.T) {
	cmd := NewSphereCmd()
	if cmd.Use != "peer" {
		t.Fatalf("cmd.Use = %q, want %q", cmd.Use, "peer")
	}
	if !slices.Contains(cmd.Aliases, "sphere") {
		t.Fatalf("cmd.Aliases = %v, want to contain %q", cmd.Aliases, "sphere")
	}

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := cmd.RunE(cmd, []string{"--help"}); err != nil {
		t.Fatalf("help failed: %v", err)
	}
	help := buf.String()

	// Must teach canonical spellings
	if !strings.Contains(help, "peer is dhnt execution tier 4") {
		t.Errorf("help missing canonical intro: %s", help)
	}
	for _, sub := range []string{"mesh", "shard", "pool", "peers", "status"} {
		wantExample := "bashy peer " + sub
		if !strings.Contains(help, wantExample) {
			t.Errorf("help missing canonical example %q:\n%s", wantExample, help)
		}
	}

	// Must not teach non-canonical command spellings
	if strings.Contains(help, "bashy sphere") {
		t.Errorf("help contains non-canonical command spelling 'bashy sphere':\n%s", help)
	}
	if strings.Contains(help, "sphere is dhnt") {
		t.Errorf("help contains non-canonical prose 'sphere is dhnt':\n%s", help)
	}

	// Keep 'sphere' only where it names the tier concept
	if !strings.Contains(cmd.Short, "the sphere tier") {
		t.Errorf("Short missing tier concept 'the sphere tier': %q", cmd.Short)
	}
	if !strings.Contains(help, "p2p sphere") {
		t.Errorf("help missing tier concept 'p2p sphere': %s", help)
	}

	// joinLines must also teach canonical spelling and preserve tier concept
	if strings.Contains(joinLines, "bashy sphere") {
		t.Errorf("joinLines contains non-canonical command spelling 'bashy sphere':\n%s", joinLines)
	}
	if !strings.Contains(joinLines, "bashy peer") {
		t.Errorf("joinLines missing canonical command spelling 'bashy peer':\n%s", joinLines)
	}
	if !strings.Contains(joinLines, "The sphere tier") {
		t.Errorf("joinLines missing tier concept 'The sphere tier':\n%s", joinLines)
	}
}

func TestSphereAliasWorks(t *testing.T) {
	cmd := NewSphereCmd()
	root := &cobra.Command{Use: "bashy"}
	root.AddCommand(cmd)

	peerTarget, _, err := root.Find([]string{"peer"})
	if err != nil || peerTarget.Name() != "peer" {
		t.Fatalf("root.Find(peer) = %v, err = %v", peerTarget, err)
	}

	sphereTarget, _, err := root.Find([]string{"sphere"})
	if err != nil || sphereTarget.Name() != "peer" {
		t.Fatalf("root.Find(sphere) = %v, err = %v", sphereTarget, err)
	}
}
