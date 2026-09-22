package bus

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
	"github.com/spf13/cobra"
)

func isolateAuthoredActor(t *testing.T) string {
	t.Helper()
	mb := t.TempDir()
	t.Setenv("BASHY_MB_DIR", mb)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("USER", "tester")
	priorNames, priorResolve, priorDetect, priorSession := FleetNames, FleetResolveName, DetectHarness, CurrentSessionClaim
	FleetNames = func() []string { return []string{"agent-x", "agent-y", "agent.x", "target"} }
	FleetResolveName = func(name string) string {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "agent-x", "x-alias":
			return "agent-x"
		case "agent-y", "y-alias":
			return "agent-y"
		case "agent.x", "dot-alias":
			return "agent.x"
		case "target":
			return "target"
		default:
			return ""
		}
	}
	DetectHarness = nil
	t.Cleanup(func() {
		FleetNames, FleetResolveName, DetectHarness = priorNames, priorResolve, priorDetect
		CurrentSessionClaim = priorSession
	})
	return mb
}

func TestResolveAuthoredActorUsesCanonicalDottedAgentClaim(t *testing.T) {
	isolateAuthoredActor(t)
	const raw = "dotted-session"
	if err := room.Join(room.Card{
		ID: room.AgentClaimID("agent.x"), Nick: "agent.x", Tool: "codex", Binding: "codex:test",
		Mode: "interactive", PID: os.Getpid(), OwnerPID: 2147483000,
		SessionClaim: HashSessionClaim(raw), Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	CurrentSessionClaim = func(string) string { return raw }
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/agent.x")
	if got, err := ResolveAuthoredActor(""); err != nil || got != "agent.x" {
		t.Fatalf("dotted agent claim = %q, %v", got, err)
	}
}

func TestResolveAuthoredActorAcceptsLiveLegacyRawDottedClaim(t *testing.T) {
	isolateAuthoredActor(t)
	if err := room.Join(room.Card{
		ID: "agent.x", Nick: "agent.x", Tool: "codex", Binding: "codex:test",
		Mode: "inbox", PID: os.Getpid(), OwnerPID: os.Getpid(), Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/agent.x")
	if got, err := ResolveAuthoredActor(""); err != nil || got != "agent.x" {
		t.Fatalf("legacy dotted claim = %q, %v", got, err)
	}
}

func TestResolveAuthoredActorAcceptsMatchingHashedSessionClaim(t *testing.T) {
	isolateAuthoredActor(t)
	const raw = "vendor-session-secret"
	if err := room.Join(room.Card{
		ID: "agent-x", Nick: "agent-x", Tool: "claude", Binding: "claude:test",
		Mode: "inbox", PID: os.Getpid(), OwnerPID: 2147483000,
		SessionClaim: HashSessionClaim(raw), Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	CurrentSessionClaim = func(string) string { return raw }
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/agent-x")
	if got, err := ResolveAuthoredActor(""); err != nil || got != "agent-x" {
		t.Fatalf("matching session claim = %q, %v", got, err)
	}
	CurrentSessionClaim = func(string) string { return "foreign-session" }
	if _, err := ResolveAuthoredActor(""); err == nil {
		t.Fatal("mismatched session claim was accepted despite foreign ancestry")
	}
}

func TestResolveAuthoredActorPrincipalAndExternalClaim(t *testing.T) {
	isolateAuthoredActor(t)
	if err := room.Join(room.Card{
		ID: "agent-x", Nick: "agent-x", Tool: "claude", Binding: "claude:test",
		Mode: "interactive", PID: os.Getpid(), OwnerPID: os.Getpid(), Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/agent-x")
	for _, tc := range []struct {
		name, explicit, want string
		wantErr              bool
	}{
		{name: "principal default", want: "agent-x"},
		{name: "canonical self", explicit: "agent-x", want: "agent-x"},
		{name: "self alias", explicit: "x-alias", want: "agent-x"},
		{name: "different agent", explicit: "agent-y", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveAuthoredActor(tc.explicit)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("ResolveAuthoredActor(%q) = %q, %v; want %q err=%v", tc.explicit, got, err, tc.want, tc.wantErr)
			}
		})
	}

	t.Setenv("BASHY_PRINCIPAL", "")
	DetectHarness = func() (string, bool) { return "claude", true }
	if _, err := ResolveAuthoredActor("agent-y"); err == nil || !strings.Contains(err.Error(), "no matching live session claim") {
		t.Fatalf("unclaimed external actor error = %v", err)
	}
	if got, err := ResolveAuthoredActor("x-alias"); err != nil || got != "agent-x" {
		t.Fatalf("claimed external actor = %q, %v", got, err)
	}
	if err := room.Join(room.Card{
		ID: "agent-y", Nick: "agent-y", Tool: "claude", Binding: "claude:test",
		Mode: "inbox", PID: os.Getpid(), OwnerPID: 2147483000, Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveAuthoredActor("agent-y"); err == nil {
		t.Fatal("stale/dead-anchor external identity was accepted")
	}
}

func TestForgedPrincipalCannotBypassForeignLiveClaim(t *testing.T) {
	mb := isolateAuthoredActor(t)
	if err := room.Join(room.Card{
		ID: "agent-y", Nick: "agent-y", Tool: "claude", Binding: "claude:test",
		Mode: "inbox", PID: os.Getpid(), OwnerPID: 2147483000, Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/agent-y")
	cmd := NewMessageBoardCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"post", "FORGED-PRINCIPAL-BODY"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("forged BASHY_PRINCIPAL bypassed the foreign session claim")
	}
	if got := authoredPostCount(t, mb); got != 0 {
		t.Fatalf("forged principal appended %d authored posts", got)
	}
}

func TestAuthoredCommandsRejectCrossIdentityWithoutAuthoredAppend(t *testing.T) {
	mb := isolateAuthoredActor(t)
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/agent-x")

	tests := []struct {
		name string
		cmd  func() *cobra.Command
		args []string
	}{
		{name: "mb post", cmd: NewMessageBoardCmd, args: []string{"post", "--as", "agent-y", "REJECTED-BODY"}},
		{name: "mb send", cmd: NewMessageBoardCmd, args: []string{"send", "--as", "agent-y", "target", "REJECTED-BODY"}},
		{name: "ping", cmd: NewPingCmd, args: []string{"--as", "agent-y", "--to", "target", "REJECTED-BODY"}},
		{name: "notify", cmd: NewNotifyCmd, args: []string{"--as", "agent-y", "target", "REJECTED-BODY"}},
		{name: "bus publish", cmd: NewBusCmd, args: []string{"publish", "--as", "agent-y", "--topic", "test", "REJECTED-BODY"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			beforePosts := authoredPostCount(t, mb)
			beforeEvents, err := room.Timeline(0)
			if err != nil {
				t.Fatal(err)
			}
			cmd := tc.cmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetContext(context.Background())
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("cross-identity command succeeded")
			}
			if got := authoredPostCount(t, mb); got != beforePosts {
				t.Fatalf("rejected command appended MB post: before=%d after=%d", beforePosts, got)
			}
			afterEvents, err := room.Timeline(0)
			if err != nil {
				t.Fatal(err)
			}
			if len(afterEvents) != len(beforeEvents)+1 {
				t.Fatalf("refusal events = %d -> %d, want exactly one warning", len(beforeEvents), len(afterEvents))
			}
			warning := afterEvents[len(afterEvents)-1]
			if warning.To != "agent-y" || warning.Topic != identityRefusalTopic ||
				warning.Principal != "bashy-identity-guard" || strings.Contains(warning.Body, "REJECTED-BODY") {
				t.Fatalf("refusal warning = %+v", warning)
			}
		})
	}
}

func authoredPostCount(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "posts.jsonl"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
}

// A bare tool that declares its identity the way the session roster trusts
// it takes an UNCLAIMED seat on its first authored message; an undeclared
// caller is refused with the remedy spelled out; a declaration never
// overrides a seat somebody else holds.
func TestResolveAuthoredActorDeclaredIdentityTakesUnclaimedSeat(t *testing.T) {
	isolateAuthoredActor(t)
	DetectHarness = func() (string, bool) { return "codex", true }
	CurrentSessionClaim = func(string) string { return "thread-123" }
	for _, env := range []string{"BASHY_AGENT", "WEAVE_AGENT", "BASHY_AGENT_ID", "WEAVE_CONDUCTOR"} {
		t.Setenv(env, "")
	}

	// Undeclared: refused, and the refusal names the fix.
	_, err := ResolveAuthoredActor("agent-y")
	if err == nil || !strings.Contains(err.Error(), "no matching live session claim") || !strings.Contains(err.Error(), "BASHY_AGENT=agent-y") {
		t.Fatalf("undeclared unclaimed actor error = %v", err)
	}
	if _, live, _ := room.Find(room.AgentClaimID("agent-y")); live {
		t.Fatal("a refused caller must not take the seat")
	}

	// Declared (alias spelling): accepted, and the seat is now held with the
	// session claim on the card, anchored to this command's parent.
	t.Setenv("BASHY_AGENT", "y-alias")
	got, err := ResolveAuthoredActor("agent-y")
	if err != nil || got != "agent-y" {
		t.Fatalf("declared unclaimed actor = %q, %v", got, err)
	}
	card, live, err := room.Find(room.AgentClaimID("agent-y"))
	if err != nil || !live {
		t.Fatalf("seat not taken: live=%v err=%v", live, err)
	}
	if card.SessionClaim != HashSessionClaim("thread-123") || card.Mode != "authored" || card.PID != os.Getppid() {
		t.Fatalf("seat card = %+v", card)
	}
	// Second message from the same session: matched by claim, no new card.
	if got, err := ResolveAuthoredActor("agent-y"); err != nil || got != "agent-y" {
		t.Fatalf("repeat authored actor = %q, %v", got, err)
	}

	// Declaring a name someone ELSE holds live (foreign anchor) is still refused.
	if err := room.Join(room.Card{
		ID: room.AgentClaimID("agent-x"), Nick: "agent-x", Tool: "claude", Binding: "claude:test",
		Mode: "inbox", SessionClaim: HashSessionClaim("other-session"),
		PID: os.Getpid(), OwnerPID: 2147483000, Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_AGENT", "agent-x")
	if _, err := ResolveAuthoredActor("agent-x"); err == nil || !strings.Contains(err.Error(), "no matching live session claim") {
		t.Fatalf("declared identity overrode a foreign live claim: %v", err)
	}
}
