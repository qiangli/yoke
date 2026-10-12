package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newCaptureCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "capture", Short: "inspect and export captured model turns"}
	var asJSON bool
	ls := &cobra.Command{Use: "ls", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		root, err := captureRoot()
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			entries = nil
			err = nil
		}
		if err != nil {
			return err
		}
		type summary struct {
			Session string    `json:"session"`
			Turns   int       `json:"turns"`
			Episode string    `json:"episode"`
			LastAt  time.Time `json:"last_at"`
		}
		rows := make([]summary, 0)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			turns, err := readCaptureTurns(e.Name())
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			row := summary{Session: e.Name(), Turns: len(turns)}
			if len(turns) > 0 {
				row.Episode = turns[len(turns)-1].Episode
				row.LastAt = turns[len(turns)-1].At
			} else {
				row.Episode = captureEpisode(e.Name())
			}
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Session < rows[j].Session })
		if asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(rows)
		}
		for _, r := range rows {
			fmt.Fprintf(c.OutOrStdout(), "%s\t%d\t%s\t%s\n", r.Session, r.Turns, r.Episode, r.LastAt.Format(time.RFC3339))
		}
		return nil
	}}
	ls.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	show := &cobra.Command{Use: "show SESSION", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		dir, err := captureSessionDir(args[0])
		if err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(dir, "turns.jsonl"))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(c.OutOrStdout(), f)
		return err
	}}
	show.Flags().Bool("json", false, "JSONL passthrough")
	var format, output string
	export := &cobra.Command{Use: "export SESSION", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		if format != "atif" {
			return fmt.Errorf("capture export: unsupported format %q (want atif)", format)
		}
		turns, err := readCaptureTurns(args[0])
		if err != nil {
			return err
		}
		data, err := exportATIF(args[0], turns)
		if err != nil {
			return err
		}
		if output != "" {
			return os.WriteFile(output, append(data, '\n'), 0600)
		}
		_, err = c.OutOrStdout().Write(append(data, '\n'))
		return err
	}}
	export.Flags().StringVar(&format, "format", "atif", "export format (atif only)")
	export.Flags().StringVarP(&output, "output", "o", "", "output file (default stdout)")
	cmd.AddCommand(ls, show, export)
	return cmd
}

// Field names follow Harbor's ATIF specification:
// https://github.com/harbor-framework/harbor/blob/main/rfcs/0001-trajectory-format.md
type atifToolCall struct {
	ToolCallID   string         `json:"tool_call_id"`
	FunctionName string         `json:"function_name"`
	Arguments    map[string]any `json:"arguments"`
}
type atifStep struct {
	StepID    int            `json:"step_id"`
	Timestamp string         `json:"timestamp"`
	Source    string         `json:"source"`
	ModelName string         `json:"model_name,omitempty"`
	Message   string         `json:"message"`
	ToolCalls []atifToolCall `json:"tool_calls,omitempty"`
	Metrics   *captureUsage  `json:"metrics,omitempty"`
}

func exportATIF(session string, turns []captureTurn) ([]byte, error) {
	steps := make([]atifStep, 0, len(turns))
	var prompt, completion int
	model := ""
	for i, t := range turns {
		step := atifStep{StepID: i + 1, Timestamp: t.At.Format(time.RFC3339Nano), Source: "agent", ModelName: t.Model, Message: assistantText(t.Response), ToolCalls: assistantTools(t.Response), Metrics: t.Usage}
		steps = append(steps, step)
		if model == "" {
			model = t.Model
		}
		if t.Usage != nil {
			prompt += t.Usage.PromptTokens
			completion += t.Usage.CompletionTokens
		}
	}
	trajectory := struct {
		SchemaVersion string `json:"schema_version"`
		SessionID     string `json:"session_id"`
		Agent         struct {
			Name      string `json:"name"`
			Version   string `json:"version"`
			ModelName string `json:"model_name,omitempty"`
		} `json:"agent"`
		Steps        []atifStep `json:"steps"`
		FinalMetrics struct {
			TotalPromptTokens     int `json:"total_prompt_tokens"`
			TotalCompletionTokens int `json:"total_completion_tokens"`
			TotalSteps            int `json:"total_steps"`
		} `json:"final_metrics"`
	}{SchemaVersion: "ATIF-v1.7", SessionID: session, Steps: steps}
	trajectory.Agent.Name = "bashy"
	trajectory.Agent.Version = "1"
	trajectory.Agent.ModelName = model
	trajectory.FinalMetrics.TotalPromptTokens = prompt
	trajectory.FinalMetrics.TotalCompletionTokens = completion
	trajectory.FinalMetrics.TotalSteps = len(steps)
	return json.MarshalIndent(trajectory, "", "  ")
}
func assistantText(response string) string {
	var v map[string]any
	if json.Unmarshal([]byte(response), &v) != nil {
		return ""
	}
	if choices, ok := v["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if m, ok := c["message"].(map[string]any); ok {
				if s, ok := m["content"].(string); ok {
					return s
				}
			}
			if s, ok := c["text"].(string); ok {
				return s
			}
		}
	}
	if m, ok := v["message"].(map[string]any); ok {
		if s, ok := m["content"].(string); ok {
			return s
		}
	}
	if s, ok := v["response"].(string); ok {
		return s
	}
	if blocks, ok := v["content"].([]any); ok {
		var parts []string
		for _, b := range blocks {
			if x, ok := b.(map[string]any); ok {
				if s, ok := x["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "")
	}
	if s, ok := v["output_text"].(string); ok {
		return s
	}
	if output, ok := v["output"].([]any); ok {
		var parts []string
		for _, item := range output {
			m, _ := item.(map[string]any)
			content, _ := m["content"].([]any)
			for _, raw := range content {
				part, _ := raw.(map[string]any)
				if s, ok := part["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}
func assistantTools(response string) []atifToolCall {
	var v map[string]any
	if json.Unmarshal([]byte(response), &v) != nil {
		return nil
	}
	var calls []any
	if choices, ok := v["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if m, ok := c["message"].(map[string]any); ok {
				calls, _ = m["tool_calls"].([]any)
			}
		}
	}
	if calls == nil {
		calls, _ = v["tool_calls"].([]any)
	}
	if calls == nil {
		if m, ok := v["message"].(map[string]any); ok {
			calls, _ = m["tool_calls"].([]any)
		}
	}
	if calls == nil {
		if blocks, ok := v["content"].([]any); ok {
			for _, raw := range blocks {
				block, _ := raw.(map[string]any)
				if block["type"] == "tool_use" {
					calls = append(calls, map[string]any{"id": block["id"], "function": map[string]any{"name": block["name"], "arguments": block["input"]}})
				}
			}
		}
	}
	if output, ok := v["output"].([]any); ok {
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if item["type"] == "function_call" {
				calls = append(calls, map[string]any{"id": item["call_id"], "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}})
			}
		}
	}
	var out []atifToolCall
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		id, _ := call["id"].(string)
		if id == "" {
			id = fmt.Sprintf("call_%d", len(out)+1)
		}
		args := map[string]any{}
		switch a := fn["arguments"].(type) {
		case string:
			_ = json.Unmarshal([]byte(a), &args)
		case map[string]any:
			args = a
		}
		out = append(out, atifToolCall{id, name, args})
	}
	return out
}
