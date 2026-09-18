package fleet_test

import (
	"os/exec"
	"strings"
	"testing"
)

// pkg/fleet is a leaf: pure data and disk, no process spawning, no routing.
//
// Its consumers all point down at it — capability reads its priors, the
// launcher reads its launch templates, principal reads its entries. If fleet
// ever imported one of them the graph would cycle, and the registry would
// stop being the thing everything else agrees on.
func TestFleetIsALeaf(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	forbidden := []string{
		"coreutils/pkg/capability",
		"coreutils/pkg/chat",
		"coreutils/pkg/weave",
		"coreutils/pkg/skills",
		"coreutils/pkg/principal",
		"coreutils/pkg/kb",
		"coreutils/pkg/meet",
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, bad := range forbidden {
			if strings.HasSuffix(dep, bad) {
				t.Errorf("pkg/fleet imports %s — the registry must not depend on its consumers", dep)
			}
		}
	}
}

// There is deliberately no "fleet must not import os/exec" guard. It would be
// unsound: `verify` asks a tool its version through the spacetime probes, and
// `edit` opens $EDITOR. What matters is not whether fleet can start a process
// but that it never starts an AGENT — and that is what TestFleetIsALeaf pins,
// by keeping the launcher out of its import graph.
