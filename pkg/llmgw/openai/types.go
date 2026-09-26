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
	Stream              bool          `json:"stream,omitempty"`
}

// ChatMessage is an input message. Content may be a string or an array of
// typed content parts.
type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
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
