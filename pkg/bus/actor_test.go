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
