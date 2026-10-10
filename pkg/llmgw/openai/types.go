package openai

import (
	"encoding/json"
	"strings"
)

// ChatRequest is the request subset used by the provider translators.
type ChatRequest struct {
	Model               string        `json:"model"`
	Messages            []ChatMessage `json:"messages"`
	MaxTokens           int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	Stop                any           `json:"stop,omitempty"`
	Tools               []Tool        `json:"tools,omitempty"`
	ToolChoice          any           `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool         `json:"parallel_tool_calls,omitempty"`
	Stream              bool          `json:"stream,omitempty"`
	ReasoningEffort     string        `json:"reasoning_effort,omitempty"`
}

// ChatMessage is an input message. Content may be a string or an array of
// typed content parts.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Tool is an OpenAI function tool declaration.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is shared by tool declarations and calls. Parameters is used
// by declarations; Arguments is used by calls.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

// FunctionDefinition is the declaration view of ToolFunction.
type FunctionDefinition = ToolFunction

// ToolCall is a complete assistant tool call.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolCallDelta is the incremental tool-call shape in a streaming chunk.
type ToolCallDelta struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function ToolFunction `json:"function"`
}

// ChatCompletion is an OpenAI-compatible chat completion or stream chunk.
type ChatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

// ChatChoice is one completion choice.
type ChatChoice struct {
	Index        int                  `json:"index"`
	Message      *ChatResponseMessage `json:"message,omitempty"`
	Delta        *ChatResponseMessage `json:"delta,omitempty"`
	FinishReason *string              `json:"finish_reason"`
}

// ChatResponseMessage is an assistant response or streaming delta.
type ChatResponseMessage struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content"`
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

// ChatUsage is retained for source compatibility. New code should use Usage.
type ChatUsage = Usage

// EffectiveMaxTokens returns the request's token ceiling, defaulting to 4096.
func (r *ChatRequest) EffectiveMaxTokens() int {
	if r.MaxTokens > 0 {
		return r.MaxTokens
	}
	if r.MaxCompletionTokens > 0 {
		return r.MaxCompletionTokens
	}
	return 4096
}

// FlattenContent reduces string or typed-part message content to plain text.
// Parts without text are ignored.
func FlattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}
