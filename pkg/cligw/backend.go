package cligw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/gateway"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

// AgentBackend exposes one fleet agent's worker pool as an llmgw backend.
type AgentBackend struct {
	Agent string
	Model string
	Pool  *Pool
}

// NewAgentBackend returns a gateway backend for one agent and model.
func NewAgentBackend(agent, model string, pool *Pool) *AgentBackend {
	return &AgentBackend{Agent: agent, Model: model, Pool: pool}
}

func (b *AgentBackend) Name() string {
	if b.Agent != "" {
		return b.Agent
	}
	if b.Pool != nil {
		return b.Pool.agent
	}
	return ""
}

func (b *AgentBackend) Capacity(context.Context) gateway.Capacity {
	if b.Pool == nil {
		return gateway.Capacity{}
	}
	stats := b.Pool.Stats()
	loaded := []string(nil)
	if stats.Idle > 0 && b.Model != "" {
		loaded = []string{b.Model}
	}
	return gateway.Capacity{
		Version: 1, MaxParallel: b.Pool.cfg.MaxWorkers, InFlight: stats.Busy,
		Queued: stats.Queued, LoadedModels: loaded, NumLoadedMax: stats.Idle,
	}
}

func (b *AgentBackend) Loaded(context.Context) (map[string]struct{}, bool) {
	if b.Pool == nil {
		return nil, false
	}
	if b.Pool.Stats().Idle == 0 {
		return map[string]struct{}{}, true
	}
	return map[string]struct{}{b.Model: {}}, true
}

var completionSequence atomic.Uint64

func (b *AgentBackend) Serve(w http.ResponseWriter, r *http.Request, body []byte, modify func(*http.Response) error) gateway.Attempt {
	var req openai.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return b.writeResponse(w, r, http.StatusBadRequest, "application/json", errorBody("decode chat request: "+err.Error()), modify)
	}
	prompt, err := RenderCompletionPrompt(&req)
	if err != nil {
		return b.writeResponse(w, r, http.StatusBadRequest, "application/json", errorBody(err.Error()), modify)
	}
	if err := fleet.ValidEffort(prompt.Effort); err != nil {
		return b.writeResponse(w, r, http.StatusBadRequest, "application/json", errorBody("reasoning_effort: "+err.Error()), modify)
	}
	if b.Pool == nil {
		return gateway.Attempt{Status: http.StatusServiceUnavailable, CanRetry: true}
	}

	worker, err := b.Pool.Acquire(r.Context())
	if err != nil {
		w.Header().Set("Retry-After", "1")
		return gateway.Attempt{Status: http.StatusServiceUnavailable, CanRetry: true}
	}
	defer b.Pool.Release(worker)

	// An effort this worker's binding or tool cannot serve is the caller's
	// error: 400, and not retryable on another backend of the same identity.
	if err := worker.CheckEffort(prompt.Effort); err != nil {
		return b.writeResponse(w, r, http.StatusBadRequest, "application/json", errorBody(err.Error()), modify)
	}

	var deltas []string
	result, err := worker.DoCompletion(r.Context(), prompt, func(event Event) {
		if event.Text != "" {
			deltas = append(deltas, event.Text)
		}
	})
	if err != nil || result.Outcome == OutcomeError {
		message := "CLI worker failed"
		if err != nil {
			message = err.Error()
		}
		w.Header().Set("X-Bashy-Backend-Error", message)
		return gateway.Attempt{Status: http.StatusBadGateway, CanRetry: true}
	}

	text, finish := FinishChat(req, result.Text)
	id := NewCompletionID()
	if result.Usage.Estimated {
		w.Header().Set("X-Bashy-Usage-Estimated", "true")
	}
	contentType, payload := EncodeChat(req, text, finish, id, deltas, result.Usage)
	return b.writeResponse(w, r, http.StatusOK, contentType, payload, modify)
}

// NewCompletionID mints one chat completion id.
func NewCompletionID() string {
	return fmt.Sprintf("chatcmpl-llmgw-%016x", completionSequence.Add(1))
}

// FinishChat applies the request's stop sequences and tool-answer recovery to
// a worker's answer text, returning the served text and finish reason.
func FinishChat(req openai.ChatRequest, text string) (string, string) {
	text, stopped := applyStops(text, rawJSON(req.Stop))
	if len(req.Tools) > 0 {
		text = recoverToolAnswer(text, req.Tools)
	}
	finish := "stop"
	if stopped {
		finish = "stop"
	}
	return text, finish
}

