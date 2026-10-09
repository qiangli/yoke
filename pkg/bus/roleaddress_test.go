package bus

import "testing"

func withHostRoles(t *testing.T, roles ...HostRole) {
	t.Helper()
	HostRoles = func() []HostRole { return roles }
	t.Cleanup(func() { HostRoles = nil })
}

func TestDirected_RoleMailRequiresHolderAuthorization(t *testing.T) {
	withHostRoles(t, HostRole{Label: "steward", Topic: "steward.test", Holder: "holder"})
	prev := RoleReaderAuthorizer
	t.Cleanup(func() { RoleReaderAuthorizer = prev })
	p := Post{To: "steward.test"}
	RoleReaderAuthorizer = nil
	if p.Directed("holder") || p.Directed(p.To) {
		t.Fatal("nil authorizer admitted role mail")
	}
	RoleReaderAuthorizer = AuthorizeByHolder
	if !p.Directed("holder") || p.Directed("other") || p.Directed(p.To) {
		t.Fatal("role holder authorization bypassed")
	}
	if !(Post{To: "holder"}).Directed("holder") {
		t.Fatal("personal mail regressed")
	}
}

func TestAuthorizeByHolder_InstancePrincipal(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	withHostRoles(t, HostRole{Label: "conductor:22", Topic: "conductor.22", Holder: id})
	for _, reader := range []string{id, "instance/" + id, "dhnt:agent/" + id} {
		if !AuthorizeByHolder("conductor.22", reader) {
			t.Errorf("holder %q refused", reader)
		}
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

func TestAuthorizeByHolder_DeputyUUIDAndVacancy(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	withHostRoles(t, HostRole{Label: "deputy:321", Topic: "deputy.321", Holder: id})
	prev := RoleReaderAuthorizer
	t.Cleanup(func() { RoleReaderAuthorizer = prev })
	p := Post{To: "deputy.321"}
	RoleReaderAuthorizer = nil
	if p.Directed(id) {
		t.Fatal("unwired deputy mail admitted")
	}
	RoleReaderAuthorizer = AuthorizeByHolder
	if !p.Directed(id) || p.Directed("other") {
		t.Fatal("deputy holder authorization failed")
	}
	withHostRoles(t, HostRole{Label: "deputy:321", Topic: "deputy.321"})
	if p.Directed(id) {
		t.Fatal("vacant deputy mail admitted")
	}
}
