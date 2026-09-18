// This file is the real guard for defect 1: it lives in the EXTERNAL test
// package (acp_test) and exercises every exported symbol of pkg/acp WITHOUT
// importing github.com/coder/acp-go-sdk. If any exported type became an alias
// to an SDK type, or any exported signature mentioned an SDK type, this file
// would fail to compile — because it constructs values by bashy-owned field
// names and pins every function/method signature to bashy-owned + stdlib
// types only. A caller of pkg/acp must be able to do exactly this.
package acp_test

import (
	"context"
	"io"
	"os/exec"
	"reflect"
	"testing"

	"github.com/qiangli/yoke/pkg/acp"
)

// Pin the top-level constructor and every Client method to a fully-spelled
// signature using only bashy-owned + stdlib types. No SDK type may appear.
var (
	_ func(context.Context, acp.Handler, *exec.Cmd) (*acp.Client, error)         = acp.NewClient
	_ func(*acp.Client, context.Context, string) (string, error)                 = (*acp.Client).NewSession
	_ func(*acp.Client, context.Context, string, string) (acp.StopReason, error) = (*acp.Client).Prompt
	_ func(*acp.Client, context.Context, string) error                           = (*acp.Client).Cancel
	_ func(*acp.Client) error                                                    = (*acp.Client).Close
	_ func(*acp.Client) error                                                    = (*acp.Client).Kill
	_ func(*acp.Client) int                                                      = (*acp.Client).ProtocolVersion
	_ func(acp.Runner, acp.AgentOptions, io.Reader, io.Writer) *acp.Agent        = acp.NewAgent
	_ func(*acp.Agent) <-chan struct{}                                           = (*acp.Agent).Done
	_ func(*acp.Agent, string) (acp.AgentResult, bool)                           = (*acp.Agent).LastResult
)

// extHandler proves the public Handler interface is implementable from outside
// the package over bashy-owned types alone.
type extHandler struct{ acp.BaseHandler }

func (extHandler) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	_ = n.SessionID
	_ = n.Update
	return nil
}

var (
	_ acp.Handler = extHandler{}
	_ acp.Handler = (*acp.BaseHandler)(nil)
	_ acp.Handler = (*acp.RecordingHandler)(nil)
)

// TestPublicAPIIsSDKFree constructs and drives every exported symbol using
// bashy-owned field names and constants. It is both a compile-time guard and a
// behavioral smoke test.
func TestPublicAPIIsSDKFree(t *testing.T) {
	// Constants.
	if acp.ProtocolVersionNumber != 1 {
		t.Errorf("ProtocolVersionNumber = %d, want 1", acp.ProtocolVersionNumber)
	}

	// StopReason + ExitFor over the exit-code constants.
	exit := map[acp.StopReason]int{
		acp.StopReasonEndTurn:         acp.ExitEndTurn,
		acp.StopReasonRefusal:         acp.ExitRefusal,
		acp.StopReasonCancelled:       acp.ExitCancelled,
		acp.StopReasonMaxTokens:       acp.ExitMaxTokens,
		acp.StopReasonMaxTurnRequests: acp.ExitMaxTurnRequests,
		acp.StopReason("x"):           acp.ExitUnknown,
	}
	for sr, want := range exit {
		if got := acp.ExitFor(sr); got != want {
			t.Errorf("ExitFor(%q) = %d, want %d", sr, got, want)
		}
	}

	// ContentBlock + TextBlock.
	if b := acp.TextBlock("hi"); b.Text != "hi" {
		t.Errorf("TextBlock().Text = %q, want hi", b.Text)
	}
	_ = acp.ContentBlock{Text: "direct"}

	// Permission types + BaseHandler behavior over them.
	req := acp.PermissionRequest{
		SessionID: "s",
		ToolCall:  acp.ToolCall{ID: "tc", Title: "t", Status: acp.ToolCallPending, Kind: "edit"},
		Options: []acp.PermissionOption{
			{ID: "r", Name: "reject", Kind: acp.PermissionRejectAlways},
			{ID: "a", Name: "allow", Kind: acp.PermissionAllowOnce},
		},
	}
	resp, err := acp.BaseHandler{}.RequestPermission(context.Background(), req)
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if resp.OptionID != "a" || resp.Cancelled {
		t.Errorf("resp = %+v, want {OptionID:a}", resp)
	}
	_ = acp.PermissionResponse{OptionID: "x", Cancelled: false}
	_ = acp.PermissionAllowAlways
	_ = acp.PermissionRejectOnce

	// Agent-side request/result types remain SDK-free.
	var runner acp.Runner = acp.RunnerFunc(func(_ context.Context, req acp.TurnRequest) (acp.TurnResponse, error) {
		_ = req.SessionID
		_ = req.Cwd
		_ = req.Prompt
		_ = req.Capabilities.FSReadTextFile
		_ = req.Capabilities.FSWriteTextFile
		_ = req.Capabilities.Terminal
		return acp.TurnResponse{Text: "done", StopReason: acp.StopReasonEndTurn}, nil
	})
	_ = runner
	_ = acp.AgentOptions{Gate: "go test ./...", GateDir: ".", OnResult: func(result acp.AgentResult) {
		_ = result.SessionID
		_ = result.Text
		_ = result.ClaimedStopReason
		_ = result.StopReason
		_ = result.Gate
		_ = result.Succeeded()
	}}

	// RecordingHandler over bashy SessionNotification / SessionUpdate /
	// ToolCall / ToolCallLocation, then read back bashy-owned results.
	rec := &acp.RecordingHandler{}
	updates := []acp.SessionNotification{
		{SessionID: "s", Update: acp.SessionUpdate{UserMessageChunk: &acp.ContentBlock{Text: "prompt"}}},
		{SessionID: "s", Update: acp.SessionUpdate{AgentThoughtChunk: &acp.ContentBlock{Text: "thinking"}}},
		{SessionID: "s", Update: acp.SessionUpdate{AgentMessageChunk: &acp.ContentBlock{Text: "done "}}},
		{SessionID: "s", Update: acp.SessionUpdate{ToolCall: &acp.ToolCall{
			ID: "tc-1", Title: "edit", Status: acp.ToolCallInProgress, Kind: "edit",
			Locations: []acp.ToolCallLocation{{Path: "main.go", Line: 3}},
		}}},
		{SessionID: "s", Update: acp.SessionUpdate{ToolCallUpdate: &acp.ToolCall{
			ID: "tc-1", Status: acp.ToolCallCompleted,
			Locations: []acp.ToolCallLocation{{Path: "util.go"}},
		}}},
	}
	for _, u := range updates {
		if err := rec.SessionUpdate(context.Background(), u); err != nil {
			t.Fatalf("SessionUpdate: %v", err)
		}
	}
	if got := rec.AgentText(); got != "done " {
		t.Errorf("AgentText() = %q, want %q", got, "done ")
	}
	if got := rec.TouchedPaths(); !reflect.DeepEqual(got, []string{"main.go", "util.go"}) {
		t.Errorf("TouchedPaths() = %v, want [main.go util.go]", got)
	}
	wantEvents := []acp.ToolCallEvent{
		{ID: "tc-1", Title: "edit", Status: acp.ToolCallInProgress},
		{ID: "tc-1", Status: acp.ToolCallCompleted},
	}
	if got := rec.ToolCallEvents(); !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("ToolCallEvents() = %+v, want %+v", got, wantEvents)
	}

	// ToolCallFailed constant is part of the public surface.
	_ = acp.ToolCallFailed
}
