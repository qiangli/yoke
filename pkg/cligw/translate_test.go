package cligw

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

func TestRenderPromptTranscriptAndTools(t *testing.T) {
	req := &openai.ChatRequest{
		Messages: []openai.ChatMessage{
			{Role: "system", Content: json.RawMessage(`"be concise"`)},
			{Role: "user", Content: json.RawMessage(`"weather?"`)},
			{Role: "assistant", Content: json.RawMessage(`"checking"`)},
			{Role: "tool", Name: "weather", Content: json.RawMessage(`"sunny"`)},
		},
		Tools: []openai.Tool{{Type: "function", Function: openai.FunctionDefinition{
			Name: "weather", Description: "get weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}}},
		ToolChoice: json.RawMessage(`"auto"`),
	}
	got, err := RenderPrompt(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"System:\nbe concise", "Conversation:\nUser:\nweather?", "Assistant:\nchecking",
		"Tool weather:\nsunny", `"name": "weather"`, "Tool choice:\n\"auto\"", toolInstruction,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestRenderCompletionPromptSeparatesSystemMessages(t *testing.T) {
	got, err := RenderCompletionPrompt(&openai.ChatRequest{Messages: []openai.ChatMessage{
		{Role: "system", Content: json.RawMessage(`"first"`)},
		{Role: "developer", Content: json.RawMessage(`"second"`)},
		{Role: "user", Content: json.RawMessage(`"hello"`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.System != "first\n\nsecond" || strings.Contains(got.Prompt, "first") || got.Prompt != "Conversation:\nUser:\nhello" {
		t.Fatalf("completion prompt = %+v", got)
	}
}

func TestRenderPromptRejectsImage(t *testing.T) {
	_, err := RenderPrompt(&openai.ChatRequest{Messages: []openai.ChatMessage{{
		Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]`),
	}}})
	if !errors.Is(err, ErrUnsupportedContent) {
		t.Fatalf("error = %v, want ErrUnsupportedContent", err)
	}
}

func TestCodexSeatIsFramedOnlyWithCallerTools(t *testing.T) {
	withTools := CompletionPrompt{Prompt: "User: make the file", CallerTools: true}
	plain := CompletionPrompt{Prompt: "User: hi"}
	if !completionFramed("muse", plain) || !completionFramed("codex", withTools) {
		t.Fatal("muse always, and codex with caller tools, must be framed")
	}
	if completionFramed("codex", plain) || completionFramed("claude", withTools) {
		t.Fatal("codex without caller tools and other seats keep their prompt")
	}
	got := framedPrompt("codex", withTools)
	if !strings.Contains(got, "Your local Codex tools remain disabled") || !strings.Contains(got, `{"tool_calls":[`) || !strings.Contains(got, "User: make the file") {
		t.Fatalf("codex frame = %q", got)
	}
	if !strings.Contains(framedPrompt("muse", plain), "Your local Muse tools remain disabled") {
		t.Fatal("muse frame text changed")
	}
}
