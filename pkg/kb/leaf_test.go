package kb_test

import (
	"os/exec"
	"strings"
	"testing"
)

// pkg/kb is a leaf WITH RESPECT TO ITS CONSUMERS. It parses every kind in the
// ref vocabulary but resolves only kb + todo nodes; every other store resolves
// its own kind and imports kb to do so (todo, weave, chat, recall, search,
// sota, foreman, execlog, and lexicon once define resolves refs). If kb ever
// imported one of them the graph would cycle — which is why the shared node
// type lives in pkg/ref, below all of them, and why sprint links stay
// `external` at this layer instead of being "fixed" by importing weave.
//
// This is deliberately NOT "kb imports the standard library only": kb already
// reaches fleet, principal, bus and room for provenance and events. Those point
// DOWN; the forbidden list is what points UP.
func TestKBIsALeaf(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	forbidden := []string{
		"coreutils/pkg/issue",
		"coreutils/pkg/todo",
		"coreutils/pkg/weave",
		"coreutils/pkg/chat",
		"coreutils/pkg/recall",
		"coreutils/pkg/search",
		"coreutils/pkg/sota",
		"coreutils/pkg/foreman",
		"coreutils/pkg/execlog",
		"coreutils/pkg/lexicon",
		"coreutils/pkg/meet",
		"coreutils/pkg/steward",
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, bad := range forbidden {
			if strings.HasSuffix(dep, bad) {
				t.Errorf("pkg/kb imports %s — kb must not depend on a store that resolves through it", dep)
			}
		}
	}
}
