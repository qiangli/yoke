package bus

import "testing"

func withHostRoles(t *testing.T, roles ...HostRole) {
	t.Helper()
	HostRoles = func() []HostRole { return roles }
	t.Cleanup(func() { HostRoles = nil })
}

// THE UNIFICATION, stated as a test. A post addressed to a role on this host is
// DIRECTED at whoever reads the board — no --as, no principal, no setup.
//
// That is what lets a third-party TUI holding the steward seat run `bashy mb`
// and see the seat's mail. A seat is host-and-login scoped rather than tied to
// an identity, and the board's rule has always been that addressing says who
// should ACT, never who may read.
func TestDirected_RoleMailIsDirectedAtAnyReaderOnThisHost(t *testing.T) {
	withHostRoles(t, HostRole{Label: "steward", Topic: "steward.dragon-u501"})

	p := Post{To: "steward.dragon-u501", Body: "volunteering as conductor"}
	for _, reader := range []string{"codex-gpt5.6-sol", "claude-opus5", "qiangli"} {
		if !p.Directed(reader) {
			t.Errorf("role mail is not directed at %q — a seat's mail must reach whoever holds it, whatever they are called", reader)
		}
	}

	// An agent's own mail is unaffected: still directed only at that agent.
	q := Post{To: "codex-gpt5.6-sol", Body: "yours alone"}
	if q.Directed("claude-opus5") {
		t.Error("an agent-addressed post leaked to another agent")
	}
	if !q.Directed("codex-gpt5.6-sol") {
		t.Error("an agent-addressed post stopped reaching its own agent")
	}
}

// A role that is not this host's must not capture mail. Otherwise a name that
// merely looks like a role would swallow posts meant for an agent.
func TestDirected_UnknownRoleDoesNotCapture(t *testing.T) {
	withHostRoles(t, HostRole{Label: "steward", Topic: "steward.dragon-u501"})

	p := Post{To: "steward.some-other-host", Body: "not ours"}
	if p.Directed("codex-gpt5.6-sol") {
		t.Error("mail for another host's seat was treated as directed here")
	}
}

// The label is what a person types; the topic is what the machine routes on.
// Both resolve, and a name this host has no role for is left ALONE — so an
// agent called something role-ish is never captured.
func TestResolveRole(t *testing.T) {
	withHostRoles(t, HostRole{Label: "steward", Topic: "steward.dragon-u501"})

	for _, in := range []string{"steward", "Steward", "steward.dragon-u501"} {
		got, ok := ResolveRole(in)
		if !ok || got != "steward.dragon-u501" {
			t.Errorf("ResolveRole(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := ResolveRole("codex-gpt5.6-sol"); ok {
		t.Error("an agent name resolved as a role — mb send would misroute it")
	}
	if _, ok := ResolveRole("stewardship"); ok {
		t.Error("a name that merely starts with a role was captured")
	}
}

// Rendering shows the name people act on, not the routing address. A post whose
// recipient reads as machine noise is one a reader skips.
func TestRoleLabelFor(t *testing.T) {
	withHostRoles(t, HostRole{Label: "steward", Topic: "steward.dragon-u501"})

	if got := (Post{To: "steward.dragon-u501"}).Audiences(); got != "steward" {
		t.Errorf("Audiences() = %q, want the human label", got)
	}
	// A role this host does not know still renders its address rather than
	// nothing: mail must never display without a recipient.
	if got := RoleLabelFor("conductor.99"); got != "conductor.99" {
		t.Errorf("unknown role rendered as %q, want its address", got)
	}
}

// With no host wired, nothing is a role — the board behaves exactly as it did
// before roles existed. pkg/bus is importable by hosts that own no seats.
func TestRoleAddressing_UnwiredHostIsUnchanged(t *testing.T) {
	HostRoles = nil
	if _, ok := ResolveRole("steward"); ok {
		t.Error("resolved a role with no host wired")
	}
	if (Post{To: "steward.dragon-u501"}).Directed("anyone") {
		t.Error("role mail was directed with no host wired")
	}
}