// EncodeChat builds the OpenAI chat completion payload for a finished turn,
// streaming SSE when the request asked for it. Usage stays in worker units;
// callers set X-Bashy-Usage-Estimated themselves when it is estimated.
func EncodeChat(req openai.ChatRequest, answer, finish, id string, deltas []string, usage Usage) (string, []byte) {
	ou := openai.Usage{
		PromptTokens: int(usage.InputTokens), CompletionTokens: int(usage.OutputTokens),
		TotalTokens: int(usage.TotalTokens),
	}
	if req.Stream {
		return "text/event-stream", streamBody(id, req.Model, deltas, answer, finish, ou)
	}
	completion := openai.ChatCompletion{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []openai.ChatChoice{{Index: 0, Message: &openai.ChatResponseMessage{Role: "assistant", Content: answer}, FinishReason: &finish}},
		Usage:   ou,
	}
	payload, _ := json.Marshal(completion)
	return "application/json", payload
}

func (b *AgentBackend) writeResponse(w http.ResponseWriter, r *http.Request, status int, contentType string, payload []byte, modify func(*http.Response) error) gateway.Attempt {
	resp := &http.Response{
		StatusCode: status, Status: strconv.Itoa(status) + " " + http.StatusText(status),
		Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload)),
		ContentLength: int64(len(payload)), Request: r,
	}
	resp.Header.Set("Content-Type", contentType)
	resp.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	if modify != nil {
		if err := modify(resp); err != nil {
			_ = resp.Body.Close()
			w.Header().Set("X-Bashy-Backend-Error", err.Error())
			return gateway.Attempt{Status: http.StatusBadGateway, CanRetry: true}
		}
	}
	for key, values := range resp.Header {
		w.Header().Del(key)
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, copyErr := io.Copy(w, resp.Body)
	_ = resp.Body.Close()
	if copyErr != nil {
		return gateway.Attempt{Status: http.StatusBadGateway, Committed: n > 0}
	}
	return gateway.Attempt{Status: resp.StatusCode, Committed: true}
}

func errorBody(message string) []byte {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message, "type": "invalid_request_error"}})
	return body
}

// rawJSON re-encodes a decoded "any" request field (stop, tool_choice) so the
// string-or-array forms can be parsed uniformly; nil stays nil.
func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func applyStops(text string, raw json.RawMessage) (string, bool) {
	var stops []string
	var one string
	if json.Unmarshal(raw, &one) == nil {
		stops = []string{one}
	} else {
		_ = json.Unmarshal(raw, &stops)
	}
	cut := len(text)
	for _, stop := range stops {
		if stop != "" {
			if at := strings.Index(text, stop); at >= 0 && at < cut {
				cut = at
			}
		}
	}
	return text[:cut], cut != len(text)
}

func streamBody(id, model string, events []string, finalText, finish string, usage openai.Usage) []byte {
	var out bytes.Buffer
	remaining := finalText
	created := time.Now().Unix()
	for i, event := range events {
		if len(remaining) == 0 {
			break
		}
		if len(event) > len(remaining) {
			event = remaining
		} else if !strings.HasPrefix(remaining, event) {
			if i == 0 {
				event = remaining
			} else {
				break
			}
		}
		remaining = remaining[len(event):]
		writeSSE(&out, streamChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChatChoice{{Index: 0, Delta: &openai.ChatResponseMessage{Role: roleFor(i), Content: event}}},
		})
	}
	if len(events) == 0 && remaining != "" {
		writeSSE(&out, streamChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChatChoice{{Index: 0, Delta: &openai.ChatResponseMessage{Role: "assistant", Content: remaining}}},
		})
	}
	writeSSE(&out, streamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []openai.ChatChoice{{Index: 0, Delta: &openai.ChatResponseMessage{}, FinishReason: &finish}}, Usage: &usage,
	})
	out.WriteString("data: [DONE]\n\n")
	return out.Bytes()
}

type streamChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []openai.ChatChoice `json:"choices"`
	Usage   *openai.Usage       `json:"usage,omitempty"`
}

func roleFor(index int) string {
	if index == 0 {
		return "assistant"
	}
	return ""
}

func writeSSE(out *bytes.Buffer, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	out.WriteString("data: ")
	out.Write(payload)
	out.WriteString("\n\n")
}

