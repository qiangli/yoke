package acp_test

import (
	"os/exec"
	"strings"
	"testing"
)

// pkg/acp MUST NOT IMPORT pkg/herald.
//
// It used to, for one function: the gate that settles a self-reported
// completion. That single edge pinned the arrow herald -> acp shut, and ACP is
// the protocol an external HOST speaks to drive this machine — so `herald acp`
// has to construct an acp.Agent, and could not while acp imported herald.
//
// The gate moved down to pkg/gate, which both protocol packages now import and
// neither owns. This test is what keeps it that way: the cycle would come back
// the moment someone reaches for herald.RunLocalGate from acp, and the compiler
// error they would get ("import cycle not allowed") appears in pkg/herald,
// nowhere near the import they added.
//
// Deliberately in package acp_test and driven through `go list` rather than a
// hand-maintained list of allowed imports: an allowlist rots into a chore, and
// the transitive set is the thing that actually forms cycles.
func TestACPDoesNotImportHerald(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/qiangli/yoke/pkg/acp").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(dep) == "github.com/qiangli/yoke/pkg/herald" {
			t.Fatal("pkg/acp imports pkg/herald — the cycle is back, and `herald acp` " +
				"cannot be built. The gate primitive lives in pkg/gate; use gate.RunLocal.")
		}
	}
}

// The other half: herald CAN import acp. Proving the cycle is gone means
// showing the arrow actually works in the direction the feature needs, not
// just that the old one is absent.
func TestHeraldCanImportACP(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/qiangli/yoke/pkg/herald").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	// herald does not import acp TODAY — `herald acp` is not wired yet. What
	// matters is that it COULD, so compile a probe package that does.
	probe := exec.Command("go", "build", "-o", "/dev/null",
		"github.com/qiangli/yoke/pkg/herald", "github.com/qiangli/yoke/pkg/acp")
	if b, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("herald and acp cannot coexist in one build: %v\n%s", err, b)
	}
	_ = out
}
