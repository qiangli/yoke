package cligw

// Sprint: #290 (W1: multi-turn tool loops through CLI seats)

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

func TestRenderShowsToolCallsAndPutsProtocolInSystem(t *testing.T) {
	req := &openai.ChatRequest{
		Messages: []openai.ChatMessage{
			{Role: "system", Content: json.RawMessage(`"solve it"`)},
			{Role: "user", Content: json.RawMessage(`"fix the bug"`)},
			{Role: "assistant", Content: json.RawMessage(`""`), ToolCalls: []openai.ToolCall{{ID: "c1", Type: "function",
				Function: openai.ToolFunction{Name: "bash", Arguments: `{"command":"ls /testbed"}`}}}},
			{Role: "tool", ToolCallID: "c1", Content: json.RawMessage(`"setup.py\nastropy"`)},
		},
		Tools: []openai.Tool{{Type: "function", Function: openai.ToolFunction{Name: "bash", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	p, err := RenderCompletionPrompt(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Prompt, `Assistant:`+"\n"+`{"tool_calls":[{"name":"bash","arguments":{"command":"ls /testbed"}}]}`) {
		t.Errorf("the assistant's tool call is not in the transcript:\n%s", p.Prompt)
	}
	if !strings.Contains(p.System, "Tool calling instructions") || strings.Contains(p.Prompt, "Tool calling instructions") {
		t.Errorf("the tool protocol belongs to the system channel:\nsystem=%q\nprompt=%q", p.System, p.Prompt)
	}
}
