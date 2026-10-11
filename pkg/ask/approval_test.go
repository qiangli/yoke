package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

func isolateApprovals(t *testing.T) {
	t.Helper()
	isolate(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
}

func approvalEvents(t *testing.T) []room.Event {
	t.Helper()
	evs, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []room.Event
	for _, e := range evs {
		if e.Type == room.EventApproval {
			out = append(out, e)
		}
	}
	return out
}

func runApprove(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewApproveCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

// The token must equal @confirm's confirmToken, or a yielded @confirm and its
// resume would never find the same record.
func TestPlanTokenMatchesConfirm(t *testing.T) {
	if got := PlanToken([]string{"rm", "-rf", "x"}); len(got) != 8 {
		t.Fatalf("token %q is not 8 hex chars", got)
	}
	if PlanToken([]string{"a", "b"}) == PlanToken([]string{"ab"}) {
		t.Fatal("argv boundaries must change the token")
	}
	if PlanToken([]string{"a"}) != PlanToken([]string{"a"}) {
		t.Fatal("token must be stable")
	}
}

func TestResolveRequiresExactSessionAndArgv(t *testing.T) {
	isolateApprovals(t)
	r, err := Raise(ApprovalSpec{Source: SourceConfirm, Token: "deadbeef", Session: "card-a", Argv: []string{"rm", "target-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decide(r.ID, Decision{Verdict: VerdictAllow}, DecideOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := Resolve(SourceConfirm, "deadbeef", "card-b", []string{"rm", "target-a"}); ok {
		t.Fatal("approval escaped its session")
	}
	if _, ok := Resolve(SourceConfirm, "deadbeef", "card-a", []string{"rm", "target-b"}); ok {
		t.Fatal("short-token collision approved different argv")
	}
	if d, ok := Resolve(SourceConfirm, "deadbeef", "card-a", []string{"rm", "target-a"}); !ok || !d.Allowed() {
		t.Fatalf("matching operation was refused: %+v, %v", d, ok)
	}
}

// A yielded @confirm becomes a listed approval; allow resumes it with the same
// token; the decision and the raise are both evented.
func TestYieldedConfirmListedAllowedResumed(t *testing.T) {
	isolateApprovals(t)
	argv := []string{"git", "push", "--force"}
	token := PlanToken(argv)

	out, _, err := runApprove(t, "request", "--source", "confirm", "--effects", "destroy", "--", "git", "push", "--force")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(out)

	out, _, err = runApprove(t, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Approvals []approvalView `json:"approvals"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Approvals) != 1 || listed.Approvals[0].ID != id || listed.Approvals[0].Token != token ||
		listed.Approvals[0].State != "pending" {
		t.Fatalf("listed = %+v", listed.Approvals)
	}
	if got := listed.Approvals[0].Effects; len(got) != 1 || got[0] != "destroy" {
		t.Fatalf("effects = %v", got)
	}

	// Before a decision, the resume finds nothing.
	if _, ok := Resolve(SourceConfirm, token, "", argv); ok {
		t.Fatal("Resolve found a decision for an undecided approval")
	}

	out, _, err = runApprove(t, "allow", shortID(id), "--token", token)
	if err != nil {
		t.Fatal(err)
	}
	if want := "--confirm=" + token + ":yes"; strings.TrimSpace(out) != want {
		t.Fatalf("allow printed %q, want %q", out, want)
	}

	d, ok := Resolve(SourceConfirm, token, "", argv)
	if !ok || !d.Allowed() {
		t.Fatalf("Resolve = %+v, %v", d, ok)
	}
	// A once-allow is single use.
	if _, ok := Resolve(SourceConfirm, token, "", argv); ok {
		t.Fatal("a once-allow resumed twice")
	}

	var topics []string
	for _, e := range approvalEvents(t) {
		topics = append(topics, e.Topic)
		if !strings.HasPrefix(e.Target, "approval:") {
			t.Errorf("event target %q", e.Target)
		}
	}
	if strings.Join(topics, ",") != "pending,allow,resume" {
		t.Fatalf("event topics = %v", topics)
	}
}

func TestDecideFirstWinsAndTokenCheck(t *testing.T) {
	isolateApprovals(t)
	r, err := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"rm", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decide(r.ID, Decision{Verdict: VerdictAllow}, DecideOpts{Token: "deadbeef"}); err == nil {
		t.Fatal("a wrong token must be refused")
	}
	if _, ok := LoadDecision(r.ID); ok {
		t.Fatal("a refused decision was recorded")
	}
	if _, err := Decide(r.ID, Decision{Verdict: VerdictDeny, Reason: "no"}, DecideOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Decide(r.ID, Decision{Verdict: VerdictAllow}, DecideOpts{}); !errors.Is(err, ErrDecided) {
		t.Fatalf("second decision err = %v, want ErrDecided", err)
	}
	d, _ := LoadDecision(r.ID)
	if d.Allowed() || d.Reason != "no" {
		t.Fatalf("stood decision = %+v", d)
	}
	// A deny stays a deny on every replay.
	for i := 0; i < 2; i++ {
		if d, ok := Resolve(SourceConfirm, r.Approval.Token, r.Approval.Session, r.Approval.Argv); !ok || d.Allowed() {
			t.Fatalf("replay %d = %+v, %v", i, d, ok)
		}
	}
	if _, err := Decide(r.ID, Decision{Verdict: "maybe"}, DecideOpts{}); err == nil {
		t.Fatal("an unknown verdict must be refused")
	}
}

func TestAlwaysAllowIsReusable(t *testing.T) {
	isolateApprovals(t)
	r, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"make", "deploy"}})
	if _, err := Decide(r.ID, Decision{Verdict: VerdictAllow, Always: true}, DecideOpts{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if d, ok := Resolve(SourceConfirm, r.Approval.Token, r.Approval.Session, r.Approval.Argv); !ok || !d.Allowed() {
			t.Fatalf("always-allow use %d = %+v, %v", i, d, ok)
		}
	}
	// A deny can never be "always".
	r2, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"x"}})
	_, _ = Decide(r2.ID, Decision{Verdict: VerdictDeny, Always: true}, DecideOpts{})
	if d, _ := LoadDecision(r2.ID); d.Always {
		t.Fatal("deny recorded as always")
	}
}

func TestRaiseIsIdempotentWhilePending(t *testing.T) {
	isolateApprovals(t)
	a, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"x", "y"}})
	b, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"x", "y"}})
	if a.ID != b.ID {
		t.Fatalf("re-yield raised a second approval: %s vs %s", a.ID, b.ID)
	}
	c, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"x", "z"}})
	if c.ID == a.ID {
		t.Fatal("a different operation shared an approval")
	}
	if _, err := Raise(ApprovalSpec{}); err == nil {
		t.Fatal("an approval without a source must be refused")
	}
}

// The requester of a yielded @confirm has exited by design; its approval must
// outlive it, unlike a value ask whose waiter died.
func TestApprovalSurvivesDeadRequester(t *testing.T) {
	isolateApprovals(t)
	r, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"x"}})
	path := filepath.Join(requestDir(r.ID), requestFile)
	b, _ := os.ReadFile(path)
	var raw Request
	_ = json.Unmarshal(b, &raw)
	raw.Requester.PID = 0x7ffffff0
	b, _ = json.Marshal(raw)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingApprovals()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, %v", pending, err)
	}
}

func TestApprovalExpiryAndRetention(t *testing.T) {
	isolateApprovals(t)
	r, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"x"}, TTL: time.Minute})
	if approvalExpired(r, time.Now()) {
		t.Fatal("fresh approval expired")
	}
	if !approvalExpired(r, time.Now().Add(2*time.Minute)) {
		t.Fatal("an undecided approval must expire at its TTL")
	}
	_, _ = Decide(r.ID, Decision{Verdict: VerdictAllow}, DecideOpts{})
	if approvalExpired(r, time.Now().Add(2*time.Minute)) {
		t.Fatal("a decided approval must stay redeemable through the retention window")
	}
	if !approvalExpired(r, time.Now().Add(DecisionRetention+time.Minute)) {
		t.Fatal("a decided approval must age out")
	}
}

func TestWaitDecisionAndCLIWait(t *testing.T) {
	isolateApprovals(t)
	r, _ := Raise(ApprovalSpec{Source: SourceACP, Argv: []string{"acp:edit", "a.go"}})
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = Decide(r.ID, Decision{Verdict: VerdictAllow}, DecideOpts{})
	}()
	out, _, err := runApprove(t, "wait", shortID(r.ID), "--timeout", "5s")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "--confirm="+r.Approval.Token+":yes" {
		t.Fatalf("wait printed %q", out)
	}

	// Timeout is its own status, distinct from deny.
	r2, _ := Raise(ApprovalSpec{Source: SourceACP, Argv: []string{"acp:edit", "b.go"}})
	_, _, err = runApprove(t, "wait", shortID(r2.ID), "--timeout", "150ms")
	var ex *ApprovalExit
	if !errors.As(err, &ex) || ex.Code != ExitApprovalTimeout {
		t.Fatalf("timeout err = %v", err)
	}
	// Deny exits 1.
	_, _ = Decide(r2.ID, Decision{Verdict: VerdictDeny}, DecideOpts{})
	_, _, err = runApprove(t, "wait", shortID(r2.ID), "--timeout", "1s")
	if !errors.As(err, &ex) || ex.Code != ExitApprovalDenied {
		t.Fatalf("deny err = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r3, _ := Raise(ApprovalSpec{Source: SourceACP, Argv: []string{"c"}})
	if _, err := WaitDecision(ctx, r3.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDecision err = %v", err)
	}
}

func TestDenyUnansweredAndRace(t *testing.T) {
	isolateApprovals(t)
	r, _ := Raise(ApprovalSpec{Source: SourceACP, Argv: []string{"x"}})
	d := DenyUnanswered(r.ID, "nobody")
	if d.Allowed() || d.Via != ViaTimeout {
		t.Fatalf("timeout decision = %+v", d)
	}
	// Losing the race to a real decision returns the one that stands.
	r2, _ := Raise(ApprovalSpec{Source: SourceACP, Argv: []string{"y"}})
	_, _ = Decide(r2.ID, Decision{Verdict: VerdictAllow}, DecideOpts{})
	if d := DenyUnanswered(r2.ID, "late"); !d.Allowed() {
		t.Fatalf("a late timeout overrode a real allow: %+v", d)
	}
}

func TestReviewDecidesOrFallsBackToAsk(t *testing.T) {
	isolateApprovals(t)
	mk := func(arg string) Request {
		r, err := Raise(ApprovalSpec{Source: SourceACP, Argv: []string{arg}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ctx := context.Background()

	r := mk("ok")
	d, ok := Review(ctx, r, func(context.Context, Request) (string, string, error) { return "ALLOW", "read only", nil }, time.Second)
	if !ok || !d.Allowed() || d.Via != ViaReview || d.Always {
		t.Fatalf("reviewer allow = %+v, %v", d, ok)
	}

	for name, rv := range map[string]ReviewFunc{
		"error":   func(context.Context, Request) (string, string, error) { return "allow", "", errors.New("boom") },
		"unsure":  func(context.Context, Request) (string, string, error) { return "unsure", "", nil },
		"nil":     nil,
		"timeout": func(c context.Context, _ Request) (string, string, error) { <-c.Done(); return "allow", "", nil },
	} {
		r := mk(name)
		start := time.Now()
		if _, ok := Review(ctx, r, rv, 100*time.Millisecond); ok {
			t.Errorf("%s: review decided when it could not", name)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("%s: review was not bounded", name)
		}
		if _, decided := LoadDecision(r.ID); decided {
			t.Errorf("%s: fallback recorded a decision; it must stay pending for a human", name)
		}
	}
}

func TestGrantSettlesPendingOrRecords(t *testing.T) {
	isolateApprovals(t)
	r, _ := Raise(ApprovalSpec{
		Source: SourceACP, Argv: []string{"acp:edit"}, Session: "sess-1",
		Options: []ApprovalOption{{ID: "o1", Kind: "allow_once"}, {ID: "o2", Kind: "allow_always"}},
	})
	if got, _ := PendingForSession("sess-1"); len(got) != 1 {
		t.Fatalf("PendingForSession = %d", len(got))
	}
	settled, err := Grant("sess-1", "human", true, nil)
	if err != nil || len(settled) != 1 || settled[0].ID != r.ID {
		t.Fatalf("Grant = %v, %v", settled, err)
	}
	d, _ := LoadDecision(r.ID)
	if !d.Allowed() || !d.Always || d.OptionID != "o2" || d.Via != ViaChat {
		t.Fatalf("decision = %+v", d)
	}

	// With nothing pending the grant is still on the books.
	made, err := Grant("sess-2", "human", false, []string{"chat", "grant", "sess-2", "allow-once"})
	if err != nil || len(made) != 1 {
		t.Fatalf("Grant (none pending) = %v, %v", made, err)
	}
	if d, ok := LoadDecision(made[0].ID); !ok || !d.Allowed() || d.Always {
		t.Fatalf("recorded grant = %+v, %v", d, ok)
	}
}

// Approvals share the store but must never be offered a value prompt.
func TestAskVerbsIgnoreApprovals(t *testing.T) {
	isolateApprovals(t)
	r, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"x"}})

	if _, err := pick(nil); err == nil || !strings.Contains(err.Error(), "nothing is waiting") {
		t.Fatalf("ask claim would have picked an approval: %v", err)
	}
	cmd := newAskCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), shortID(r.ID)) {
		t.Fatalf("ask ls listed an approval:\n%s", out.String())
	}
	cmd = newAskCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader("secret\n"))
	cmd.SetArgs([]string{"answer", shortID(r.ID)})
	if err := cmd.Execute(); err == nil {
		t.Fatal("ask answer delivered a value to an approval")
	}
	if _, _, err := runApprove(t, "show", "nonexistent"); err == nil {
		t.Fatal("show of a missing id must fail")
	}
}

func TestApproveShowAndDenyJSON(t *testing.T) {
	isolateApprovals(t)
	r, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"terraform", "destroy"}, Effects: []string{"destroy"}, Sprint: "#418"})
	out, _, err := runApprove(t, "show", shortID(r.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"terraform destroy", "destroy", "#418", r.Approval.Token, "pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("show missing %q:\n%s", want, out)
		}
	}
	out, _, err = runApprove(t, "deny", shortID(r.ID), "--reason", "prod", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v approvalView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if v.State != VerdictDeny || v.Resume != "--confirm="+r.Approval.Token+":no" || v.Decision.Reason != "prod" {
		t.Fatalf("deny view = %+v", v)
	}
	if _, _, err := runApprove(t, "allow", shortID(r.ID)); err == nil {
		t.Fatal("deciding twice must fail")
	}
	if out, _, _ := runApprove(t, "list"); !strings.Contains(out, "nothing pending") {
		t.Fatalf("list after decision:\n%s", out)
	}
	if out, _, _ := runApprove(t, "list", "--all"); !strings.Contains(out, "deny") {
		t.Fatalf("list --all:\n%s", out)
	}
}
