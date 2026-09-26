package cligw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

const toolInstruction = `Tool calling instructions:
Respond with plain text, or, when calling tools, with exactly one JSON object and no markdown or additional text:
{"tool_calls":[{"name":"<tool name>","arguments":{}}]}
Use only listed tool names. Each arguments object must satisfy that tool's JSON schema.`

// ErrUnsupportedContent is returned when a message contains an image or any
// other content part that a text-only CLI worker cannot faithfully consume.
var ErrUnsupportedContent = errors.New("cligw: images and non-text message content are not supported")

// RenderPrompt turns an OpenAI chat request into the deterministic transcript
// consumed by one-shot CLI workers.
func RenderPrompt(req *openai.ChatRequest) (string, error) {
	if req == nil {
		return "", errors.New("cligw: nil chat request")
	}

	var systems, turns []string
	for _, message := range req.Messages {
		content, err := textContent(message.Content)
		if err != nil {
			return "", fmt.Errorf("cligw: %s message: %w", message.Role, err)
		}
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "developer":
			systems = append(systems, content)
		case "user":
			turns = append(turns, "User:\n"+content)
		case "assistant":
			turns = append(turns, "Assistant:\n"+content)
		case "tool":
			label := "Tool"
			if message.Name != "" {
				label += " " + message.Name
			} else if message.ToolCallID != "" {
				label += " " + message.ToolCallID
			}
			turns = append(turns, label+":\n"+content)
		default:
			return "", fmt.Errorf("cligw: unsupported message role %q", message.Role)
		}
	}

	sections := make([]string, 0, 4)
	if len(systems) > 0 {
		sections = append(sections, "System:\n"+strings.Join(systems, "\n\n"))
	}
	if len(turns) > 0 {
		sections = append(sections, "Conversation:\n"+strings.Join(turns, "\n\n"))
	}
	if len(req.Tools) > 0 || len(bytes.TrimSpace(req.ToolChoice)) > 0 {
		block, err := renderTools(req.Tools, req.ToolChoice)
		if err != nil {
			return "", err
		}
		sections = append(sections, block)
	}
	return strings.Join(sections, "\n\n"), nil
}

func textContent(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", ErrUnsupportedContent
	}
	var out strings.Builder
	for _, part := range parts {
		if part.Type != "text" && part.Type != "input_text" {
			return "", ErrUnsupportedContent
		}
		out.WriteString(part.Text)
	}
	return out.String(), nil
}

func renderTools(tools []openai.Tool, choice json.RawMessage) (string, error) {
	schemas, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		return "", fmt.Errorf("cligw: encode tool schemas: %w", err)
	}
	var out strings.Builder
	out.WriteString("Tools:\n")
	out.Write(schemas)
	if len(bytes.TrimSpace(choice)) > 0 {
		if !json.Valid(choice) {
			return "", errors.New("cligw: invalid tool_choice")
		}
		out.WriteString("\nTool choice:\n")
		out.Write(bytes.TrimSpace(choice))
	}
	out.WriteString("\n\n")
	out.WriteString(toolInstruction)
	return out.String(), nil
}
