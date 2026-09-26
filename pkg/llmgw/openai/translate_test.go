package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rawMessage(s string) json.RawMessage { return json.RawMessage(`"` + s + `"`) }

func TestCallAnthropic(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	var got struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Stream    bool   `json:"stream"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"hello from claude"}],"usage":{"input_tokens":5,"output_tokens":3}}`)
	}))
	t.Cleanup(srv.Close)

	req := &ChatRequest{
		Model:     "claude-sonnet",
		MaxTokens: 123,
		Messages: []ChatMessage{
			{Role: "system", Content: rawMessage("be brief")},
			{Role: "user", Content: rawMessage("hi")},
			{Role: "assistant", Content: rawMessage("hello")},
			{Role: "tool", Content: rawMessage("result")},
		},
	}
	comp, status, vendorErr, err := CallAnthropic(context.Background(), srv.Client(), srv.URL+"/v1/", "sk-vendor-key", "claude-3-5-sonnet", req)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || vendorErr != nil {
		t.Fatalf("status=%d vendorErr=%q", status, vendorErr)
	}
	if gotPath != "/v1/messages" || gotKey != "sk-vendor-key" || gotVersion != "2023-06-01" {
		t.Errorf("path=%q key=%q version=%q", gotPath, gotKey, gotVersion)
	}
	if got.Model != "claude-3-5-sonnet" || got.MaxTokens != 123 || got.System != "be brief" || got.Stream {
		t.Errorf("request=%+v", got)
	}
	if len(got.Messages) != 3 || got.Messages[2].Role != "user" {
		t.Errorf("messages=%+v", got.Messages)
	}
	if comp.Object != "chat.completion" || comp.Model != "claude-sonnet" {
		t.Errorf("completion=%+v", comp)
	}
	if len(comp.Choices) != 1 || comp.Choices[0].Message == nil || comp.Choices[0].Message.Content != "hello from claude" {
		t.Errorf("choices=%+v", comp.Choices)
	}
	if comp.Choices[0].FinishReason == nil || *comp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason=%v", comp.Choices[0].FinishReason)
	}
	if comp.Usage != (ChatUsage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}) {
		t.Errorf("usage=%+v", comp.Usage)
	}
}

func TestCallGemini(t *testing.T) {
	var gotPath, gotKey string
	var got struct {
		Contents []struct {
			Role string `json:"role"`
		} `json:"contents"`
		SystemInstruction *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"hi "},{"text":"from gemini"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}`)
	}))
	t.Cleanup(srv.Close)

	req := &ChatRequest{
		Model: "gem",
		Messages: []ChatMessage{
			{Role: "system", Content: rawMessage("be brief")},
			{Role: "user", Content: rawMessage("hi")},
			{Role: "assistant", Content: rawMessage("hello")},
		},
	}
	comp, status, vendorErr, err := CallGemini(context.Background(), srv.Client(), srv.URL+"/v1beta/", "sk-vendor-key", "gemini-2.0-flash", req)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || vendorErr != nil {
		t.Fatalf("status=%d vendorErr=%q", status, vendorErr)
	}
	if gotPath != "/v1beta/models/gemini-2.0-flash:generateContent" || gotKey != "sk-vendor-key" {
		t.Errorf("path=%q key=%q", gotPath, gotKey)
	}
	if len(got.Contents) != 2 || got.Contents[0].Role != "user" || got.Contents[1].Role != "model" {
		t.Errorf("contents=%+v", got.Contents)
	}
	if got.SystemInstruction == nil || len(got.SystemInstruction.Parts) != 1 || got.SystemInstruction.Parts[0].Text != "be brief" {
		t.Errorf("systemInstruction=%+v", got.SystemInstruction)
	}
	if len(comp.Choices) != 1 || comp.Choices[0].Message == nil || comp.Choices[0].Message.Content != "hi from gemini" {
		t.Errorf("choices=%+v", comp.Choices)
	}
	if comp.Usage.TotalTokens != 6 {
		t.Errorf("usage=%+v", comp.Usage)
	}
}

func TestProviderNonSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"slow down"}`)
	}))
	t.Cleanup(srv.Close)

	comp, status, body, err := CallAnthropic(context.Background(), srv.Client(), srv.URL, "key", "model", &ChatRequest{})
	if err != nil || comp != nil || status != http.StatusTooManyRequests || string(body) != `{"error":"slow down"}` {
		t.Fatalf("comp=%v status=%d body=%q err=%v", comp, status, body, err)
	}
}

func TestStopMappings(t *testing.T) {
	for input, want := range map[string]string{"max_tokens": "length", "tool_use": "tool_calls", "end_turn": "stop"} {
		if got := MapAnthropicStop(input); got != want {
			t.Errorf("MapAnthropicStop(%q)=%q, want %q", input, got, want)
		}
	}
	for input, want := range map[string]string{"MAX_TOKENS": "length", "safety": "content_filter", "STOP": "stop"} {
		if got := MapGeminiFinish(input); got != want {
			t.Errorf("MapGeminiFinish(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestNormalizeVendorError(t *testing.T) {
	got := string(NormalizeVendorError("gemini", []byte("  denied  ")))
	want := `{"error":{"message":"denied","provider":"gemini","type":"upstream_error"}}`
	if got != want {
		t.Fatalf("NormalizeVendorError()=%s, want %s", got, want)
	}
	if got := string(NormalizeVendorError("gemini", nil)); !strings.Contains(got, "commercial provider gemini returned an error") {
		t.Fatalf("empty body=%s", got)
	}
}

func TestServeTranslated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`)
	}))
	t.Cleanup(srv.Close)

	body := []byte(`{"model":"gem","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	if !ServeTranslated(rec, req, srv.URL, "/v1/chat/completions", "key", "gemini", "gemini-x", body) {
		t.Fatal("ServeTranslated returned false")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var comp ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &comp); err != nil {
		t.Fatal(err)
	}
	if comp.Choices[0].Message.Content != "hi" {
		t.Fatalf("completion=%+v", comp)
	}
}

func TestServeTranslatedPseudoStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`)
	}))
	t.Cleanup(srv.Close)

	body := []byte(`{"model":"gem","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	ServeTranslated(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), srv.URL, "/v1/chat/completions", "key", "gemini", "gemini-x", body)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if got := rec.Body.String(); !strings.Contains(got, `"content":"hi"`) || !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Fatalf("stream=%q", got)
	}
}

func TestServeTranslatedNormalizesProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"bad key"}`)
	}))
	t.Cleanup(srv.Close)

	body := []byte(`{"model":"gem","messages":[]}`)
	rec := httptest.NewRecorder()
	ServeTranslated(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), srv.URL, "/v1/chat/completions", "key", "gemini", "gemini-x", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := `{"error":{"message":"{\"message\":\"bad key\"}","provider":"gemini","type":"upstream_error"}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body=%s, want %s", got, want)
	}
}

func TestServeTranslatedRejectsUnsupportedPathAndProvider(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	for _, tc := range []struct {
		path, provider string
	}{
		{path: "/v1/embeddings", provider: "gemini"},
		{path: "/v1/chat/completions", provider: "unknown"},
	} {
		rec := httptest.NewRecorder()
		ServeTranslated(rec, req, "", tc.path, "", tc.provider, "", []byte(`{}`))
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("path=%q provider=%q status=%d", tc.path, tc.provider, rec.Code)
		}
	}
}

func TestServeTranslatedBadBody(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeTranslated(rec, httptest.NewRequest(http.MethodPost, "/", nil), "", "/v1/chat/completions", "", "gemini", "", []byte(`{`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "parse chat body") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
