package broker

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesModelRouting(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/openai/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				h := newHarness(t, nil)
				resp, body := h.raw(path, map[string]any{"model": "llama3.2:3b", "input": "hi", "instructions": "be terse", "stream": stream, "max_output_tokens": 32})
				if resp.StatusCode != http.StatusOK || resp.Header.Get(BackendHeader) != BackendLocal {
					t.Fatalf("local routing: status=%d backend=%q body=%s", resp.StatusCode, resp.Header.Get(BackendHeader), body)
				}
				sent := h.eng.body()
				if sent["model"] != "llama3.2:3b" || sent["max_completion_tokens"] != float64(32) {
					t.Fatalf("engine request: %#v", sent)
				}
				messages, _ := sent["messages"].([]any)
				if len(messages) != 2 || messages[1].(map[string]any)["content"] != "hi" {
					t.Fatalf("messages: %#v", messages)
				}
				if stream {
					if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, "output_text") {
						t.Fatalf("SSE: %s", body)
					}
				} else {
					var result map[string]any
					if err := json.Unmarshal([]byte(body), &result); err != nil || result["object"] != "response" || !strings.Contains(body, `"text":"ok"`) {
						t.Fatalf("response: %s", body)
					}
				}
				_, cliBody, _ := h.cli.snap()
				if cliBody != nil {
					t.Fatalf("local request reached cligw: %#v", cliBody)
				}
				rec := h.lastRecord(1)
				if rec.Backend != BackendLocal || rec.PromptTok != 7 || rec.OutputTok != 3 {
					t.Fatalf("audit: %+v", rec)
				}
			})
		}
		t.Run(path+"/agent", func(t *testing.T) {
			h := newHarness(t, nil)
			resp, body := h.raw(path, map[string]any{"model": "L4", "input": "hi"})
			_, sent, _ := h.cli.snap()
			if resp.StatusCode != http.StatusOK || sent["model"] != "L4" || sent["input"] != "hi" || h.eng.body() != nil {
				t.Fatalf("agent routing: %d %s sent=%#v", resp.StatusCode, body, sent)
			}
		})
	}
}

func TestResponsesLocalErrors(t *testing.T) {
	h := newHarness(t, nil)
	resp, body := h.raw("/v1/responses", map[string]any{"model": "llama3.2:3b", "input": "hi", "store": true})
	if resp.StatusCode != http.StatusBadRequest || h.eng.body() != nil {
		t.Fatalf("stateful request: %d %s", resp.StatusCode, body)
	}
	h.eng.chatFn = func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
	}
	resp, body = h.raw("/v1/responses", map[string]any{"model": "llama3.2:3b", "input": "hi"})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "busy") {
		t.Fatalf("engine error: %d %s", resp.StatusCode, body)
	}
}
