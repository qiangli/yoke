package supervise

import (
	"context"
	"testing"
)

// runGate's exit code is a VERDICT — the orchestrator's own check that a
// worker's "done" is true. Before this was fixed it set no cmd.Env at all and
// inherited the whole environment, so a control set in the launching shell
// reached the gate and the verdict could differ by where it ran.
//
// This runs a real gate and asks it what it can see, so it fails if the
// environment is ever inherited again.
func TestRunGateDoesNotInheritAmbientControls(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "1")

	pass, exit, tail := runGate(context.Background(), t.TempDir(), `printf '%s' "${BASHY_AGENTIC-unset}"`)

	if !pass || exit != 0 {
		t.Fatalf("gate did not run: pass=%v exit=%d tail=%q", pass, exit, tail)
	}
	if tail != "unset" {
		t.Fatalf("gate saw BASHY_AGENTIC=%q; a gate must not inherit ambient controls", tail)
	}
}

// The same gate must still see what a build or test command needs, or the fix
// would trade a correctness bug for a broken gate.
func TestRunGateKeepsBuildEnvironment(t *testing.T) {
	pass, exit, tail := runGate(context.Background(), t.TempDir(), `test -n "$PATH" && printf ok`)
	if !pass || exit != 0 || tail != "ok" {
		t.Fatalf("gate lost PATH: pass=%v exit=%d tail=%q", pass, exit, tail)
	}
}
