package bus

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
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

// foreignLivePID is a pid that is LIVE and is not an ancestor of this process —
// what a card held by somebody else's harness looks like.
//
// These fixtures used a high unused pid number for "foreign", which also made
// the card DEAD once room judged a card by its recorded owner rather than by
// whichever process wrote it last. A refusal would then have been proving only
// that nobody held the seat, which is not what these tests are about. A child
// of the test process is live and, being a descendant, is foreign to the
// ancestry check.
func foreignLivePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a live foreign process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

func TestResolveAuthoredActorUsesCanonicalDottedAgentClaim(t *testing.T) {
	isolateAuthoredActor(t)
	const raw = "dotted-session"
	if err := room.Join(room.Card{
		ID: room.AgentClaimID("agent.x"), Nick: "agent.x", Tool: "codex", Binding: "codex:test",
		Mode: "interactive", PID: os.Getpid(), OwnerPID: foreignLivePID(t),
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
		Mode: "inbox", PID: os.Getpid(), OwnerPID: foreignLivePID(t),
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
		Mode: "inbox", PID: os.Getpid(), OwnerPID: foreignLivePID(t),
		SessionClaim: HashSessionClaim("other-session"), Principal: "operator",
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
		PID: os.Getpid(), OwnerPID: foreignLivePID(t), Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_AGENT", "agent-x")
	if _, err := ResolveAuthoredActor("agent-x"); err == nil || !strings.Contains(err.Error(), "no matching live session claim") {
		t.Fatalf("declared identity overrode a foreign live claim: %v", err)
	}
}

const instanceTestUUID = "aaaaaaaa-1111-2222-3333-444444444444"

func openInstanceForTest(t *testing.T) fleet.Instance {
	t.Helper()
	store := fleet.NewInstanceStore(t.TempDir())
	prior := InstanceStoreFn
	InstanceStoreFn = func() *fleet.InstanceStore { return store }
	t.Cleanup(func() { InstanceStoreFn = prior })
	inst, err := store.Open(fleet.Family{Name: "esme-cfg", Display: "Esme", Policy: fleet.PolicySingle,
		Bindings: []string{"claude:opus5.5"}}, fleet.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// GAP 2. A session stamped with dhnt:agent/<uuid> whose instance:<uuid> card
// carries the current session digest authors as the instance, with no catalog
// agent name anywhere.
func TestResolveAuthoredActorAcceptsAHeldInstanceClaim(t *testing.T) {
	isolateAuthoredActor(t)
	inst := openInstanceForTest(t)
	const raw = "instance-session"
	if err := principal.ClaimInstance(inst, principal.InstanceClaim{
		Session: HashSessionClaim(raw), OwnerPID: foreignLivePID(t), Cwd: "/tmp/x",
	}); err != nil {
		t.Fatal(err)
	}
	CurrentSessionClaim = func(string) string { return raw }
	t.Setenv("BASHY_PRINCIPAL", inst.URN())

	want := inst.MailAddress()
	if got, err := ResolveAuthoredActor(""); err != nil || got != want {
		t.Fatalf("actor = %q, %v; want %q", got, err, want)
	}
	if got, err := ResolveAuthoredActor(inst.UUID); err != nil || got != want {
		t.Fatalf("--as <uuid> actor = %q, %v; want %q", got, err, want)
	}
	// An instance cannot author as somebody else.
	if _, err := ResolveAuthoredActor("agent-x"); err == nil {
		t.Fatal("an instance authored as a different agent")
	}
}

// GAP 2, the refusal half: a card held by a DIFFERENT session digest, owned by
// a process that is not our ancestor, is a competing session.
func TestResolveAuthoredActorRefusesACompetingInstanceSession(t *testing.T) {
	isolateAuthoredActor(t)
	inst := openInstanceForTest(t)
	if err := principal.ClaimInstance(inst, principal.InstanceClaim{
		Session: HashSessionClaim("the-owner"), OwnerPID: foreignLivePID(t), Cwd: "/tmp/x",
	}); err != nil {
		t.Fatal(err)
	}
	CurrentSessionClaim = func(string) string { return "an-impostor" }
	t.Setenv("BASHY_PRINCIPAL", inst.URN())
	if got, err := ResolveAuthoredActor(""); err == nil {
		t.Fatalf("a competing session authored as %q", got)
	}
}

// GAP 2: an unknown or retired instance is not an identity.
func TestResolveAuthoredActorRefusesAnUnknownInstance(t *testing.T) {
	isolateAuthoredActor(t)
	openInstanceForTest(t)
	CurrentSessionClaim = func(string) string { return "s" }
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/"+instanceTestUUID)
	if got, err := ResolveAuthoredActor(""); err == nil {
		t.Fatalf("an instance with no record authored as %q", got)
	}
}

// GAP 2: an unowned instance the caller names takes its seat on first use,
// and later commands from the same session are recognised by digest.
func TestResolveAuthoredActorTakesAnUnownedInstanceOnFirstUse(t *testing.T) {
	isolateAuthoredActor(t)
	inst := openInstanceForTest(t)
	CurrentSessionClaim = func(string) string { return "first-use-session" }
	if got, err := ResolveAuthoredActor(inst.UUID); err != nil || got != inst.MailAddress() {
		t.Fatalf("first use = %q, %v", got, err)
	}
	card, live, err := room.Find(inst.ClaimID())
	if err != nil || !live || card.SessionClaim != HashSessionClaim("first-use-session") {
		t.Fatalf("seat not taken by the session: %+v live=%v err=%v", card, live, err)
	}
	t.Setenv("BASHY_PRINCIPAL", inst.URN())
	if got, err := ResolveAuthoredActor(""); err != nil || got != inst.MailAddress() {
		t.Fatalf("second command = %q, %v", got, err)
	}
}
