package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

func TestToOpenAI_SystemToolsAndResults(t *testing.T) {
	var req MessagesRequest
	body := `{
      "model":"L4","system":[{"type":"text","text":"be concise"},{"type":"text","text":"use tools"}],
      "messages":[
        {"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"Paris"}}]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"sunny"}]}]}
      ],
      "tools":[{"name":"weather","description":"forecast","input_schema":{"type":"object"}}],
      "tool_choice":{"type":"tool","name":"weather"},"max_tokens":128,"stop_sequences":["END"]
    }`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	got, err := ToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages=%d, want 3: %#v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "system" || openai.FlattenContent(got.Messages[0].Content) != "be conciseuse tools" {
		t.Errorf("system=%+v", got.Messages[0])
	}
	call := got.Messages[1].ToolCalls
	if len(call) != 1 || call[0].ID != "toolu_1" || call[0].Function.Name != "weather" || call[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool call=%+v", call)
	}
	if openai.FlattenContent(got.Messages[1].Content) != "checking" {
		t.Errorf("assistant content=%s", got.Messages[1].Content)
	}
	if got.Messages[2].Role != "tool" || got.Messages[2].ToolCallID != "toolu_1" || openai.FlattenContent(got.Messages[2].Content) != "sunny" {
		t.Errorf("tool result=%+v", got.Messages[2])
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "weather" {
		t.Errorf("tools=%+v", got.Tools)
	}
	if got.Stop != "END" {
		t.Errorf("stop=%#v", got.Stop)
	}
}

func TestFromOpenAI_ContentToolsUsageAndStops(t *testing.T) {
	for openAI, want := range map[string]string{"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "content_filter": "end_turn"} {
		reason := openAI
		comp := &openai.ChatCompletion{ID: "chatcmpl-1", Model: "L4", Usage: openai.Usage{PromptTokens: 7, CompletionTokens: 5}, Choices: []openai.ChatChoice{{
			Message: &openai.ChatResponseMessage{Content: "I'll check", ToolCalls: []openai.ToolCallDelta{{
				ID: "call_1", Type: "function", Function: openai.ToolFunction{Name: "weather", Arguments: `{"city":"Paris"}`},
			}}},
			FinishReason: &reason,
		}}}
		got := FromOpenAI(comp)
		if got.StopReason == nil || *got.StopReason != want {
			t.Errorf("%s stop=%v, want %s", openAI, got.StopReason, want)
		}
		if got.Usage != (Usage{InputTokens: 7, OutputTokens: 5}) {
			t.Errorf("usage=%+v", got.Usage)
		}
		if len(got.Content) != 2 || got.Content[0].Text != "I'll check" || got.Content[1].Type != "tool_use" || string(got.Content[1].Input) != `{"city":"Paris"}` {
			t.Errorf("content=%+v", got.Content)
		}
	}
}

func TestConvertStream_TextAndToolUse(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"L4","choices":[{"index":0,"delta":{"role":"assistant","content":"hi "},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"L4","choices":[{"index":0,"delta":{"content":"there","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"L4","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`,
		`data: [DONE]`, "",
	}, "\n\n")
	var out strings.Builder
	if err := ConvertStream(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`event: message_start`, `"type":"text_delta","text":"hi "`, `"type":"input_json_delta","partial_json":"{\"city\":"`, `"stop_reason":"tool_use"`, `"output_tokens":5`, `event: message_stop`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stream missing %q:\n%s", want, out.String())
		}
	}
}
