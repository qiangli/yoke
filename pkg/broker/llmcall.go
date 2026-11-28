package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"
	"github.com/qiangli/yoke/pkg/broker/door"
)

// CallSchemaVersion names the envelope of `llm call`.
const CallSchemaVersion = "bashy-llm-call-v1"

// CallUsage is the token count the door reported for one call.
type CallUsage struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

// CallError is the typed failure of one call. Kind is one of: door (the
// request did not complete), refusal (the model declined), schema (the
// answer is not JSON matching --schema).
type CallError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (e *CallError) Error() string { return "llm call: " + e.Message }

// CallResult is the bashy-llm-call-v1 envelope. JSON is set only with
// --schema; Usage only when the door reports it (never estimated).
type CallResult struct {
	SchemaVersion string          `json:"schema_version"`
	Model         string          `json:"model"`
	Text          string          `json:"text"`
	JSON          json.RawMessage `json:"json,omitempty"`
	Usage         *CallUsage      `json:"usage,omitempty"`
	Error         *CallError      `json:"error,omitempty"`
}

// CallRequest is one one-shot model request. Model is a door model, band or
// MODEL@DOOR. Schema, when set, is the JSON Schema the answer must match.
type CallRequest struct {
	Model     string
	System    string
	Prompt    string
	Schema    json.RawMessage
	MaxTokens int64
}

// Call sends one non-streaming chat completion through the door's
// OpenAI-compatible route: the local door, or the named door for MODEL@DOOR
// (ResolveDoorModel). The envelope is returned even on a typed failure, with
// Error set and the same *CallError as the error.
func Call(ctx context.Context, req CallRequest) (CallResult, error) {
	res := CallResult{SchemaVersion: CallSchemaVersion, Model: req.Model}
	fail := func(kind, format string, a ...any) (CallResult, error) {
		res.Error = &CallError{Kind: kind, Message: fmt.Sprintf(format, a...)}
		return res, res.Error
	}
	var resolved *jsonschema.Resolved
	if len(req.Schema) > 0 {
		var schema jsonschema.Schema
		if err := json.Unmarshal(req.Schema, &schema); err != nil {
			return res, fmt.Errorf("llm call: --schema is not a JSON Schema: %w", err)
		}
		var err error
		if resolved, err = schema.Resolve(nil); err != nil {
			return res, fmt.Errorf("llm call: --schema is not a JSON Schema: %w", err)
		}
	}
	model, base, key := req.Model, "", ""
	if m, ok, err := ResolveDoorModel(ctx, req.Model); ok {
		if err != nil {
			return fail("door", "%v", err)
		}
		model, base, key = m.ModelID, m.BaseURL, m.Key
	} else {
		token, err := door.Token()
		if err != nil {
			return res, err
		}
		base, key = door.BaseURL(), token
		if session := strings.TrimSpace(os.Getenv(SessionEnv)); session != "" {
			base += "/s/" + session
		}
		base += "/v1"
	}
	body := map[string]any{"model": model, "stream": false}
	var messages []map[string]string
	if req.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": req.System})
	}
	body["messages"] = append(messages, map[string]string{"role": "user", "content": req.Prompt})
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if resolved != nil {
		body["response_format"] = map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "result", "schema": req.Schema}}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return res, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return res, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		// A URL error repeats the request URL, which may carry a credential.
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap()
		}
		return fail("door", "the door did not answer (%v); for the local door run `bashy llm up`", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fail("door", "read the door's answer: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 512 {
			msg = msg[:512]
		}
		return fail("door", "the door answered %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return fail("door", "the door's answer is not a chat completion")
	}
	if out.Model != "" {
		res.Model = out.Model
	}
	if out.Usage != nil {
		res.Usage = &CallUsage{Input: out.Usage.Prompt, Output: out.Usage.Completion}
	}
	message := out.Choices[0].Message
	res.Text = message.Content
	if message.Refusal != "" {
		return fail("refusal", "the model refused: %s", message.Refusal)
	}
	if resolved != nil {
		text := strings.TrimSpace(res.Text)
		// Models often fence a JSON answer; the fence is not part of it.
		if strings.HasPrefix(text, "```") && strings.HasSuffix(text, "```") {
			text = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```"), "```")
		}
		var value any
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			return fail("schema", "the answer is not JSON: %v", err)
		}
		if err := resolved.Validate(value); err != nil {
			return fail("schema", "the answer does not match the schema: %v", err)
		}
		res.JSON = json.RawMessage(strings.TrimSpace(text))
	}
	return res, nil
}

