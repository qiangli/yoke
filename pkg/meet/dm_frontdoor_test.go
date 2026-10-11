package meet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/room"
)

// fakePeerPairing stands in for pkg/webconsole's app-pair store, which
// imports this package and so cannot be linked into its internal tests. The
// real store is exercised end to end by pkg/webconsole's pair_peer tests.
type fakePeerPairing struct {
	devices  map[string][]string // channel:sender -> scope
	requests int
	revoked  []string
}

func (f *fakePeerPairing) Paired(channel, sender string) (PairedPeer, bool, error) {
	scope, ok := f.devices[channel+":"+sender]
	if !ok {
		return PairedPeer{}, false, nil
	}
	return PairedPeer{Device: "dev-" + sender, Scope: scope, Expires: time.Now().Add(time.Hour)}, true, nil
}

func (f *fakePeerPairing) Request(channel, sender string) (PeerRequest, error) {
	f.requests++
	if f.requests > 1 {
		return PeerRequest{Ticket: "t1", Pending: true, Expires: time.Now().Add(time.Hour)}, nil
	}
	return PeerRequest{Code: "ABCD-EFGH", Ticket: "t1", Expires: time.Now().Add(time.Hour)}, nil
}

func (f *fakePeerPairing) Revoke(channel, sender string) (int, error) {
	key := channel + ":" + sender
	if _, ok := f.devices[key]; !ok {
		return 0, nil
	}
	delete(f.devices, key)
	f.revoked = append(f.revoked, key)
	return 1, nil
}

func setupDMFrontDoorEnv(t *testing.T) *fakePeerPairing {
	t.Helper()
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("USER", "tester")
	pinFleet(t)
	f := &fakePeerPairing{devices: map[string][]string{}}
	old := peerPairing
	peerPairing = f
	t.Cleanup(func() { peerPairing = old })
	return f
}

// stubInvoke replaces chat.Invoke and records every call.
func stubInvoke(t *testing.T, reply string) *[]chat.Options {
	t.Helper()
	var calls []chat.Options
	old := invokeRelayDM
	invokeRelayDM = func(_ context.Context, opt chat.Options, _ chat.Runner) (chat.Result, error) {
		calls = append(calls, opt)
		return chat.Result{Output: reply}, nil
	}
	t.Cleanup(func() { invokeRelayDM = old })
	return &calls
}

func runDMFrontDoorCmd(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	cmd := newDMCmd()
	cmd.SetArgs(args)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dm %v: %v\noutput: %s", args, err, out.String())
	}
	return out.String()
}

func inboundEvents(t *testing.T, actor string) []room.Event {
	t.Helper()
	events, err := room.Timeline(100)
	if err != nil {
		t.Fatal(err)
	}
	var out []room.Event
	for _, e := range events {
		if e.Type == room.EventInbound && e.Actor == actor {
			out = append(out, e)
		}
	}
	return out
}

// An unrecognised sender gets a pair code from the store and no agent runs.
func TestDMFrontDoorUnpairedSenderGetsPairCodeWithoutInvokingAgent(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	calls := stubInvoke(t, "should never run")

	out := runDMFrontDoorCmd(t, "", "codex", "--channel", "slack", "--sender", "alice", "hello", "there")
	if len(*calls) != 0 {
		t.Fatal("an unpaired sender's message invoked the agent")
	}
	if !strings.Contains(out, "pair code ABCD-EFGH") || !strings.Contains(out, "bashy app pair approve ABCD-EFGH") {
		t.Fatalf("output has no pair code: %q", out)
	}
	out = runDMFrontDoorCmd(t, "", "codex", "--channel", "slack", "--sender", "alice", "hello", "again")
	if !strings.Contains(out, "pairing pending") || strings.Contains(out, "ABCD-EFGH") {
		t.Fatalf("a resend must report the pending ticket, never re-show a code: %q", out)
	}
	if len(*calls) != 0 {
		t.Fatal("a pending sender's message invoked the agent")
	}
	if ev := inboundEvents(t, "slack:alice"); len(ev) != 2 || ev[0].Target != "codex" {
		t.Fatalf("want one inbound event per message, got %+v", ev)
	}
	if f.requests != 2 {
		t.Fatalf("requests = %d", f.requests)
	}
}

