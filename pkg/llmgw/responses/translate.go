// Package responses translates the stateless OpenAI Responses dialect to the
// chat completion dialect used by the model door.
package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

type Request struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions,omitempty"`
	Tools        []toolSpec      `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Reasoning       struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
	Stream             bool   `json:"stream,omitempty"`
	Store              *bool  `json:"store,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

type toolSpec struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Tools       []toolSpec      `json:"tools,omitempty"` // type "namespace"
}

// customInputSchema is the function schema a freeform custom tool is offered
// under: its raw text input travels as the one string argument.
var customInputSchema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"the tool's raw text input, verbatim (not JSON-encoded)"}},"required":["input"]}`)

// flattenTools expands namespace groups (Codex nests its tools in one) and
// keeps the kinds the door can express: function tools as they are, and
// custom (freeform) tools as one-string-argument functions. Hosted tools
// (web_search, local_shell, ...) cannot run here and are not offered.
func flattenTools(tools []toolSpec) []toolSpec {
	var out []toolSpec
	for _, t := range tools {
		switch t.Type {
		case "namespace":
			out = append(out, flattenTools(t.Tools)...)
		case "function", "custom":
			out = append(out, t)
		}
	}
	return out
}

// CustomToolNames lists the freeform custom tools a Responses request offers,
// so FromChatWithCustom can answer a call to one as a custom_tool_call.
func CustomToolNames(body []byte) map[string]bool {
	var req Request
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	all := req.Tools
	var items []inputItem
	if json.Unmarshal(bytes.TrimSpace(req.Input), &items) == nil {
		for _, item := range items {
			if item.Type == "additional_tools" {
				all = append(all, item.Tools...)
			}
		}
	}
	names := map[string]bool{}
	for _, t := range flattenTools(all) {
		if t.Type == "custom" && t.Name != "" {
			names[t.Name] = true
		}
	}
	return names
}

type inputItem struct {
	Tools     []toolSpec      `json:"tools"`
	Input     string          `json:"input"`
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// ToChat rejects stateful requests because this door has no response store.
func ToChat(body []byte) (*openai.ChatRequest, bool, error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, fmt.Errorf("invalid Responses request: %w", err)
	}
	if req.PreviousResponseID != "" {
		return nil, false, fmt.Errorf("previous_response_id requires server-side response state, which this door does not support")
	}
	if req.Store != nil && *req.Store {
		return nil, false, fmt.Errorf("store=true requires server-side response state, which this door does not support")
	}
	if req.Model == "" {
		return nil, false, fmt.Errorf("model is required")
	}
	out := &openai.ChatRequest{Model: req.Model, MaxCompletionTokens: req.MaxOutputTokens, ReasoningEffort: req.Reasoning.Effort}
	if req.Instructions != "" {
		out.Messages = append(out.Messages, openai.ChatMessage{Role: "system", Content: mustJSON(req.Instructions)})
	}
	input := bytes.TrimSpace(req.Input)
	if len(input) == 0 || bytes.Equal(input, []byte("null")) {
		return nil, false, fmt.Errorf("input is required")
	}
	if input[0] == '"' {
		var prompt string
		if err := json.Unmarshal(input, &prompt); err != nil {
			return nil, false, err
		}
		out.Messages = append(out.Messages, openai.ChatMessage{Role: "user", Content: mustJSON(prompt)})
	} else {
		var items []inputItem
		if err := json.Unmarshal(input, &items); err != nil {
			return nil, false, fmt.Errorf("input items: %w", err)
		}
		for i, item := range items {
			switch item.Type {
			case "", "message":
				if item.Role != "user" && item.Role != "assistant" && item.Role != "system" && item.Role != "developer" {
					return nil, false, fmt.Errorf("input[%d]: unsupported role %q", i, item.Role)
				}
				content, err := messageContent(item.Content)
				if err != nil {
					return nil, false, fmt.Errorf("input[%d]: %w", i, err)
				}
				role := item.Role
				if role == "developer" {
					role = "system"
				}
				out.Messages = append(out.Messages, openai.ChatMessage{Role: role, Content: content})
			case "additional_tools":
				// Codex declares further tools as an input item; they join the
				// request's tool list under the same rules.
				req.Tools = append(req.Tools, item.Tools...)
			case "custom_tool_call":
				if item.CallID == "" || item.Name == "" {
					return nil, false, fmt.Errorf("input[%d]: custom_tool_call needs call_id and name", i)
				}
				args, _ := json.Marshal(map[string]string{"input": item.Input})
				out.Messages = append(out.Messages, openai.ChatMessage{Role: "assistant", Content: mustJSON(""), ToolCalls: []openai.ToolCall{{ID: item.CallID, Type: "function", Function: openai.ToolFunction{Name: item.Name, Arguments: string(args)}}}})
			case "custom_tool_call_output":
				if item.CallID == "" {
					return nil, false, fmt.Errorf("input[%d]: custom_tool_call_output needs call_id", i)
				}
				content := item.Output
				if len(content) == 0 {
					content = mustJSON("")
				}
				out.Messages = append(out.Messages, openai.ChatMessage{Role: "tool", ToolCallID: item.CallID, Content: content})
			case "function_call":
				if item.CallID == "" || item.Name == "" {
					return nil, false, fmt.Errorf("input[%d]: function_call needs call_id and name", i)
				}
				out.Messages = append(out.Messages, openai.ChatMessage{Role: "assistant", Content: mustJSON(""), ToolCalls: []openai.ToolCall{{ID: item.CallID, Type: "function", Function: openai.ToolFunction{Name: item.Name, Arguments: item.Arguments}}}})
			case "function_call_output":
				if item.CallID == "" {
					return nil, false, fmt.Errorf("input[%d]: function_call_output needs call_id", i)
				}
				content := item.Output
				if len(content) == 0 {
					content = mustJSON("")
				}
				out.Messages = append(out.Messages, openai.ChatMessage{Role: "tool", ToolCallID: item.CallID, Content: content})
			default:
				return nil, false, fmt.Errorf("input[%d]: unsupported item type %q", i, item.Type)
			}
		}
	}
	for i, tool := range flattenTools(req.Tools) {
		if tool.Name == "" {
			return nil, false, fmt.Errorf("tools[%d]: %s tool needs a name", i, tool.Type)
		}
		params := tool.Parameters
		if tool.Type == "custom" {
			params = customInputSchema
		}
		out.Tools = append(out.Tools, openai.Tool{Type: "function", Function: openai.ToolFunction{Name: tool.Name, Description: tool.Description, Parameters: params}})
	}
	if len(req.ToolChoice) != 0 {
		var choice string
		if json.Unmarshal(req.ToolChoice, &choice) == nil {
			if choice != "auto" && choice != "none" && choice != "required" {
				return nil, false, fmt.Errorf("unsupported tool_choice %q", choice)
			}
			out.ToolChoice = choice
		} else {
			var named struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(req.ToolChoice, &named); err != nil || named.Type != "function" || named.Name == "" {
				return nil, false, fmt.Errorf("unsupported tool_choice")
			}
			out.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": named.Name}}
		}
	}
	return out, req.Stream, nil
}

func messageContent(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return mustJSON(""), nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return raw, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("invalid message content: %w", err)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "input_text" && p.Type != "output_text" && p.Type != "text" {
			return nil, fmt.Errorf("unsupported content part %q", p.Type)
		}
		b.WriteString(p.Text)
	}
	return mustJSON(b.String()), nil
}

type OutputItem struct {
	ID        string              `json:"id"`
	Type      string              `json:"type"`
	Status    string              `json:"status,omitempty"`
	Role      string              `json:"role,omitempty"`
	Content   []map[string]string `json:"content,omitempty"`
	CallID    string              `json:"call_id,omitempty"`
	Name      string              `json:"name,omitempty"`
	Arguments string              `json:"arguments,omitempty"`
	Input     string              `json:"input,omitempty"` // custom_tool_call
}
type Response struct {
	ID        string       `json:"id"`
	Object    string       `json:"object"`
	CreatedAt int64        `json:"created_at"`
	Status    string       `json:"status"`
	Model     string       `json:"model"`
	Output    []OutputItem `json:"output"`
	Usage     struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func FromChat(comp *openai.ChatCompletion) (*Response, error) {
	return FromChatWithCustom(comp, nil)
}

// FromChatWithCustom is FromChat for a request that offered custom tools: a
// call to one of those names is answered as a custom_tool_call whose input is
// the call's "input" argument (or the raw arguments if they are not that shape).
func FromChatWithCustom(comp *openai.ChatCompletion, custom map[string]bool) (*Response, error) {
	if comp == nil {
		return nil, fmt.Errorf("empty chat completion")
	}
	resp := &Response{ID: comp.ID, Object: "response", CreatedAt: comp.Created, Status: "completed", Model: comp.Model, Output: []OutputItem{}}
	if resp.ID == "" {
		resp.ID = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	if resp.CreatedAt == 0 {
		resp.CreatedAt = time.Now().Unix()
	}
	resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.TotalTokens = comp.Usage.PromptTokens, comp.Usage.CompletionTokens, comp.Usage.TotalTokens
	if resp.Usage.TotalTokens == 0 {
		resp.Usage.TotalTokens = resp.Usage.InputTokens + resp.Usage.OutputTokens
	}
	if len(comp.Choices) == 0 || comp.Choices[0].Message == nil {
		return resp, nil
	}
	msg := comp.Choices[0].Message
	if msg.Content != "" {
		resp.Output = append(resp.Output, OutputItem{ID: resp.ID + "_msg", Type: "message", Status: "completed", Role: "assistant", Content: []map[string]string{{"type": "output_text", "text": msg.Content}}})
	}
	for i, call := range msg.ToolCalls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("%s_call_%d", resp.ID, i)
		}
		if custom[call.Function.Name] {
			var args struct {
				Input *string `json:"input"`
			}
			input := call.Function.Arguments
			if json.Unmarshal([]byte(call.Function.Arguments), &args) == nil && args.Input != nil {
				input = *args.Input
			}
			resp.Output = append(resp.Output, OutputItem{ID: resp.ID + fmt.Sprintf("_ctc_%d", i), Type: "custom_tool_call", Status: "completed", CallID: id, Name: call.Function.Name, Input: input})
			continue
		}
		resp.Output = append(resp.Output, OutputItem{ID: resp.ID + fmt.Sprintf("_fc_%d", i), Type: "function_call", Status: "completed", CallID: id, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	return resp, nil
}

func mustJSON(v any) json.RawMessage { data, _ := json.Marshal(v); return data }