func newCallCmd() *cobra.Command {
	var req CallRequest
	var band, prompt, schema string
	var asJSON bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "call [--model M|M@DOOR | --band L] [flags] PROMPT|-",
		Short: "one-shot model call: one request in, one response out",
		Long: `call sends one request to a model through the door and prints the answer.

  bashy llm call --band L4 "summarize: ..."         # a band on the local door
  bashy llm call --model qwen3:8b@dragon -          # a model on a shared door, prompt on stdin
  bashy llm call --band L4 --schema s.json --json "extract ..."

--model takes a door model, registry model or agent, or MODEL@DOOR for a door
another host shared with this one (bashy llm door import). --band is a band
name (L4) on the local door. PROMPT is the positional words, --prompt, or -
for standard input.

--schema FILE (or inline JSON) asks for an answer matching that JSON Schema;
the parsed answer is the envelope's json field. --json prints the
bashy-llm-call-v1 envelope: {model, text, json, usage {input, output}}; usage
is what the door reported and is omitted when it reported none. A failure is
typed in the envelope's error {kind, message}: door, refusal or schema.

One request, one response: no streaming, no tool use, no session (bashy chat).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			if (req.Model == "") == (band == "") {
				return errors.New("llm call: name one of --model M (or M@DOOR) or --band L")
			}
			if band != "" {
				req.Model = band
			}
			if len(args) > 0 {
				if prompt != "" {
					return errors.New("llm call: give the prompt as words or with --prompt, not both")
				}
				prompt = strings.Join(args, " ")
			}
			if prompt == "-" {
				data, err := io.ReadAll(c.InOrStdin())
				if err != nil {
					return err
				}
				prompt = string(data)
			}
			if strings.TrimSpace(prompt) == "" {
				return errors.New("llm call: no prompt (words, --prompt, or - for standard input)")
			}
			req.Prompt = prompt
			if s := strings.TrimSpace(schema); strings.HasPrefix(s, "{") {
				req.Schema = json.RawMessage(s)
			} else if s != "" {
				data, err := os.ReadFile(s)
				if err != nil {
					return fmt.Errorf("llm call: --schema: %w", err)
				}
				req.Schema = json.RawMessage(data)
			}
			ctx := c.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			res, err := Call(ctx, req)
			out := c.OutOrStdout()
			switch {
			case asJSON || os.Getenv("BASHY_AGENTIC") != "":
				if res.Error != nil || err == nil {
					data, _ := json.Marshal(res)
					fmt.Fprintln(out, string(data))
				}
			case err != nil:
			case res.JSON != nil:
				fmt.Fprintln(out, string(res.JSON))
			default:
				fmt.Fprint(out, res.Text)
				if !strings.HasSuffix(res.Text, "\n") {
					fmt.Fprintln(out)
				}
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Model, "model", "", "door model, registry model or agent, or MODEL@DOOR for a shared door")
	f.StringVar(&band, "band", "", "band on the local door, e.g. L4")
	f.StringVar(&req.System, "system", "", "system prompt")
	f.StringVar(&prompt, "prompt", "", "the prompt (alternative to the positional words)")
	f.StringVar(&schema, "schema", "", "JSON Schema the answer must match: a file, or inline JSON")
	f.Int64Var(&req.MaxTokens, "max-tokens", 0, "cap on the answer's tokens")
	f.DurationVar(&timeout, "timeout", 10*time.Minute, "give up after this long")
	f.BoolVar(&asJSON, "json", false, "print the bashy-llm-call-v1 envelope")
	return cmd
}

// MCPCommand is the typed tool `llm`: one `llm call`, arguments bound by
// schema, the bashy-llm-call-v1 envelope on stdout. K2 serves this same
// definition for the Model offer class.
func MCPCommand() mcp.RegisteredCommand {
	return mcp.RegisteredCommand{
		Name:     "llm",
		Synopsis: "one-shot model call: one prompt in, one answer out",
		Usage: "model is a door model, band (L4) or MODEL@DOOR; schema is a JSON Schema (inline JSON or a file) the answer must match. " +
			"Output: bashy-llm-call-v1 {model, text, json (with schema), usage {input, output}, error {kind: door|refusal|schema, message}}. " +
			"Unit: token. No streaming, tool use or session.",
		Schema: &tool.ArgSchema{Flags: []tool.ArgFlag{
			{Name: "prompt", Required: true},
			{Name: "model", Required: true},
			{Name: "system"},
			{Name: "schema"},
			{Name: "max-tokens", Type: "int"},
		}},
		// No host write or exec: the call reads a model over the door.
		Effects: []string{"net", "read", "spend"},
		Run: func(ctx context.Context, argv []string, stdin, _ string) (string, string, int) {
			cmd := newCallCmd()
			cmd.SetContext(ctx)
			var out, errb bytes.Buffer
			cmd.SetArgs(append([]string{"--json"}, argv...))
			cmd.SetIn(strings.NewReader(stdin))
			cmd.SetOut(&out)
			cmd.SetErr(&errb)
			if err := cmd.Execute(); err != nil {
				fmt.Fprintln(&errb, err)
				var typed *CallError
				if errors.As(err, &typed) {
					return out.String(), errb.String(), 1
				}
				return out.String(), errb.String(), 2
			}
			return out.String(), errb.String(), 0
		},
	}
}
