package chat

import (
	"strings"
	"testing"
)

// TestPickAgentConflict — --agent with --band/--tool is a contradiction, not a
// silent precedence.
func TestPickAgentConflict(t *testing.T) {
	if _, err := PickAgent(Selector{Agent: "claude", Band: 3}); err == nil {
		t.Fatal("expected an error for --agent + --band, got nil")
	}
	if _, err := PickAgent(Selector{Agent: "claude", Tool: "codex"}); err == nil {
		t.Fatal("expected an error for --agent + --tool, got nil")
	}
}

// TestPickAgentSpecific — a specific name passes through (canonicalized when the
// catalog knows it, verbatim when it is a bare tool).
// A BARE TOOL IS NOT AN AGENT. This asserted the opposite until sprint #111:
// `--agent claude` passed through and minted a launch identity no `bashy agent`
// record owned — an address bus and inbox could never route to.
//
// The contract change is deliberate and scoped to THIS call site. `chat --agent`
// claims an identity: the session is addressable, keeps a conversation store and
// answers mail under that name. weave's `-- claude` is a raw command that claims
// no identity and is untouched — its own tests say rewriting it "would silently
// change every conductor script".
func TestPickAgentRefusesABareToolAsAnIdentity(t *testing.T) {
	_, err := PickAgent(Selector{Agent: "claude"})
	if err == nil {
		t.Fatal("a bare tool was accepted as an agent identity")
	}
	// The refusal has to be actionable, not merely correct.
	for _, want := range []string{"not a registered Bashy agent", "bashy agent add claude"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%v", want, err)
		}
	}
}

// --tool SELECTS among registered agents rather than naming one, so it is
// unaffected by the rule above. Pinning it here because the two read alike and
// a later reader could easily "tidy" one into the other.
func TestPickAgentStillSelectsByTool(t *testing.T) {
	if _, err := PickAgent(Selector{Tool: "claude"}); err != nil {
		if strings.Contains(err.Error(), "not a registered Bashy agent") {
			t.Fatalf("--tool was refused as if it named an identity: %v", err)
		}
	}
}

// TestPickAgentEmpty — no selector means "use the default", signalled by "".
func TestPickAgentEmpty(t *testing.T) {
	got, err := PickAgent(Selector{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("empty selector should return \"\", got %q", got)
	}
}

// TestPickAgentBandRange — an out-of-range band is rejected loudly.
func TestPickAgentBandRange(t *testing.T) {
	if _, err := PickAgent(Selector{Band: 99}); err == nil {
		t.Fatal("expected out-of-range band error, got nil")
	}
}
