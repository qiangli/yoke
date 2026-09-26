package cligw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

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
	prompt, err := RenderPrompt(&req)
	if err != nil {
		return b.writeResponse(w, r, http.StatusBadRequest, "application/json", errorBody(err.Error()), modify)
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

	var deltas []string
	result, err := worker.Do(r.Context(), prompt, func(event Event) {
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

	text, stopped := applyStops(result.Text, req.Stop)
	finish := "stop"
	if stopped {
		finish = "stop"
	}
	id := fmt.Sprintf("chatcmpl-llmgw-%016x", completionSequence.Add(1))
	usage := openai.Usage{
		PromptTokens: int(result.Usage.InputTokens), CompletionTokens: int(result.Usage.OutputTokens),
		TotalTokens: int(result.Usage.TotalTokens),
	}
	if result.Usage.Estimated {
		w.Header().Set("X-Bashy-Usage-Estimated", "true")
	}
	if req.Stream {
		payload := streamBody(id, req.Model, deltas, text, finish, usage)
		return b.writeResponse(w, r, http.StatusOK, "text/event-stream", payload, modify)
	}
	completion := openai.ChatCompletion{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []openai.ChatChoice{{Index: 0, Message: &openai.ChatResponseMessage{Role: "assistant", Content: text}, FinishReason: &finish}},
		Usage:   usage,
	}
	payload, _ := json.Marshal(completion)
	return b.writeResponse(w, r, http.StatusOK, "application/json", payload, modify)
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
		writeSSE(&out, openai.ChatCompletion{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChatChoice{{Index: 0, Delta: &openai.ChatResponseMessage{Role: roleFor(i), Content: event}}},
		})
	}
	if len(events) == 0 && remaining != "" {
		writeSSE(&out, openai.ChatCompletion{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChatChoice{{Index: 0, Delta: &openai.ChatResponseMessage{Role: "assistant", Content: remaining}}},
		})
	}
	writeSSE(&out, openai.ChatCompletion{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []openai.ChatChoice{{Index: 0, Delta: &openai.ChatResponseMessage{}, FinishReason: &finish}}, Usage: usage,
	})
	out.WriteString("data: [DONE]\n\n")
	return out.Bytes()
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
