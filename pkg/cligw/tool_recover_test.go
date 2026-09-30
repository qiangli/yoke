package cligw

import (
	"encoding/json"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

func TestRecoverBareToolArguments(t *testing.T) {
	bashy := openai.Tool{Type: "function", Function: openai.ToolFunction{Name: "bashy", Parameters: json.RawMessage(`{"type":"object","properties":{"script":{"type":"string"},"timeout_ms":{"type":"integer"}},"required":["script"]}`)}}
	other := openai.Tool{Type: "function", Function: openai.ToolFunction{Name: "other", Parameters: json.RawMessage(`{"type":"object","properties":{"script":{"type":"string"}},"required":["script"]}`)}}
	for _, tc := range []struct {
		name, answer, want string
		tools              []openai.Tool
	}{
		{"recorded codex answer", `{"script":"pwd; echo x > probe2.txt; ls -la probe2.txt"}Done.`, `{"tool_calls":[{"name":"bashy","arguments":{"script":"pwd; echo x > probe2.txt; ls -la probe2.txt"}}]}`, []openai.Tool{bashy}},
		{"fenced arguments", "```json\n{\"script\":\"pwd\"}\n```Done.", `{"tool_calls":[{"name":"bashy","arguments":{"script":"pwd"}}]}`, []openai.Tool{bashy}},
		{"wrong argument type", `{"script":5}Done.`, `{"script":5}Done.`, []openai.Tool{bashy}},
		{"ambiguous tools", `{"script":"pwd"}Done.`, `{"script":"pwd"}Done.`, []openai.Tool{bashy, other}},
		{"plain prose", `I can help with that.`, `I can help with that.`, []openai.Tool{bashy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoverToolAnswer(tc.answer, tc.tools); got != tc.want {
				t.Fatalf("recoverToolAnswer = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecoverBareToolArgumentsBecomesStructuredCall(t *testing.T) {
	tool := openai.Tool{Type: "function", Function: openai.ToolFunction{Name: "bashy", Parameters: json.RawMessage(`{"type":"object","properties":{"script":{"type":"string"},"timeout_ms":{"type":"integer"}},"required":["script"]}`)}}
	answer := recoverToolAnswer(`{"script":"pwd; echo x > probe2.txt; ls -la probe2.txt"}Done.`, []openai.Tool{tool})
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}})
	rewritten, ok := openai.TransformChatCompletionJSON(body)
	if !ok {
		t.Fatalf("tool call was not extracted: %s", body)
	}
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []openai.ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rewritten, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) != 1 || response.Choices[0].FinishReason != "tool_calls" || len(response.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected one structured tool call: %s", rewritten)
	}
	call := response.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "bashy" || call.Function.Arguments != `{"script":"pwd; echo x > probe2.txt; ls -la probe2.txt"}` {
		t.Fatalf("unexpected tool call: %+v", call)
	}
}
