package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

// ToOpenAI converts an Anthropic Messages request into the canonical request
// served by the gateway's shared inference spine.
func ToOpenAI(req *MessagesRequest) (*openai.ChatRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	out := &openai.ChatRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      req.Stream,
	}
	if len(req.StopSequences) == 1 {
		out.Stop = req.StopSequences[0]
	} else if len(req.StopSequences) > 1 {
		out.Stop = req.StopSequences
	}
	if len(req.System.Blocks) > 0 {
		content, err := openAIContent(req.System.Blocks)
		if err != nil {
			return nil, fmt.Errorf("system: %w", err)
		}
		out.Messages = append(out.Messages, openai.ChatMessage{Role: "system", Content: content})
	}
	for i, message := range req.Messages {
		converted, err := convertMessage(message)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, converted...)
	}
	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, openai.Tool{Type: "function", Function: openai.ToolFunction{
			Name: tool.Name, Description: tool.Description, Parameters: cloneRaw(tool.InputSchema),
		}})
	}
	if req.ToolChoice != nil {
		if req.ToolChoice.DisableParallelToolUse {
			parallel := false
			out.ParallelToolCalls = &parallel
		}
		switch req.ToolChoice.Type {
		case "auto", "none":
			out.ToolChoice = req.ToolChoice.Type
		case "any":
			out.ToolChoice = "required"
		case "tool":
			out.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": req.ToolChoice.Name}}
		default:
			return nil, fmt.Errorf("unsupported tool_choice type %q", req.ToolChoice.Type)
		}
	}
	return out, nil
}

func convertMessage(message Message) ([]openai.ChatMessage, error) {
	if message.Role != "user" && message.Role != "assistant" {
		return nil, fmt.Errorf("unsupported role %q", message.Role)
	}
	if message.Role == "assistant" {
		var text []ContentBlock
		var calls []openai.ToolCall
		for _, block := range message.Content.Blocks {
			switch block.Type {
			case "text":
				text = append(text, block)
			case "tool_use":
				args := strings.TrimSpace(string(block.Input))
				if args == "" || args == "null" {
					args = "{}"
				}
				calls = append(calls, openai.ToolCall{ID: block.ID, Type: "function", Function: openai.ToolFunction{Name: block.Name, Arguments: args}})
			default:
				return nil, fmt.Errorf("unsupported content block type %q", block.Type)
			}
		}
		content := json.RawMessage(`""`)
		if len(text) > 0 {
			var err error
			content, err = openAIContent(text)
			if err != nil {
				return nil, err
			}
		}
		return []openai.ChatMessage{{Role: "assistant", Content: content, ToolCalls: calls}}, nil
	}
	var result []openai.ChatMessage
	var ordinary []ContentBlock
	flush := func() error {
		if len(ordinary) == 0 {
			return nil
		}
		content, err := openAIContent(ordinary)
		if err != nil {
			return err
		}
		result = append(result, openai.ChatMessage{Role: message.Role, Content: content})
		ordinary = nil
		return nil
	}
	for _, block := range message.Content.Blocks {
		switch block.Type {
		case "text":
			ordinary = append(ordinary, block)
		case "tool_result":
			if err := flush(); err != nil {
				return nil, err
			}
			content, err := toolResultText(block.Content)
			if err != nil {
				return nil, err
			}
			result = append(result, openai.ChatMessage{Role: "tool", ToolCallID: block.ToolUseID, Content: mustJSON(content)})
		default:
			return nil, fmt.Errorf("unsupported content block type %q", block.Type)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return result, nil
}

func openAIContent(blocks []ContentBlock) (json.RawMessage, error) {
	parts := make([]map[string]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			return nil, fmt.Errorf("unsupported content block type %q", block.Type)
		}
		parts = append(parts, map[string]string{"type": "text", "text": block.Text})
	}
	if len(parts) == 1 {
		return mustJSON(parts[0]["text"]), nil
	}
	data, err := json.Marshal(parts)
	return data, err
}

func toolResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("tool_result content: %w", err)
	}
	var b strings.Builder
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fmt.Errorf("unsupported tool_result block type %q", block.Type)
		}
		b.WriteString(block.Text)
	}
	return b.String(), nil
}

// FromOpenAI converts a completed OpenAI response into a Messages response.
func FromOpenAI(comp *openai.ChatCompletion) *MessagesResponse {
	resp := &MessagesResponse{Type: "message", Role: "assistant", Content: []ContentBlock{}}
	if comp == nil {
		return resp
	}
	resp.ID, resp.Model = comp.ID, comp.Model
	resp.Usage = Usage{InputTokens: comp.Usage.PromptTokens, OutputTokens: comp.Usage.CompletionTokens}
	if len(comp.Choices) == 0 {
		return resp
	}
	choice := comp.Choices[0]
	if choice.FinishReason != nil {
		reason := FromOpenAIStop(*choice.FinishReason)
		resp.StopReason = &reason
	}
	if choice.Message == nil {
		return resp
	}
	if choice.Message.Content != "" {
		resp.Content = append(resp.Content, ContentBlock{Type: "text", Text: choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		input := json.RawMessage(call.Function.Arguments)
		if !json.Valid(input) {
			input = json.RawMessage(`{}`)
		}
		resp.Content = append(resp.Content, ContentBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: input})
	}
	return resp
}

// FromOpenAIStop is the inverse of openai.MapAnthropicStop for the distinct
// finish reasons. OpenAI's generic stop maps to Anthropic end_turn because a
// concrete stop sequence is unavailable in the completion response.
func FromOpenAIStop(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func mustJSON(v any) json.RawMessage             { data, _ := json.Marshal(v); return data }
func cloneRaw(v json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), v...) }
