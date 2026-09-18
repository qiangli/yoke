package weave

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	weaveStreamJSONHintLimit = 100
	weaveStreamJSONTextLimit = 200
)

type weaveStreamJSONLogWriter struct {
	w   io.Writer
	buf []byte
}

func newWeaveStreamJSONLogWriter(w io.Writer) *weaveStreamJSONLogWriter {
	return &weaveStreamJSONLogWriter{w: w}
}

func (w *weaveStreamJSONLogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), w.buf[:i+1]...)
		w.buf = w.buf[i+1:]
		if err := w.writeLine(line); err != nil {
			return 0, err
		}
	}
	if len(w.buf) > 0 && !weaveStreamJSONMaybeObject(w.buf) {
		if _, err := w.w.Write(w.buf); err != nil {
			return 0, err
		}
		w.buf = nil
	}
	return len(p), nil
}

func (w *weaveStreamJSONLogWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	line := append([]byte(nil), w.buf...)
	w.buf = nil
	return w.writeLine(line)
}

func (w *weaveStreamJSONLogWriter) writeLine(line []byte) error {
	text := string(line)
	body := strings.TrimSuffix(text, "\n")
	body = strings.TrimSuffix(body, "\r")
	summary, ok := weaveDistillStreamJSONLine(body)
	if !ok {
		_, err := w.w.Write(line)
		return err
	}
	if summary == "" {
		return nil
	}
	_, err := io.WriteString(w.w, summary+"\n")
	return err
}

type weaveStreamJSONEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Content json.RawMessage `json:"content"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	CostUSD         float64 `json:"total_cost_usd"`
	DurationMS      int64   `json:"duration_ms"`
	NumTurns        int64   `json:"num_turns"`
	IsError         bool    `json:"is_error"`
	Result          string  `json:"result"`
	UsageLimit      string  `json:"usage_limit"`
	TotalCostUSDAlt float64 `json:"cost_usd"`
	ThreadID        string  `json:"thread_id"`
	Item            struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		Status           string `json:"status"`
		ExitCode         *int   `json:"exit_code"`
	} `json:"item"`
	Usage struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Part struct {
		Type   string  `json:"type"`
		Text   string  `json:"text"`
		Tool   string  `json:"tool"`
		Reason string  `json:"reason"`
		Cost   float64 `json:"cost"`
		Tokens struct {
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
		} `json:"tokens"`
		State struct {
			Status string         `json:"status"`
			Input  map[string]any `json:"input"`
			Title  string         `json:"title"`
		} `json:"state"`
	} `json:"part"`
}

type weaveStreamJSONContent struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   map[string]any  `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

func weaveDistillStreamJSONLine(line string) (string, bool) {
	var event weaveStreamJSONEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &event); err != nil {
		return "", false
	}

	switch event.Type {
	case "assistant":
		return weaveDistillAssistantStreamJSON(event)
	case "user":
		return weaveDistillUserStreamJSON(event)
	case "system":
		return "", true
	case "result":
		return weaveDistillResultStreamJSON(event), true
	case "thread.started":
		return "[thread started]", true
	case "turn.started":
		return "[turn started]", true
	case "item.started":
		return weaveDistillCodexItem(event, true), true
	case "item.completed":
		return weaveDistillCodexItem(event, false), true
	case "turn.completed":
		return weaveDistillCodexTurnCompleted(event), true
	case "turn.failed":
		message := weaveTruncateLogText(strings.TrimSpace(event.Error.Message), weaveStreamJSONHintLimit)
		if message == "" {
			return "[turn failed]", true
		}
		return "[turn failed] " + message, true
	case "error":
		message := weaveTruncateLogText(strings.TrimSpace(event.Error.Message), weaveStreamJSONTextLimit)
		if message == "" {
			return "[error]", true
		}
		return "[error] " + message, true
	case "step_start":
		return "[step started]", true
	case "tool_use":
		return weaveDistillOpenCodeTool(event), true
	case "text":
		return weaveTruncateLogText(strings.TrimSpace(event.Part.Text), weaveStreamJSONTextLimit), true
	case "step_finish":
		return weaveDistillOpenCodeStepFinish(event), true
	default:
		return "", false
	}
}

func weaveDistillOpenCodeTool(event weaveStreamJSONEvent) string {
	name := strings.TrimSpace(event.Part.Tool)
	if name == "" {
		name = "tool"
	}
	line := "-> " + name
	hint := strings.TrimSpace(event.Part.State.Title)
	if hint == "" {
		hint = weaveStreamJSONToolHint(event.Part.State.Input)
	}
	if hint != "" {
		line += " " + weaveTruncateLogText(hint, weaveStreamJSONHintLimit)
	}
	if status := strings.TrimSpace(event.Part.State.Status); status != "" {
		line += " [" + status + "]"
	}
	return line
}

