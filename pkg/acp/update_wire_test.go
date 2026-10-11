package acp

import (
	"encoding/json"
	"reflect"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

// The four update kinds the event contract maps (tool_call, plan, thought,
// usage) must survive the SDK boundary. Before plan and usage_update were
// converted they decoded fine and then vanished here, so the handler never saw
// them. Each case is the ACP wire JSON a real adapter sends.
func TestSessionUpdateWireGoldens(t *testing.T) {
	line := 7
	cases := []struct {
		name string
		wire string
		want SessionUpdate
	}{
		{
			name: "tool_call",
			wire: `{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Run go test","kind":"execute","status":"pending","locations":[{"path":"pkg/a.go","line":7}]}}`,
			want: SessionUpdate{ToolCall: &ToolCall{ID: "t1", Title: "Run go test", Kind: "execute", Status: ToolCallPending, Locations: []ToolCallLocation{{Path: "pkg/a.go", Line: line}}}},
		},
		{
			name: "tool_call_update",
			wire: `{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}`,
			want: SessionUpdate{ToolCallUpdate: &ToolCall{ID: "t1", Status: ToolCallCompleted}},
		},
		{
			name: "plan",
			wire: `{"sessionId":"s1","update":{"sessionUpdate":"plan","entries":[{"content":"read code","priority":"high","status":"completed"},{"content":"fix bug","priority":"medium","status":"in_progress"}]}}`,
			want: SessionUpdate{Plan: &Plan{Entries: []PlanEntry{
				{Content: "read code", Priority: "high", Status: "completed"},
				{Content: "fix bug", Priority: "medium", Status: "in_progress"},
			}}},
		},
		{
			name: "agent_thought_chunk",
			wire: `{"sessionId":"s1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}`,
			want: SessionUpdate{AgentThoughtChunk: &ContentBlock{Text: "thinking"}},
		},
		{
			name: "usage_update",
			wire: `{"sessionId":"s1","update":{"sessionUpdate":"usage_update","used":1200,"size":200000,"cost":{"amount":0.25,"currency":"USD"}}}`,
			want: SessionUpdate{Usage: &Usage{Used: 1200, Size: 200000, CostAmount: 0.25, CostCurrency: "USD"}},
		},
		{
			name: "usage_update without cost",
			wire: `{"sessionId":"s1","update":{"sessionUpdate":"usage_update","used":5,"size":10}}`,
			want: SessionUpdate{Usage: &Usage{Used: 5, Size: 10}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n sdk.SessionNotification
			if err := json.Unmarshal([]byte(tc.wire), &n); err != nil {
				t.Fatalf("decode wire: %v", err)
			}
			got := fromSDKNotification(n)
			if got.SessionID != "s1" {
				t.Errorf("SessionID = %q", got.SessionID)
			}
			if !reflect.DeepEqual(got.Update, tc.want) {
				t.Errorf("update =\n %#v\nwant\n %#v", got.Update, tc.want)
			}
		})
	}
}
