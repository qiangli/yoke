package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/anthropic"
	"github.com/qiangli/yoke/pkg/llmgw/resolve"
)

func anthropicPayload(stream bool) map[string]any {
	return map[string]any{"model": "L4", "max_tokens": 64, "stream": stream, "messages": []map[string]any{{"role": "user", "content": "hello"}}}
}

func TestAnthropicMessages_NonStreamUsesSharedBackend(t *testing.T) {
	var gotModel string
	up := newUpstream(t, "alpha", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ChatCompletionsPath {
			t.Errorf("path=%q", r.URL.Path)
		}
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotModel = req.Model
		echoJSON(w, r)
	})
	env := newEnv(t, []resolve.ModelRow{row("L4", "alpha")}, []*upstream{up}, func(c *Config) {
		c.Authorize = func(r *http.Request) (string, int, error) {
			if r.Header.Get("Authorization") != "Bearer sdk-key" {
				t.Errorf("authorization=%q", r.Header.Get("Authorization"))
			}
			return "alice", 100, nil
		}
	})
	w := env.do(t, http.MethodPost, AnthropicMessagesPath, anthropicPayload(false), map[string]string{"x-api-key": "sdk-key"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotModel != "L4" {
		t.Errorf("model=%q", gotModel)
	}
	var got anthropic.MessagesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "message" || len(got.Content) != 1 || got.Content[0].Text != "hi" {
		t.Errorf("response=%+v", got)
	}
	if got.StopReason == nil || *got.StopReason != "end_turn" {
		t.Errorf("stop=%v", got.StopReason)
	}
	if got.Usage != (anthropic.Usage{InputTokens: 3, OutputTokens: 4}) {
		t.Errorf("usage=%+v", got.Usage)
	}
}

func TestAnthropicMessages_Stream(t *testing.T) {
	up := newUpstream(t, "alpha", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"L4\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"L4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	env := newEnv(t, []resolve.ModelRow{row("L4", "alpha")}, []*upstream{up}, nil)
	w := env.do(t, http.MethodPost, MessagesPath, anthropicPayload(true), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, want := range []string{"event: message_start", `"type":"text_delta","text":"hello"`, `"stop_reason":"end_turn"`, `"output_tokens":1`, "event: message_stop"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("body missing %q:\n%s", want, w.Body.String())
		}
	}
}

func TestAnthropicMessages_ErrorShape(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("L4", "alpha")}, []*upstream{up}, nil)
	w := env.do(t, http.MethodPost, MessagesPath, map[string]any{"max_tokens": 1, "messages": []any{}}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got anthropic.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "error" || got.Error.Type != "invalid_request_error" || !strings.Contains(got.Error.Message, "model is required") {
		t.Errorf("error=%+v", got)
	}
}

func TestAnthropicMessages_CatalogFailureShape(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("L4", "alpha")}, []*upstream{up}, func(c *Config) {
		c.Catalog.(*fakeCatalog).err = errors.New("inventory offline")
	})
	w := env.do(t, http.MethodPost, AnthropicMessagesPath, anthropicPayload(false), nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", w.Code, w.Body.String())
	}
	var got anthropic.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "error" || got.Error.Type != "catalog_unavailable" || !strings.Contains(got.Error.Message, "inventory offline") {
		t.Errorf("error=%+v", got)
	}
}

func TestAnthropicMessages_UpstreamErrorShapeAndStatus(t *testing.T) {
	up := newUpstream(t, "alpha", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request from backend","type":"invalid_request_error"}}`)
	})
	env := newEnv(t, []resolve.ModelRow{row("L4", "alpha")}, []*upstream{up}, nil)
	w := env.do(t, http.MethodPost, MessagesPath, anthropicPayload(false), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got anthropic.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "error" || got.Error.Type != "invalid_request_error" || got.Error.Message != "bad request from backend" {
		t.Errorf("error=%+v", got)
	}
}
