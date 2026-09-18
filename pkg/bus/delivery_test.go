package bus

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// captureCmd is a cobra command whose stderr and stdout are buffers, so a
// receipt can be asserted on.
func captureCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	return cmd, &out, &errb
}

// EACH of the six provable states is reachable. deliveryState is the classifier
// every send path runs through, so pinning its branches pins the model.
func TestDeliveryState_AllSixAreReachable(t *testing.T) {
	boardInTempHome(t)

	// delivered — SteerLive succeeded, whatever the cursor says.
	if got := deliveryState("agent-x", 5, true, true); got != StateDelivered {
		t.Errorf("steered = %q, want delivered", got)
	}
	// accepted — resolved, but no single reader cursor to judge (a role/group).
	if got := deliveryState("steward.dragon", 5, false, false); got != StateAccepted {
		t.Errorf("role, not steered = %q, want accepted", got)
	}
	// unverified — a per-reader target that has NEVER read the board. This is
	// the state the old wording erased: "queued for X" claimed more than the
	// evidence, when no X had ever looked.
	if got := deliveryState("never-read", 5, false, true); got != StateUnverified {
		t.Errorf("no cursor = %q, want unverified", got)
	}
	// queued — a reader with a cursor behind the post.
	if err := MarkSeen("behind", 2); err != nil {
		t.Fatal(err)
	}
	if got := deliveryState("behind", 5, false, true); got != StateQueued {
		t.Errorf("cursor behind = %q, want queued", got)
	}
	// read — a reader whose cursor is at or past the post.
	if err := MarkSeen("caught-up", 5); err != nil {
		t.Fatal(err)
	}
	if got := deliveryState("caught-up", 5, false, true); got != StateRead {
		t.Errorf("cursor at seq = %q, want read", got)
	}
	// failed is the resolution outcome, asserted in the send tests below where a
	// target resolves to nothing.
}

// unverified vs queued is the crux: a never-read reader is not merely behind.
// CursorSeq is what draws the line, and it must not collapse "no cursor" into a
// cursor of zero.
func TestCursorSeq_DistinguishesNeverReadFromZero(t *testing.T) {
	boardInTempHome(t)
	if _, ok := CursorSeq("stranger"); ok {
		t.Fatal("a name that never read must report no cursor")
	}
	if err := MarkSeen("reader", 3); err != nil {
		t.Fatal(err)
	}
	if seq, ok := CursorSeq("reader"); !ok || seq != 3 {
		t.Fatalf("CursorSeq = %d,%v want 3,true", seq, ok)
	}
}

// THE DEFECT (issue (b)). `ping --as X profile-b "msg"` to a name that is no
// role, no agent and no reader must write NOTHING and report failed with near
// misses — not post with a literal to: and claim "waiting on the board for".
func TestPingSend_UnresolvableTargetWritesNothingAndFails(t *testing.T) {
	boardInTempHome(t)
	FleetNames = func() []string { return []string{"profile-a", "codex-gpt5.6-sol"} }
	t.Cleanup(func() { FleetNames = nil })

	cmd, _, _ := captureCmd()
	err := pingSend(cmd, "qiangli", "profile-b", "hello?")
	if err == nil {
		t.Fatal("a send to a nonexistent recipient must fail, not report a phantom delivery")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error does not name the failure: %v", err)
	}
	// Near misses: profile-a is one edit away and must be offered as a choice.
	if !strings.Contains(err.Error(), "profile-a") {
		t.Errorf("failure does not name the near miss profile-a: %v", err)
	}
	// NOTHING was written — the board is still empty.
	if posts, _ := Posts(); len(posts) != 0 {
		t.Fatalf("a failed send wrote %d post(s); it must write nothing", len(posts))
	}
}

func TestPingMessageCapRejectsBeforeBoardAppend(t *testing.T) {
	boardInTempHome(t)
	FleetNames = func() []string { return []string{"target-agent"} }
	t.Cleanup(func() { FleetNames = nil })
	cmd, _, _ := captureCmd()
	body := strings.Repeat("x", 1024)
	if err := pingSend(cmd, "sender", "target-agent", body); err != nil {
		t.Fatalf("1024-byte ping message rejected: %v", err)
	}
	before, err := Posts()
	if err != nil || len(before) != 1 || before[0].Body != body {
		t.Fatalf("accepted ping body changed: posts=%d err=%v", len(before), err)
	}
	err = pingSend(cmd, "sender", "target-agent", strings.Repeat("x", 1025))
	if err == nil || !strings.Contains(err.Error(), "1025 UTF-8 bytes") {
		t.Fatalf("oversized ping message error = %v", err)
	}
	after, err := Posts()
	if err != nil || len(after) != len(before) {
		t.Fatalf("rejected ping mutated board: before=%d after=%d err=%v", len(before), len(after), err)
	}
}

