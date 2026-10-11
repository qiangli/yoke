package acp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
)

func isolateQueue(t *testing.T) {
	t.Helper()
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
}

func permReq() PermissionRequest {
	return PermissionRequest{
		SessionID: "s1",
		ToolCall:  ToolCall{ID: "t1", Title: "edit main.go", Kind: "edit", Locations: []ToolCallLocation{{Path: "main.go"}}},
		Options: []PermissionOption{
			{ID: "ao", Name: "Allow once", Kind: PermissionAllowOnce},
			{ID: "aa", Name: "Always allow", Kind: PermissionAllowAlways},
			{ID: "ro", Name: "Reject", Kind: PermissionRejectOnce},
		},
	}
}

// decideSoon answers the next pending approval from another goroutine, the way
// `bashy approve allow|deny` would from a human's terminal.
func decideSoon(t *testing.T, d ask.Decision) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if p, _ := ask.PendingApprovals(); len(p) > 0 {
				if _, err := ask.Decide(p[0].ID, d, ask.DecideOpts{}); err != nil {
					t.Errorf("decide: %v", err)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("no approval was raised")
	}()
}

// The default must be unchanged: auto selects allow-once and raises nothing.
func TestAutoModeUnchangedAndRaisesNothing(t *testing.T) {
	isolateQueue(t)
	want, _ := BaseHandler{}.RequestPermission(context.Background(), permReq())
	for _, mode := range []string{"", PermissionModeAuto, "AUTO"} {
		h := &ApprovalHandler{Mode: mode}
		got, err := h.RequestPermission(context.Background(), permReq())
		if err != nil || got != want || got.OptionID != "ao" {
			t.Fatalf("mode %q = %+v, %v; want %+v", mode, got, err, want)
		}
	}
	if p, _ := ask.PendingApprovals(); len(p) != 0 {
		t.Fatalf("auto raised %d approvals", len(p))
	}
}

func TestAskRoundTripsThroughQueue(t *testing.T) {
	isolateQueue(t)
	h := &ApprovalHandler{Mode: PermissionModeAsk, Session: func() string { return "card-1" }}
	decideSoon(t, ask.Decision{Verdict: ask.VerdictAllow})
	got, err := h.RequestPermission(context.Background(), permReq())
	if err != nil || got.OptionID != "ao" || got.Cancelled {
		t.Fatalf("allow = %+v, %v", got, err)
	}
	all, _ := ask.ListApprovals()
	if len(all) != 1 {
		t.Fatalf("approvals = %d", len(all))
	}
	a := all[0].Approval
	if a.Source != ask.SourceACP || a.Session != "card-1" || a.Mode != "ask" || len(a.Options) != 3 || a.Argv[1] != "edit main.go" {
		t.Fatalf("approval = %+v", a)
	}
}

func TestAskAlwaysAndDenyMapToOptions(t *testing.T) {
	isolateQueue(t)
	h := &ApprovalHandler{Mode: PermissionModeAsk}
	decideSoon(t, ask.Decision{Verdict: ask.VerdictAllow, Always: true})
	if got, _ := h.RequestPermission(context.Background(), permReq()); got.OptionID != "aa" {
		t.Fatalf("always = %+v", got)
	}
	decideSoon(t, ask.Decision{Verdict: ask.VerdictDeny})
	if got, _ := h.RequestPermission(context.Background(), permReq()); got.OptionID != "ro" || got.Cancelled {
		t.Fatalf("deny = %+v", got)
	}
}

func TestDenyModeRejectsWithoutAsking(t *testing.T) {
	isolateQueue(t)
	h := &ApprovalHandler{Mode: PermissionModeDeny}
	got, _ := h.RequestPermission(context.Background(), permReq())
	if got.OptionID != "ro" {
		t.Fatalf("deny mode = %+v", got)
	}
	if p, _ := ask.PendingApprovals(); len(p) != 0 {
		t.Fatal("deny mode raised an approval")
	}
	// No reject option on offer: cancel, never allow.
	req := permReq()
	req.Options = req.Options[:2]
	if got, _ := h.RequestPermission(context.Background(), req); !got.Cancelled {
		t.Fatalf("no reject option = %+v", got)
	}
	// An unknown mode must not fail open.
	if got, _ := (&ApprovalHandler{Mode: "yolo"}).RequestPermission(context.Background(), permReq()); got.OptionID == "ao" {
		t.Fatal("unknown mode allowed")
	}
}

func TestUnansweredAskIsDenied(t *testing.T) {
	isolateQueue(t)
	h := &ApprovalHandler{Mode: PermissionModeAsk, Wait: 150 * time.Millisecond}
	got, _ := h.RequestPermission(context.Background(), permReq())
	if got.OptionID != "ro" {
		t.Fatalf("timeout = %+v, want reject", got)
	}
	all, _ := ask.ListApprovals()
	if d, ok := ask.LoadDecision(all[0].ID); !ok || d.Via != ask.ViaTimeout || d.Allowed() {
		t.Fatalf("recorded = %+v, %v", d, ok)
	}
}

func TestContextCancelCancelsTurn(t *testing.T) {
	isolateQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	got, _ := (&ApprovalHandler{Mode: PermissionModeAsk}).RequestPermission(ctx, permReq())
	if !got.Cancelled {
		t.Fatalf("cancelled ctx = %+v", got)
	}
}

func TestReviewDecidesOrFallsBackToHuman(t *testing.T) {
	isolateQueue(t)
	h := &ApprovalHandler{
		Mode:     PermissionModeReview,
		Reviewer: func(context.Context, ask.Request) (string, string, error) { return "allow", "safe", nil },
	}
	start := time.Now()
	if got, _ := h.RequestPermission(context.Background(), permReq()); got.OptionID != "ao" {
		t.Fatalf("review allow = %+v", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("a definite review should not wait for a human")
	}

	// A reviewer that errors falls back to ask, answered by a human.
	h = &ApprovalHandler{
		Mode:          PermissionModeReview,
		ReviewTimeout: 100 * time.Millisecond,
		Reviewer:      func(context.Context, ask.Request) (string, string, error) { return "", "", errors.New("door down") },
	}
	decideSoon(t, ask.Decision{Verdict: ask.VerdictDeny})
	if got, _ := h.RequestPermission(context.Background(), permReq()); got.OptionID != "ro" {
		t.Fatalf("review fallback = %+v", got)
	}
}

type recordingInner struct {
	BaseHandler
	got int
}

func (r *recordingInner) SessionUpdate(context.Context, SessionNotification) error {
	r.got++
	return nil
}

func TestSessionUpdateDelegates(t *testing.T) {
	inner := &recordingInner{}
	h := &ApprovalHandler{Inner: inner}
	_ = h.SessionUpdate(context.Background(), SessionNotification{})
	_ = (&ApprovalHandler{}).SessionUpdate(context.Background(), SessionNotification{})
	if inner.got != 1 {
		t.Fatalf("delegated %d", inner.got)
	}
}
