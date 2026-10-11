package meet

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/room"
)

func setupDMFrontDoorEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("USER", "tester")
	pinFleet(t)
}

func runDMFrontDoorCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newDMCmd()
	cmd.SetArgs(args)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dm %v: %v\noutput: %s", args, err, out.String())
	}
	return out.String()
}

// An unrecognised sender must get a pair code and the agent must never be
// invoked — a message from a sender nobody admitted cannot reach, let alone
// launch, the agent.
func TestDMFrontDoorUnpairedSenderGetsPairCodeWithoutInvokingAgent(t *testing.T) {
	setupDMFrontDoorEnv(t)
	invoked := false
	old := invokeRelayDM
	invokeRelayDM = func(context.Context, chat.Options, chat.Runner) (chat.Result, error) {
		invoked = true
		return chat.Result{Output: "should never run"}, nil
	}
	t.Cleanup(func() { invokeRelayDM = old })

	out := runDMFrontDoorCmd(t, "codex", "--channel", "slack", "--sender", "alice", "hello", "there")
	if invoked {
		t.Fatal("an unpaired sender's message invoked the agent")
	}
	if !strings.Contains(out, "pair code") {
		t.Fatalf("output has no pair code: %q", out)
	}
	allowed, err := dmSenderAllowed("codex", "slack", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("an unpaired sender must not be admitted before a decision")
	}

	pending, err := ask.PendingApprovals()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Approval.Source != ask.SourceMeetDM {
		t.Fatalf("pending approvals = %+v", pending)
	}

	events, err := room.Timeline(10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == room.EventInbound && e.Actor == "slack:alice" && e.Target == "codex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no inbound room event recorded for the message: %+v", events)
	}
}

// process-level: unpaired -> an operator's approval decision -> the SAME
// message resent now reaches the agent and its reply comes back on stdout.
func TestDMFrontDoorApprovalThenSameMessageGetsReply(t *testing.T) {
	setupDMFrontDoorEnv(t)
	calls := 0
	old := invokeRelayDM
	invokeRelayDM = func(_ context.Context, opt chat.Options, _ chat.Runner) (chat.Result, error) {
		calls++
		if opt.Instruction != "hello there" {
			t.Fatalf("instruction = %q", opt.Instruction)
		}
		return chat.Result{Output: "hi alice"}, nil
	}
	t.Cleanup(func() { invokeRelayDM = old })

	runDMFrontDoorCmd(t, "codex", "--channel", "slack", "--sender", "alice", "hello", "there")
	if calls != 0 {
		t.Fatalf("calls = %d before any approval", calls)
	}

	pending, err := ask.PendingApprovals()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
	if _, err := ask.Decide(pending[0].ID, ask.Decision{Verdict: ask.VerdictAllow, By: "operator"}, ask.DecideOpts{}); err != nil {
		t.Fatal(err)
	}

	out := runDMFrontDoorCmd(t, "codex", "--channel", "slack", "--sender", "alice", "hello", "there")
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly one agent invocation", calls)
	}
	if strings.TrimSpace(out) != "hi alice" {
		t.Fatalf("stdout = %q, want the agent's reply", out)
	}
	allowed, err := dmSenderAllowed("codex", "slack", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("the sender should now be admitted")
	}

	// A third send uses the standing admission directly: no new approval.
	runDMFrontDoorCmd(t, "codex", "--channel", "slack", "--sender", "alice", "hello", "there")
	if calls != 2 {
		t.Fatalf("calls = %d, want two total invocations", calls)
	}
	pending, err = ask.PendingApprovals()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none once admitted", pending)
	}
}

// A denied pairing must never admit the sender, and a revoke on an admitted
// sender must put them back behind pairing.
func TestDMFrontDoorDenyAndRevoke(t *testing.T) {
	setupDMFrontDoorEnv(t)
	invoked := false
	old := invokeRelayDM
	invokeRelayDM = func(context.Context, chat.Options, chat.Runner) (chat.Result, error) {
		invoked = true
		return chat.Result{Output: "no"}, nil
	}
	t.Cleanup(func() { invokeRelayDM = old })

	runDMFrontDoorCmd(t, "codex", "--channel", "slack", "--sender", "bob", "hi")
	pending, err := ask.PendingApprovals()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
	if _, err := ask.Decide(pending[0].ID, ask.Decision{Verdict: ask.VerdictDeny, By: "operator"}, ask.DecideOpts{}); err != nil {
		t.Fatal(err)
	}

	out := runDMFrontDoorCmd(t, "codex", "--channel", "slack", "--sender", "bob", "hi")
	if invoked {
		t.Fatal("a denied sender's message invoked the agent")
	}
	if !strings.Contains(out, "denied") {
		t.Fatalf("output = %q, want a denial notice", out)
	}
	if allowed, _ := dmSenderAllowed("codex", "slack", "bob"); allowed {
		t.Fatal("a denied sender must not be admitted")
	}

	if err := dmSenderGrantAccess("codex", "slack", "carol", "operator"); err != nil {
		t.Fatal(err)
	}
	out = runDMFrontDoorCmd(t, "codex", "--channel", "slack", "--sender", "carol", "--revoke")
	if !strings.Contains(out, "revoked") {
		t.Fatalf("revoke output = %q", out)
	}
	if allowed, _ := dmSenderAllowed("codex", "slack", "carol"); allowed {
		t.Fatal("carol should be revoked")
	}
}