// A ROLE address (steward) still resolves and posts — the fix must not refuse a
// legitimate seat.
func TestPingSend_RoleStillResolves(t *testing.T) {
	boardInTempHome(t)
	withHostRoles(t, HostRole{Label: "steward", Topic: "steward.dragon-u501"})

	cmd, _, errb := captureCmd()
	if err := pingSend(cmd, "qiangli", "steward", "volunteering"); err != nil {
		t.Fatalf("a role must resolve, got %v", err)
	}
	posts, _ := Posts()
	if len(posts) != 1 || posts[0].To != "steward.dragon-u501" {
		t.Fatalf("role send stored %+v, want one post to the seat topic", posts)
	}
	// A role has no single cursor to judge, so the receipt is `accepted`, and it
	// is rendered by the human label, not the routing address.
	if r := errb.String(); !strings.Contains(r, "posted to the board for: steward") {
		t.Errorf("role receipt = %q, want an accepted line naming the seat", r)
	}
}

// An AGENT in the roster resolves; the receipt reflects the recipient's cursor
// state — unverified when it has never read, queued once it has a cursor behind
// the post.
func TestMBSend_AgentResolvesAndReceiptTracksCursor(t *testing.T) {
	boardInTempHome(t)
	FleetNames = func() []string { return []string{"codex-gpt5.6-sol"} }
	t.Cleanup(func() { FleetNames = nil })

	// Never read → unverified.
	cmd := newMBSendCmd()
	_, errb := attachBuffers(cmd)
	cmd.SetArgs([]string{"--as", "qiangli", "codex-gpt5.6-sol", "first ping"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("agent send failed: %v", err)
	}
	if r := errb.String(); !strings.Contains(r, "UNVERIFIED") {
		t.Errorf("first send to a never-read agent = %q, want unverified", r)
	}
	if posts, _ := Posts(); len(posts) != 1 {
		t.Fatalf("agent send wrote %d posts, want 1", len(posts))
	}

	// Now the agent has a cursor behind a later post → queued.
	if err := MarkSeen("codex-gpt5.6-sol", 1); err != nil {
		t.Fatal(err)
	}
	cmd2 := newMBSendCmd()
	_, errb2 := attachBuffers(cmd2)
	cmd2.SetArgs([]string{"--as", "qiangli", "codex-gpt5.6-sol", "second ping"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("second agent send failed: %v", err)
	}
	if r := errb2.String(); !strings.Contains(r, "queued on the board for") {
		t.Errorf("second send to a behind agent = %q, want queued", r)
	}
}

// An existing BOARD READER — a name with a cursor but not in the roster — is a
// demonstrable participant and resolves, so a human who has read the board can
// still be messaged by name.
func TestMBSend_ExistingReaderResolves(t *testing.T) {
	boardInTempHome(t)
	// No roster wired; the only evidence "qiangli" exists is a cursor it wrote
	// by having read the board once.
	if err := MarkSeen("qiangli", 1); err != nil {
		t.Fatal(err)
	}
	addr, kind, ok := ResolveSendTarget("qiangli")
	if !ok || kind != TargetReader || addr != "qiangli" {
		t.Fatalf("ResolveSendTarget(qiangli) = %q,%q,%v want qiangli,reader,true", addr, kind, ok)
	}
	// And an unresolvable name is rejected even with a reader present, offering
	// the reader as a near miss.
	if _, _, ok := ResolveSendTarget("qiangl"); ok {
		t.Fatal("a typo of a reader must not resolve")
	}
}

// reportDelivery renders every state under a distinct, honest label, and a
// delivery with no state set is at least `accepted` (it was posted).
func TestReportDelivery_LabelsEachState(t *testing.T) {
	cmd, _, errb := captureCmd()
	reportDelivery(cmd, []Delivery{
		{To: "a", State: StateDelivered},
		{To: "b", State: StateQueued},
		{To: "c", State: StateUnverified},
		{To: "d", State: StateRead},
		{To: "e", State: StateAccepted},
		{To: "f"}, // no state → accepted
	})
	r := errb.String()
	for _, want := range []string{
		"delivered now (live session): a",
		"queued on the board for: b",
		"no read cursor yet for: c",
		"already read by: d",
		"posted to the board for: e, f",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("receipt missing %q in:\n%s", want, r)
		}
	}
}

// attachBuffers points a command's stdout/stderr at fresh buffers.
func attachBuffers(cmd *cobra.Command) (*bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	return &out, &errb
}
