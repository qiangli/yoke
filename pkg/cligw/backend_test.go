package cligw

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/gateway"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
	"github.com/qiangli/yoke/pkg/llmgw/resolve"
)

func TestAgentBackendNonStream(t *testing.T) {
	backend, cleanup := testBackend(t, "backend-text")
	defer cleanup()
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if attempt.Status != http.StatusOK || !attempt.Committed || attempt.CanRetry {
		t.Fatalf("attempt = %+v", attempt)
	}
	var got openai.ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "test-model" || got.Choices[0].Message.Content != "hello from cli" {
		t.Fatalf("completion = %+v", got)
	}
	if got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestAgentBackendDrainsFastCLIOutputBeforeWaitReturns(t *testing.T) {
	backend, cleanup := testBackend(t, "backend-burst")
	defer cleanup()
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if attempt.Status != http.StatusOK || !attempt.Committed || attempt.CanRetry {
		t.Fatalf("attempt = %+v; backend error = %q", attempt, rec.Header().Get("X-Bashy-Backend-Error"))
	}
	var got openai.ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage.CompletionTokens != 96 || !strings.Contains(got.Choices[0].Message.Content, "chunk-095") {
		t.Fatalf("CLI output was truncated before its terminal event: usage=%+v content=%q", got.Usage, got.Choices[0].Message.Content)
	}
}

func TestAgentBackendStream(t *testing.T) {
	backend, cleanup := testBackend(t, "backend-stream")
	defer cleanup()
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if attempt.Status != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("attempt=%+v content-type=%q", attempt, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hello "`) || !strings.Contains(body, `"content":"world"`) || !strings.Contains(body, `"finish_reason":"stop"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream = %s", body)
	}
	chunks := strings.Split(strings.TrimSuffix(strings.TrimSuffix(body, "data: [DONE]\n\n"), "\n\n"), "\n\n")
	for i, chunk := range chunks {
		var doc map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(chunk, "data: ")), &doc); err != nil {
			t.Fatal(err)
		}
		_, hasUsage := doc["usage"]
		if hasUsage != (i == len(chunks)-1) {
			t.Fatalf("chunk %d usage presence = %v; stream=%s", i, hasUsage, body)
		}
	}
}