func weaveDistillOpenCodeStepFinish(event weaveStreamJSONEvent) string {
	parts := []string{"step finished"}
	if reason := strings.TrimSpace(event.Part.Reason); reason != "" {
		parts = append(parts, "reason="+reason)
	}
	if event.Part.Tokens.Input > 0 {
		parts = append(parts, fmt.Sprintf("input=%d", event.Part.Tokens.Input))
	}
	if event.Part.Tokens.Output > 0 {
		parts = append(parts, fmt.Sprintf("output=%d", event.Part.Tokens.Output))
	}
	if event.Part.Cost > 0 {
		parts = append(parts, fmt.Sprintf("cost=$%.4f", event.Part.Cost))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func weaveDistillCodexItem(event weaveStreamJSONEvent, started bool) string {
	typ := strings.TrimSpace(event.Item.Type)
	if typ == "" {
		typ = "item"
	}
	if typ == "reasoning" {
		return ""
	}
	if typ == "agent_message" {
		if started {
			return ""
		}
		return weaveTruncateLogText(strings.TrimSpace(event.Item.Text), weaveStreamJSONTextLimit)
	}
	if started {
		line := "-> " + strings.ReplaceAll(typ, "_", " ")
		if command := weaveTruncateLogText(strings.TrimSpace(event.Item.Command), weaveStreamJSONHintLimit); command != "" {
			line += " " + command
		}
		return line
	}
	status := strings.TrimSpace(event.Item.Status)
	if event.Item.ExitCode != nil {
		if *event.Item.ExitCode == 0 {
			status = "ok"
		} else {
			status = fmt.Sprintf("exit=%d", *event.Item.ExitCode)
		}
	}
	if status == "" {
		status = "done"
	}
	line := "   " + status
	if output := strings.TrimSpace(event.Item.AggregatedOutput); output != "" {
		line += ": " + weaveTruncateLogText(firstLogLine(output), weaveStreamJSONHintLimit)
	}
	return line
}

func weaveDistillCodexTurnCompleted(event weaveStreamJSONEvent) string {
	parts := []string{"turn completed"}
	if event.Usage.InputTokens > 0 {
		parts = append(parts, fmt.Sprintf("input=%d", event.Usage.InputTokens))
	}
	if event.Usage.CachedInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("cached=%d", event.Usage.CachedInputTokens))
	}
	if event.Usage.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("output=%d", event.Usage.OutputTokens))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func weaveStreamJSONMaybeObject(p []byte) bool {
	trimmed := bytes.TrimLeft(p, " \t\r")
	return len(trimmed) == 0 || trimmed[0] == '{'
}

func weaveDistillAssistantStreamJSON(event weaveStreamJSONEvent) (string, bool) {
	var out []string
	for _, c := range weaveStreamJSONContentItems(event) {
		switch c.Type {
		case "tool_use":
			name := strings.TrimSpace(c.Name)
			if name == "" {
				name = "tool"
			}
			line := "-> " + name
			if hint := weaveStreamJSONToolHint(c.Input); hint != "" {
				line += " " + weaveTruncateLogText(hint, weaveStreamJSONHintLimit)
			}
			out = append(out, line)
		case "text":
			if text := weaveTruncateLogText(strings.TrimSpace(c.Text), weaveStreamJSONTextLimit); text != "" {
				out = append(out, text)
			}
		case "thinking":
			// Thinking carries large signatures and token noise; omit it from the human log.
		}
	}
	return strings.Join(out, "\n"), true
}

func weaveDistillUserStreamJSON(event weaveStreamJSONEvent) (string, bool) {
	var out []string
	for _, c := range weaveStreamJSONContentItems(event) {
		if c.Type != "tool_result" {
			continue
		}
		status := "ok"
		if c.IsError || event.IsError {
			status = "err"
		}
		if text := weaveStreamJSONToolResultText(c); text != "" {
			out = append(out, fmt.Sprintf("   %s: %s", status, weaveTruncateLogText(firstLogLine(text), weaveStreamJSONHintLimit)))
		} else {
			out = append(out, fmt.Sprintf("   %s:", status))
		}
	}
	return strings.Join(out, "\n"), true
}

func weaveDistillResultStreamJSON(event weaveStreamJSONEvent) string {
	var parts []string
	label := "result"
	if event.Subtype != "" {
		label += " " + event.Subtype
	}
	parts = append(parts, label)
	if event.NumTurns > 0 {
		parts = append(parts, fmt.Sprintf("turns=%d", event.NumTurns))
	}
	if event.DurationMS > 0 {
		parts = append(parts, fmt.Sprintf("duration=%ds", event.DurationMS/1000))
	}
	cost := event.CostUSD
	if cost == 0 {
		cost = event.TotalCostUSDAlt
	}
	if cost > 0 {
		parts = append(parts, fmt.Sprintf("cost=$%.4f", cost))
	}
	if event.UsageLimit != "" {
		parts = append(parts, "usage_limit="+event.UsageLimit)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func weaveStreamJSONContentItems(event weaveStreamJSONEvent) []weaveStreamJSONContent {
	raw := event.Content
	if len(raw) == 0 || string(raw) == "null" {
		raw = event.Message.Content
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []weaveStreamJSONContent
	if err := json.Unmarshal(raw, &items); err == nil {
		return items
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return []weaveStreamJSONContent{{Type: "text", Text: s}}
	}
	return nil
}

func weaveStreamJSONToolHint(input map[string]any) string {
	for _, key := range []string{"description", "file_path", "command", "pattern"} {
		if v, ok := input[key]; ok {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

func weaveStreamJSONToolResultText(c weaveStreamJSONContent) string {
	if len(c.Content) == 0 || string(c.Content) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(c.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(c.Content, &parts); err == nil {
		var out []string
		for _, part := range parts {
			if strings.TrimSpace(part.Text) != "" {
				out = append(out, part.Text)
			}
		}
		return strings.Join(out, "\n")
	}
	return strings.TrimSpace(string(c.Content))
}

func firstLogLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.TrimSuffix(s, "\r"))
}

func weaveTruncateLogText(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if limit <= 0 || len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return s[:limit]
	}
	return strings.TrimSpace(s[:limit-3]) + "..."
}