// With no pairing store linked the front door refuses; it never falls open.
func TestDMFrontDoorWithoutPairingStoreRefuses(t *testing.T) {
	setupDMFrontDoorEnv(t)
	peerPairing = nil
	calls := stubInvoke(t, "no")
	cmd := newDMCmd()
	cmd.SetArgs([]string{"codex", "--channel", "slack", "--sender", "alice", "hi"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "pairing store") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if len(*calls) != 0 {
		t.Fatal("agent invoked without a pairing store")
	}
}

// A paired ask-scope sender reaches the agent read-only; the reply is stdout
// and both sides land in the shared DM room transcript.
func TestDMFrontDoorPairedAskSenderGetsReadOnlyReply(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["slack:alice"] = []string{PeerScopeAsk}
	calls := stubInvoke(t, "hi alice")

	out := runDMFrontDoorCmd(t, `{"text":"hello there","thread":"T42"}`,
		"codex", "--channel", "slack", "--sender", "alice")
	if strings.TrimSpace(out) != "hi alice" {
		t.Fatalf("stdout = %q, want the agent's reply", out)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
	if c := (*calls)[0]; c.Instruction != "hello there" || !c.ReadOnly || c.AllowUnsafe {
		t.Fatalf("ask scope must run read-only: %+v", c)
	}
	st, err := dmRoomFor("codex", "slack:alice")
	if err != nil {
		t.Fatal(err)
	}
	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range events {
		texts = append(texts, e.Text)
	}
	joined := strings.Join(texts, "|")
	if !strings.Contains(joined, "[T42] hello there") || !strings.Contains(joined, "hi alice") {
		t.Fatalf("transcript = %q", joined)
	}
	if ev := inboundEvents(t, "slack:alice"); len(ev) != 1 || ev[0].Body != "[T42] hello there" {
		t.Fatalf("inbound events = %+v", ev)
	}
}

// control scope runs with the host's ordinary turn authority.
func TestDMFrontDoorControlScopeRunsWithTurnAuthority(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["slack:alice"] = []string{PeerScopeControl}
	calls := stubInvoke(t, "done")
	runDMFrontDoorCmd(t, "", "codex", "--channel", "slack", "--sender", "alice", "fix", "it")
	readOnly, allowUnsafe := turnAuthority()
	if c := (*calls)[0]; c.ReadOnly != readOnly || c.AllowUnsafe != allowUnsafe {
		t.Fatalf("control scope authority = %+v", c)
	}
}

// An empty agent operand routes to the S2 sender binding.
func TestDMFrontDoorEmptyAgentUsesSenderBinding(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["slack:alice"] = []string{PeerScopeAsk}
	calls := stubInvoke(t, "bound")
	old := boundAgentFor
	boundAgentFor = func(channel, sender string) (string, bool) {
		if channel == "slack" && sender == "alice" {
			return "codex", true
		}
		return "", false
	}
	t.Cleanup(func() { boundAgentFor = old })

	out := runDMFrontDoorCmd(t, "", "", "--channel", "slack", "--sender", "alice", "status?")
	if strings.TrimSpace(out) != "bound" || len(*calls) != 1 || (*calls)[0].Agent != "codex" {
		t.Fatalf("out = %q calls = %+v", out, *calls)
	}
}

// Scope is enforced per request: ask cannot approve, approve cannot message.
func TestDMFrontDoorScopeIsEnforced(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["slack:alice"] = []string{PeerScopeAsk}
	f.devices["slack:ops"] = []string{PeerScopeApprove}
	calls := stubInvoke(t, "no")

	req, err := ask.Raise(ask.ApprovalSpec{Source: ask.SourceConfirm, Argv: []string{"rm", "-rf", "build"}})
	if err != nil {
		t.Fatal(err)
	}
	out := runDMFrontDoorCmd(t, "", "", "--channel", "slack", "--sender", "alice", "/approve", req.ID)
	if !strings.Contains(out, `needs scope "approve"`) {
		t.Fatalf("out = %q", out)
	}
	if _, decided := ask.LoadDecision(req.ID); decided {
		t.Fatal("an ask-scope sender settled an approval")
	}
	out = runDMFrontDoorCmd(t, "", "codex", "--channel", "slack", "--sender", "ops", "hello")
	if !strings.Contains(out, `needs scope "ask"`) || len(*calls) != 0 {
		t.Fatalf("approve-only sender reached the agent: out=%q calls=%d", out, len(*calls))
	}
	out = runDMFrontDoorCmd(t, "", "", "--channel", "slack", "--sender", "alice", "/meet", "standup", "hi")
	if !strings.Contains(out, `needs scope "control"`) {
		t.Fatalf("out = %q", out)
	}
}

// /approve and /deny settle the shared S4 queue, attributed to the sender.
func TestDMFrontDoorApproveAndDenyResolveSharedQueue(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["slack:ops"] = []string{PeerScopeApprove}
	calls := stubInvoke(t, "no")

	a, err := ask.Raise(ask.ApprovalSpec{Source: ask.SourceConfirm, Argv: []string{"git", "push"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ask.Raise(ask.ApprovalSpec{Source: ask.SourceConfirm, Argv: []string{"git", "reset", "--hard"}})
	if err != nil {
		t.Fatal(err)
	}
	out := runDMFrontDoorCmd(t, "", "", "--channel", "slack", "--sender", "ops", "/approve", a.ID)
	if !strings.Contains(out, "allow "+a.ID) {
		t.Fatalf("out = %q", out)
	}
	if d, ok := ask.LoadDecision(a.ID); !ok || !d.Allowed() || d.By != "slack:ops" {
		t.Fatalf("decision = %+v ok=%v", d, ok)
	}
	runDMFrontDoorCmd(t, "", "", "--channel", "slack", "--sender", "ops", "/deny", b.ID)
	if d, ok := ask.LoadDecision(b.ID); !ok || d.Allowed() {
		t.Fatalf("deny decision = %+v ok=%v", d, ok)
	}
	if len(*calls) != 0 {
		t.Fatal("an approval command invoked an agent")
	}
	if ev := inboundEvents(t, "slack:ops"); len(ev) != 2 {
		t.Fatalf("each command is an inbound event too: %+v", ev)
	}
}

// --revoke goes to the store; a revoked sender is back behind pairing.
func TestDMFrontDoorRevoke(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["slack:carol"] = []string{PeerScopeAsk}
	calls := stubInvoke(t, "no")

	out := runDMFrontDoorCmd(t, "", "--channel", "slack", "--sender", "carol", "--revoke")
	if !strings.Contains(out, "revoked carol") || len(f.revoked) != 1 {
		t.Fatalf("revoke output = %q revoked=%v", out, f.revoked)
	}
	out = runDMFrontDoorCmd(t, "", "--channel", "slack", "--sender", "carol", "--revoke")
	if !strings.Contains(out, "was not paired") {
		t.Fatalf("second revoke = %q", out)
	}
	out = runDMFrontDoorCmd(t, "", "codex", "--channel", "slack", "--sender", "carol", "hi")
	if !strings.Contains(out, "pair code") || len(*calls) != 0 {
		t.Fatalf("revoked sender must be re-paired: %q", out)
	}
}
