package acp

import (
	"context"
	"reflect"
	"testing"
)

func toolCallUpdate(tc ToolCall) SessionNotification {
	return SessionNotification{Update: SessionUpdate{ToolCallUpdate: &tc}}
}
func toolCallStart(tc ToolCall) SessionNotification {
	return SessionNotification{Update: SessionUpdate{ToolCall: &tc}}
}
func agentText(s string) SessionNotification {
	return SessionNotification{Update: SessionUpdate{AgentMessageChunk: &ContentBlock{Text: s}}}
}

func TestRecordingHandler(t *testing.T) {
	tests := []struct {
		name       string
		updates    []SessionNotification
		wantText   string
		wantPaths  []string
		wantEvents []ToolCallEvent
	}{
		{
			name:      "agent text is concatenated",
			updates:   []SessionNotification{agentText("hello "), agentText("world")},
			wantText:  "hello world",
			wantPaths: nil,
		},
		{
			name: "tool call locations captured and deduped in order",
			updates: []SessionNotification{
				toolCallStart(ToolCall{ID: "t1", Status: ToolCallPending, Locations: []ToolCallLocation{{Path: "a.go"}, {Path: "b.go"}}}),
				toolCallUpdate(ToolCall{ID: "t1", Status: ToolCallInProgress, Locations: []ToolCallLocation{{Path: "b.go"}, {Path: "c.go"}}}),
				toolCallUpdate(ToolCall{ID: "t1", Status: ToolCallCompleted}),
			},
			wantPaths: []string{"a.go", "b.go", "c.go"},
			wantEvents: []ToolCallEvent{
				{ID: "t1", Status: ToolCallPending},
				{ID: "t1", Status: ToolCallInProgress},
				{ID: "t1", Status: ToolCallCompleted},
			},
		},
		{
			name: "status transitions include failed",
			updates: []SessionNotification{
				toolCallStart(ToolCall{ID: "t2", Title: "run", Status: ToolCallInProgress}),
				toolCallUpdate(ToolCall{ID: "t2", Status: ToolCallFailed}),
			},
			wantEvents: []ToolCallEvent{
				{ID: "t2", Title: "run", Status: ToolCallInProgress},
				{ID: "t2", Status: ToolCallFailed},
			},
		},
		{
			name: "empty-path locations and empty status are ignored",
			updates: []SessionNotification{
				toolCallUpdate(ToolCall{ID: "t3", Locations: []ToolCallLocation{{Path: ""}}}),
			},
			wantPaths:  nil,
			wantEvents: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &RecordingHandler{}
			for _, u := range tt.updates {
				if err := h.SessionUpdate(context.Background(), u); err != nil {
					t.Fatalf("SessionUpdate: %v", err)
				}
			}
			if got := h.AgentText(); got != tt.wantText {
				t.Errorf("AgentText() = %q, want %q", got, tt.wantText)
			}
			if got := h.TouchedPaths(); !reflect.DeepEqual(got, tt.wantPaths) {
				t.Errorf("TouchedPaths() = %v, want %v", got, tt.wantPaths)
			}
			if got := h.ToolCallEvents(); !reflect.DeepEqual(got, tt.wantEvents) {
				t.Errorf("ToolCallEvents() = %+v, want %+v", got, tt.wantEvents)
			}
		})
	}
}

func TestBaseHandler_RequestPermission(t *testing.T) {
	tests := []struct {
		name          string
		options       []PermissionOption
		wantOptionID  string
		wantCancelled bool
	}{
		{
			name: "selects allow_once when offered",
			options: []PermissionOption{
				{ID: "reject", Kind: PermissionRejectOnce},
				{ID: "ok", Kind: PermissionAllowOnce},
			},
			wantOptionID: "ok",
		},
		{
			name:          "cancels when no allow_once option",
			options:       []PermissionOption{{ID: "always", Kind: PermissionAllowAlways}},
			wantCancelled: true,
		},
		{
			name:          "cancels on empty options",
			options:       nil,
			wantCancelled: true,
		},
	}

	var b BaseHandler
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := b.RequestPermission(context.Background(), PermissionRequest{Options: tt.options})
			if err != nil {
				t.Fatalf("RequestPermission: %v", err)
			}
			if resp.Cancelled != tt.wantCancelled {
				t.Errorf("Cancelled = %v, want %v", resp.Cancelled, tt.wantCancelled)
			}
			if resp.OptionID != tt.wantOptionID {
				t.Errorf("OptionID = %q, want %q", resp.OptionID, tt.wantOptionID)
			}
		})
	}
}

func TestExitFor(t *testing.T) {
	tests := []struct {
		reason StopReason
		want   int
	}{
		{StopReasonEndTurn, ExitEndTurn},
		{StopReasonRefusal, ExitRefusal},
		{StopReasonCancelled, ExitCancelled},
		{StopReasonMaxTokens, ExitMaxTokens},
		{StopReasonMaxTurnRequests, ExitMaxTurnRequests},
		{StopReason(""), ExitUnknown},
		{StopReason("bogus"), ExitUnknown},
	}
	for _, tt := range tests {
		if got := ExitFor(tt.reason); got != tt.want {
			t.Errorf("ExitFor(%q) = %d, want %d", tt.reason, got, tt.want)
		}
	}
}
