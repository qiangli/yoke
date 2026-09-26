package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmitStreamBytes(t *testing.T) {
	finish := "stop"
	comp := &ChatCompletion{
		ID:      "chatcmpl-1",
		Object:  "chat.completion",
		Created: 123,
		Model:   "model-1",
		Choices: []ChatChoice{{
			Index:        0,
			Message:      &ChatResponseMessage{Role: "assistant", Content: "hello"},
			FinishReason: &finish,
		}},
		Usage: ChatUsage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
	}
	rec := httptest.NewRecorder()
	EmitStream(rec, comp)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type=%q", got)
	}
	want := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"model-1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"model-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n" +
		"data: [DONE]\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("stream bytes mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestEmitStreamWithoutFlusher(t *testing.T) {
	w := &plainResponseWriter{header: make(http.Header)}
	EmitStream(w, &ChatCompletion{})
	if w.status != http.StatusOK {
		t.Fatalf("status=%d", w.status)
	}
}

type plainResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *plainResponseWriter) Header() http.Header { return w.header }
func (w *plainResponseWriter) WriteHeader(status int) {
	w.status = status
}
func (w *plainResponseWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}
