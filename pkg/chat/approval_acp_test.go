package chat

import (
	"context"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/acp"
	"github.com/qiangli/yoke/pkg/ask"
)

func chatPermReq() acp.PermissionRequest {
	return acp.PermissionRequest{
		SessionID: "s",
		ToolCall:  acp.ToolCall{Title: "run tests", Kind: "execute"},
		Options: []acp.PermissionOption{
			{ID: "ao", Kind: acp.PermissionAllowOnce},
			{ID: "aa", Kind: acp.PermissionAllowAlways},
			{ID: "ro", Kind: acp.PermissionRejectOnce},
		},
	}
}

// Default (no approvals handler, i.e. permission auto): auto-allow, unchanged.
func TestACPHandlerDefaultStaysAutoAllow(t *testing.T) {
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	got, err := (&acpHandler{}).RequestPermission(context.Background(), chatPermReq())
	if err != nil || got.OptionID != "ao" {
		t.Fatalf("default = %+v, %v", got, err)
	}
	if p, _ := ask.PendingApprovals(); len(p) != 0 {
		t.Fatal("the default raised an approval")
	}
}

// `chat grant` settles the SAME record an ACP request raised, and the blocked
// agent receives the matching option.
func TestChatGrantSettlesACPPermissionRequest(t *testing.T) {
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	h := &acpHandler{approvals: &acp.ApprovalHandler{
		Mode:    "ask",
		Session: func() string { return "card-9" },
	}}
	done := make(chan acp.PermissionResponse, 1)
	go func() {
		got, _ := h.RequestPermission(context.Background(), chatPermReq())
		done <- got
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if q, _ := ask.PendingForSession("card-9"); len(q) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no approval raised")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := ask.Grant("card-9", "human", true, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.OptionID != "aa" {
		t.Fatalf("grant always -> %+v", got)
	}
}
