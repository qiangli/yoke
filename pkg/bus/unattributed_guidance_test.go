package bus

// THE FOUR-TRIES REFUSAL (story cb890394). From an agent session with no
// claimed identity, `bashy mb send` fails with `unattributed agent session`,
// and getting to a working send took four attempts: --as needs a REGISTERED
// agent or person; `agent add` alone still needs a LIVE claim; the claim is
// taken by `bashy inbox --peek --as NAME` or by exporting BASHY_AGENT — and
// after all that, a body over the board's cap is rejected for size.
//
// The refusal must state the whole working sequence in one place, so the
// next attempt after reading it is the one that works.

import (
	"errors"
	"strings"
	"testing"
)

func TestUnattributedRefusalStatesTheCompleteWorkingSequence(t *testing.T) {
	boardInTempHome(t)
	DetectHarness = func() (string, bool) { return "codex", true }
	t.Cleanup(func() { DetectHarness = nil })

	refusals := map[string]error{}
	_, refusals["BoardIdentity (mb read side)"] = BoardIdentity("")
	_, refusals["ResolveAuthoredActor (mb send side)"] = ResolveAuthoredActor("")

	// register, claim, send — one sequence, both halves of the board.
	for where, err := range refusals {
		if !errors.Is(err, ErrUnattributed) {
			t.Fatalf("%s: want ErrUnattributed, got %v", where, err)
		}
		for _, want := range []string{
			"bashy agent add NAME --tool T --model M", // 1. register
			"export BASHY_AGENT=NAME",                 // 2a. claim via environment
			"bashy inbox --peek --as NAME",            // 2b. claim via a live peek
			"bashy mb send --as NAME",                 // 3. then send
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal does not state %q:\n%v", where, want, err)
			}
		}
		// The body cap travels with the sequence: the send that finally
		// works must not be rejected for size one attempt later.
		if !strings.Contains(err.Error(), "1024") {
			t.Errorf("%s: refusal does not mention the 1024-byte body limit:\n%v", where, err)
		}
		// The human escape hatch stays: a person in an agent session
		// speaks as themselves, and the login name is who that is here.
		for _, want := range []string{"--as", "codex", "tester"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal no longer mentions %q:\n%v", where, want, err)
			}
		}
	}
}
