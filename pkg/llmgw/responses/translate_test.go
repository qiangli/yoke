package responses

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

// The shape follows ycode's buildResponsesRequest: flattened message and
// function items, flat function tools, store=false, and turn-local reasoning.
const ycodeRequest = `{"model":"gpt-6-sol","input":[{"role":"user","content":"Find the answer"},{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"/tmp/work"}],"instructions":"Be concise","tools":[{"type":"function","name":"shell","description":"run a command","parameters":{"type":"object"}}],"tool_choice":"auto","max_output_tokens":512,"reasoning":{"effort":"high"},"stream":true,"store":false,"include":["reasoning.encrypted_content"]}`

func TestYcodeRequestToChat(t *testing.T) {
	req, stream, err := ToChat([]byte(ycodeRequest))
	if err != nil {
		t.Fatal(err)
	}
	if !stream || req.Stream || req.Model != "gpt-6-sol" || req.MaxCompletionTokens != 512 || req.ReasoningEffort != "high" {
		t.Fatalf("request = %+v, stream=%v", req, stream)
	}
	if len(req.Messages) != 4 || req.Messages[0].Role != "system" || req.Messages[2].ToolCalls[0].ID != "call_1" || req.Messages[3].ToolCallID != "call_1" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if got := openai.FlattenContent(req.Messages[3].Content); got != "/tmp/work" {
		t.Fatalf("tool output = %q", got)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "shell" || req.ToolChoice != "auto" {
		t.Fatalf("tools = %+v, choice=%v", req.Tools, req.ToolChoice)
	}
}

func TestStatefulRequestsRejected(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-6-sol","input":"hi","previous_response_id":"resp_1"}`,
		`{"model":"gpt-6-sol","input":"hi","store":true}`,
	} {
		if _, _, err := ToChat([]byte(body)); err == nil || !strings.Contains(err.Error(), "server-side response state") {
			t.Fatalf("body %s: %v", body, err)
		}
	}
}

func TestChatCompletionToResponseAndSSE(t *testing.T) {
	comp := &openai.ChatCompletion{ID: "chat_1", Model: "gpt-6-sol", Created: 123, Usage: openai.Usage{PromptTokens: 10, CompletionTokens: 4}, Choices: []openai.ChatChoice{{Message: &openai.ChatResponseMessage{Content: "done", ToolCalls: []openai.ToolCallDelta{{ID: "call_2", Type: "function", Function: openai.ToolFunction{Name: "shell", Arguments: `{"cmd":"ls"}`}}}}}}}
	resp, err := FromChat(comp)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Object != "response" || len(resp.Output) != 2 || resp.Output[0].Content[0]["text"] != "done" || resp.Output[1].CallID != "call_2" || resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("response = %+v", resp)
	}
	var buf bytes.Buffer
	if err := WriteSSE(&buf, resp); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"response.created", "response.output_text.delta", "response.output_item.done", "response.completed"} {
		if !strings.Contains(buf.String(), "event: "+kind) {
			t.Fatalf("missing %s: %s", kind, buf.String())
		}
	}
	var final map[string]any
	lines := strings.Split(buf.String(), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &final); err != nil {
				t.Fatal(err)
			}
		}
	}
	if final["type"] != "response.completed" {
		t.Fatalf("final event = %v", final)
	}
}