var _ gateway.Backend = (*AgentBackend)(nil)

var toolEnvelopeStart = regexp.MustCompile(`\{\s*"tool_calls"\s*:`)

// toolEnvelopeTail keeps only the tool-call envelope when a CLI answered a
// tool call with text around it (a sentence before, a markdown fence, or
// more text after): the worker was told to reply with the bare envelope, and
// the gateway turns exactly that into structured tool_calls. The first
// well-formed envelope wins. Failing that, the first complete call object
// after an envelope's opening bracket is returned as a one-call envelope: a
// model that batches calls and drops the array's closing bracket
// (`{"tool_calls":[{call}}{"tool_calls":[{call}}}`, gpt-5.5 on the codex seat)
// otherwise has every call fall through as text, and then narrates results it
// never got. Anything else is returned unchanged.
func toolEnvelopeTail(text string) string {
	starts := toolEnvelopeStart.FindAllStringIndex(text, -1)
	for _, at := range starts {
		dec := json.NewDecoder(strings.NewReader(text[at[0]:]))
		var env struct {
			ToolCalls []json.RawMessage `json:"tool_calls"`
		}
		if dec.Decode(&env) != nil || len(env.ToolCalls) == 0 {
			continue
		}
		return strings.TrimSpace(text[at[0] : at[0]+int(dec.InputOffset())])
	}
	for _, at := range starts {
		rest := strings.TrimLeft(text[at[1]:], " \t\r\n")
		if !strings.HasPrefix(rest, "[") {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(rest[1:]))
		var call json.RawMessage
		if dec.Decode(&call) != nil {
			continue
		}
		var probe struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(call, &probe) != nil || probe.Name == "" {
			continue
		}
		return `{"tool_calls":[` + string(call) + `]}`
	}
	return text
}

// recoverToolAnswer accepts a leading bare arguments object only when the
// offered tool schemas identify one callable tool. The envelope path keeps
// its existing behavior for model replies that already name their tool.
func recoverToolAnswer(answer string, tools []openai.Tool) string {
	if env := toolEnvelopeTail(answer); env != answer {
		return env
	}
	start := strings.TrimSpace(answer)
	if strings.HasPrefix(start, "```") {
		lineEnd := strings.IndexByte(start, '\n')
		if lineEnd < 0 || (start[3:lineEnd] != "" && strings.TrimSpace(start[3:lineEnd]) != "json") {
			return answer
		}
		start = strings.TrimSpace(start[lineEnd+1:])
	}
	if !strings.HasPrefix(start, "{") {
		return answer
	}
	dec := json.NewDecoder(strings.NewReader(start))
	var args map[string]json.RawMessage
	if dec.Decode(&args) != nil || args == nil || len(args) == 0 {
		return answer
	}
	raw := json.RawMessage(start[:dec.InputOffset()])
	var match string
	for _, offered := range tools {
		if offered.Type != "function" || offered.Function.Name == "" || !validToolArguments(args, offered.Function.Parameters, len(tools) > 1) {
			continue
		}
		if match != "" {
			return answer
		}
		match = offered.Function.Name
	}
	if match == "" {
		return answer
	}
	name, _ := json.Marshal(match)
	return `{"tool_calls":[{"name":` + string(name) + `,"arguments":` + string(raw) + `}]}`
}

func validToolArguments(args map[string]json.RawMessage, parameters json.RawMessage, exactRequired bool) bool {
	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required             []string        `json:"required"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}
	if json.Unmarshal(parameters, &schema) != nil || schema.Type != "object" || len(schema.Properties) == 0 {
		return false
	}
	required := make(map[string]bool, len(schema.Required))
	for _, key := range schema.Required {
		required[key] = true
		if _, ok := args[key]; !ok {
			return false
		}
	}
	if exactRequired && len(args) != len(required) {
		return false
	}
	for key, value := range args {
		property, ok := schema.Properties[key]
		if !ok {
			if exactRequired || string(schema.AdditionalProperties) == "false" {
				return false
			}
			continue
		}
		if exactRequired && !required[key] {
			return false
		}
		if !validJSONType(value, property.Type) {
			return false
		}
	}
	return true
}

func validJSONType(raw json.RawMessage, typ string) bool {
	if typ == "" {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch typ {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		n, ok := value.(float64)
		return ok && n == float64(int64(n))
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "null":
		return value == nil
	default:
		return false
	}
}
