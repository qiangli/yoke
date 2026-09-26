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
