// Package anthropic translates the Anthropic Messages wire protocol to and
// from the gateway's OpenAI-compatible internal representation.
package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MessagesRequest is the supported Anthropic Messages request surface.
type MessagesRequest struct {
	Model         string      `json:"model"`
	System        Content     `json:"system,omitempty"`
	Messages      []Message   `json:"messages"`
	Tools         []Tool      `json:"tools,omitempty"`
	ToolChoice    *ToolChoice `json:"tool_choice,omitempty"`
	MaxTokens     int         `json:"max_tokens"`
	StopSequences []string    `json:"stop_sequences,omitempty"`
	Temperature   *float64    `json:"temperature,omitempty"`
	Stream        bool        `json:"stream,omitempty"`
}

type Message struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// Content is the string-or-block-array union used by system and message
// content. Decoding a string produces one text block; marshaling uses the
// compact string form only when String is true.
type Content struct {
	Blocks []ContentBlock
	String bool
}

func TextContent(text string) Content {
	return Content{Blocks: []ContentBlock{{Type: "text", Text: text}}, String: true}
}

func (c *Content) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || len(data) == 0 {
		*c = Content{}
		return nil
	}
	var text string
	if data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*c = TextContent(text)
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return fmt.Errorf("content must be a string or block array: %w", err)
	}
	*c = Content{Blocks: blocks}
	return nil
}

func (c Content) MarshalJSON() ([]byte, error) {
	if c.String && len(c.Blocks) == 1 && c.Blocks[0].Type == "text" {
		return json.Marshal(c.Blocks[0].Text)
	}
	if c.Blocks == nil {
		return []byte("null"), nil
	}
	return json.Marshal(c.Blocks)
}

func (c Content) IsZero() bool { return len(c.Blocks) == 0 }

// ContentBlock covers request and response text, tool_use, and tool_result
// blocks. Content on a tool_result remains raw because the protocol permits
// either a string or a nested content-block array.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type ErrorResponse struct {
	Type  string    `json:"type"`
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Streaming event wire types.
type MessageStartEvent struct {
	Type    string           `json:"type"`
	Message MessagesResponse `json:"message"`
}

type ContentBlockStartEvent struct {
	Type         string       `json:"type"`
	Index        int          `json:"index"`
	ContentBlock ContentBlock `json:"content_block"`
}

type ContentBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta Delta  `json:"delta"`
}

type Delta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type ContentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type MessageDeltaEvent struct {
	Type  string            `json:"type"`
	Delta MessageDelta      `json:"delta"`
	Usage MessageDeltaUsage `json:"usage"`
}

type MessageDelta struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type MessageDeltaUsage struct {
	OutputTokens int `json:"output_tokens"`
}

type MessageStopEvent struct {
	Type string `json:"type"`
}
type PingEvent struct {
	Type string `json:"type"`
}
type ErrorEvent struct {
	Type  string    `json:"type"`
	Error ErrorBody `json:"error"`
}