func TestAgentBackendToolCallRoundTrip(t *testing.T) {
	backend, cleanup := testBackend(t, "backend-tool")
	defer cleanup()
	modify := func(resp *http.Response) error {
		resp.Body = openai.ApplyToolCallExtractor(resp)
		return nil
	}
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","messages":[{"role":"user","content":"weather"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}]}`, modify)
	if attempt.Status != http.StatusOK {
		t.Fatalf("attempt = %+v", attempt)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	choice := doc["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("response = %s", rec.Body.String())
	}
	message := choice["message"].(map[string]any)
	if len(message["tool_calls"].([]any)) != 1 {
		t.Fatalf("response = %s", rec.Body.String())
	}
}

func TestAgentBackendStreamingToolCallRoundTrip(t *testing.T) {
	backend, cleanup := testBackend(t, "backend-tool")
	defer cleanup()
	modify := func(resp *http.Response) error {
		resp.Body = openai.ApplyToolCallExtractor(resp)
		return nil
	}
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"weather"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}]}`, modify)
	if attempt.Status != http.StatusOK {
		t.Fatalf("attempt = %+v", attempt)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream = %s", body)
	}
}

func TestAgentBackendImageIs400(t *testing.T) {
	backend := NewAgentBackend("test-agent", "test-model", nil)
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, nil)
	if attempt.Status != http.StatusBadRequest || !attempt.Committed || !strings.Contains(rec.Body.String(), "non-text") {
		t.Fatalf("attempt=%+v body=%s", attempt, rec.Body.String())
	}
}

func TestAgentBackendCrashCanRetry(t *testing.T) {
	backend, cleanup := testBackend(t, "backend-crash")
	defer cleanup()
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if attempt.Status != http.StatusBadGateway || !attempt.CanRetry || attempt.Committed || rec.Body.Len() != 0 {
		t.Fatalf("attempt=%+v body=%q", attempt, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("X-Bashy-Backend-Error"), "exit status 7") {
		t.Fatalf("backend error header = %q", rec.Header().Get("X-Bashy-Backend-Error"))
	}
}

func TestAgentBackendClientCancelKillsWorker(t *testing.T) {
	started := t.TempDir() + "/started"
	t.Setenv("CLIGW_CANCEL_STARTED", started)
	backend, cleanup := testBackend(t, "backend-cancel")
	defer cleanup()
	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"wait"}]}`)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan gateway.Attempt, 1)
	go func() { done <- backend.Serve(rec, req, body, nil) }()
	waitFor(t, 3*time.Second, func() bool { _, err := os.Stat(started); return err == nil })
	cancel()
	select {
	case attempt := <-done:
		if attempt.Status != http.StatusBadGateway || !attempt.CanRetry {
			t.Fatalf("attempt = %+v", attempt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after client cancellation")
	}
	if stats := backend.Pool.Stats(); stats.Busy != 0 {
		t.Fatalf("pool still has busy worker: %+v", stats)
	}
}

func TestAgentBackendsFailOverThroughGateway(t *testing.T) {
	first, closeFirst := testBackendNamed(t, "first", "backend-crash")
	defer closeFirst()
	second, closeSecond := testBackendNamed(t, "second", "backend-text")
	defer closeSecond()
	backends := map[string]gateway.Backend{"first": first, "second": second}
	loaded := gateway.NewLoadedCache(time.Minute, time.Minute)
	loaded.Put("first", map[string]struct{}{"test-model": {}})
	handler := gateway.New(gateway.Config{
		Catalog:        staticCatalog{{Name: "test-model", Backend: "first"}, {Name: "test-model", Backend: "second"}},
		Backend:        func(name string) (gateway.Backend, bool) { b, ok := backends[name]; return b, ok },
		Authorize:      func(*http.Request) (string, int, error) { return "owner", 0, nil },
		Loaded:         loaded,
		ModifyResponse: func(resp *http.Response) error { return nil },
	})
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello from cli") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func testBackend(t *testing.T, mode string) (*AgentBackend, func()) {
	return testBackendNamed(t, "test-agent", mode)
}

func testBackendNamed(t *testing.T, name, mode string) (*AgentBackend, func()) {
	t.Helper()
	installFakeCatalog(t, "fake-"+mode, WarmCold, mode)
	// installFakeCatalog records test-agent; pool and backend names need not be
	// the registry lookup name for routing tests.
	pool := NewPool(context.Background(), "test-agent", PoolConfig{StartServers: 1, MaxWorkers: 1, MaxSpare: 1})
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 1 })
	return NewAgentBackend(name, "test-model", pool), func() { _ = pool.Close() }
}

func serveBackend(t *testing.T, backend *AgentBackend, body string, modify func(*http.Response) error) (*httptest.ResponseRecorder, gateway.Attempt) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	return rec, backend.Serve(rec, req, []byte(body), modify)
}

type staticCatalog []resolve.ModelRow

func (c staticCatalog) Rows(context.Context, string) []resolve.ModelRow   { return c }
func (staticCatalog) Alias(context.Context, string) (int, []string, bool) { return 0, nil, false }

// reasoning_effort decodes off the wire and rides the rendered prompt to the
// worker.
func TestChatRequestDecodesReasoningEffort(t *testing.T) {
	var req openai.ChatRequest
	body := `{"model":"m","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q", req.ReasoningEffort)
	}
	prompt, err := RenderCompletionPrompt(&req)
	if err != nil {
		t.Fatal(err)
	}
	if prompt.Effort != "high" {
		t.Fatalf("CompletionPrompt.Effort = %q", prompt.Effort)
	}
	req.ReasoningEffort = ""
	if prompt, _ = RenderCompletionPrompt(&req); prompt.Effort != "" {
		t.Fatalf("Effort = %q without a request effort", prompt.Effort)
	}
}
