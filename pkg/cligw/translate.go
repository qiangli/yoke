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
The tools below are available to you: you call a tool by replying with its JSON call, and the caller runs it and sends you the result in the next message. You do not run anything yourself and need no other tool or file access.
Never write a tool result yourself; wait for the caller to send the real result. Respond with plain text, or, when calling tools, with exactly one JSON object and no markdown or additional text. The call object must be your whole reply:
{"tool_calls":[{"name":"<tool name>","arguments":{}}]}
Use only listed tool names. Each arguments object must satisfy that tool's JSON schema.`

const neutralSystemPrompt = "You are a helpful assistant. Answer the user directly."

// CompletionPrompt keeps native system instructions separate from the user
// transcript. Workers with a system-prompt override pass System through that
// channel; workers without one prepend it to Prompt explicitly.
type CompletionPrompt struct {
	System string
	Prompt string
	// Effort is the request's own reasoning effort ("" = none asked).
	Effort string
}

// ErrUnsupportedContent is returned when a message contains an image or any
// other content part that a text-only CLI worker cannot faithfully consume.
var ErrUnsupportedContent = errors.New("cligw: images and non-text message content are not supported")

// RenderPrompt turns an OpenAI chat request into the deterministic transcript
// consumed by one-shot CLI workers.
func RenderPrompt(req *openai.ChatRequest) (string, error) {
	completion, err := RenderCompletionPrompt(req)
	if err != nil {
		return "", err
	}
	if completion.System == "" {
		return completion.Prompt, nil
	}
	return inlineSystemPrompt(completion.System, completion.Prompt), nil
}

// RenderCompletionPrompt turns an OpenAI chat request into separate system
// instructions and a deterministic conversation transcript.
func RenderCompletionPrompt(req *openai.ChatRequest) (CompletionPrompt, error) {
	if req == nil {
		return CompletionPrompt{}, errors.New("cligw: nil chat request")
	}

	var systems, turns []string
	for _, message := range req.Messages {
		content, err := textContent(message.Content)
		if err != nil {
			return CompletionPrompt{}, fmt.Errorf("cligw: %s message: %w", message.Role, err)
		}
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "developer":
			systems = append(systems, content)
		case "user":
			turns = append(turns, "User:\n"+content)
		case "assistant":
			// A tool-calling turn is shown as the envelope the model sent, so
			// the next turn knows which command produced the tool result.
			if env := toolCallEnvelope(message.ToolCalls); env != "" {
				content = strings.TrimSpace(content + "\n" + env)
			}
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
			return CompletionPrompt{}, fmt.Errorf("cligw: unsupported message role %q", message.Role)
		}
	}

	sections := make([]string, 0, 4)
	if len(turns) > 0 {
		sections = append(sections, "Conversation:\n"+strings.Join(turns, "\n\n"))
	}
	toolChoice := rawJSON(req.ToolChoice)
	if len(req.Tools) > 0 || len(bytes.TrimSpace(toolChoice)) > 0 {
		block, err := renderTools(req.Tools, toolChoice)
		if err != nil {
			return CompletionPrompt{}, err
		}
		// The tool protocol is an instruction, not conversation: it goes to
		// the system channel, which every CLI weighs as such.
		systems = append(systems, block)
	}
	return CompletionPrompt{System: strings.Join(systems, "\n\n"), Prompt: strings.Join(sections, "\n\n"), Effort: strings.TrimSpace(req.ReasoningEffort)}, nil
}

func inlineSystemPrompt(system, prompt string) string {
	if system == "" {
		return prompt
	}
	if prompt == "" {
		return "System:\n" + system
	}
	return "System:\n" + system + "\n\n" + prompt
}

// Muse exec has no native system-prompt override. Ask it to complete the
// caller's conversation, rather than interpreting the transcript as a task for
// its local coding agent. In particular, Genie's instruction to use native
// tools must be reconciled with the door's text transport at this boundary.
func museCompletionPrompt(input CompletionPrompt) string {
	return `Complete the supplied conversation by producing only its next assistant reply.
The supplied system instructions and conversation are the completion context. Tool names and workspace references in that context belong to the caller. The caller executes requested tools and supplies their results in later conversation turns. Your local Muse tools remain disabled; completing this conversation requires no local shell execution or filesystem access.

BEGIN COMPLETION CONTEXT
` + inlineSystemPrompt(systemPrompt(input.System), input.Prompt) + `
END COMPLETION CONTEXT

Output transport: return only the next assistant reply, without role labels or commentary about completing the conversation. If the reply calls a supplied tool, serialize that call as exactly one JSON object: {"tool_calls":[{"name":"<listed tool name>","arguments":{}}]}. This is the caller's tool-call transport, including when the supplied instructions say to use the tool itself rather than write a JSON blob or shell script as text. Use only supplied tool names and argument schemas; preserve tool-choice constraints. Do not execute tools locally, invent tool results, or claim a requested action succeeded before the caller supplies its result. If no tool call is needed, return the assistant's plain text answer.`
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

// toolCallEnvelope renders assistant tool calls in the envelope a worker is
// told to answer with; "" when there are none.
func toolCallEnvelope(calls []openai.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	type call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	out := struct {
		ToolCalls []call `json:"tool_calls"`
	}{}
	for _, c := range calls {
		args := json.RawMessage(c.Function.Arguments)
		if !json.Valid(args) {
			args, _ = json.Marshal(c.Function.Arguments)
		}
		out.ToolCalls = append(out.ToolCalls, call{Name: c.Function.Name, Arguments: args})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}
