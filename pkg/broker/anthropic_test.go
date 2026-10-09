package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func (h *harness) raw(path string, body any) (*http.Response, string) {
	h.t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", h.server.URL+path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func anthReq(extra map[string]any) map[string]any {
	req := map[string]any{
		"model":      "llama3.2:3b",
		"max_tokens": 64,
		"system":     "be terse",
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}
	for k, v := range extra {
		req[k] = v
	}
	return req
}

func TestAnthropicMessagesLocalNonStreaming(t *testing.T) {
	h := newHarness(t, nil)
	h.eng.chatFn = func(w http.ResponseWriter, _ map[string]any) {
		io.WriteString(w, `{"id":"chatcmpl-1","model":"llama3.2:3b","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}
	for _, path := range []string{"/anthropic/v1/messages", "/v1/messages"} {
		resp, body := h.raw(path, anthReq(map[string]any{"temperature": 0.25, "stop_sequences": []string{"END"}}))
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		var msg struct {
			Type, Role, Model string
			Content           []struct{ Type, Text string }
			StopReason        string `json:"stop_reason"`
			Usage             struct{ Input_tokens, Output_tokens int }
		}
		if err := json.Unmarshal([]byte(body), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type != "message" || msg.Role != "assistant" || len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "hello" {
			t.Fatalf("%s: message %s", path, body)
		}
		if msg.StopReason != "max_tokens" || msg.Usage.Input_tokens != 7 || msg.Usage.Output_tokens != 3 {
			t.Fatalf("%s: stop/usage %s", path, body)
		}
		sent := h.eng.body()
		msgs := sent["messages"].([]any)
		if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "be terse" ||
			msgs[1].(map[string]any)["role"] != "user" {
			t.Fatalf("%s: engine messages %v", path, msgs)
		}
		if sent["max_tokens"] != float64(64) || sent["temperature"] != 0.25 || sent["stop"] != "END" || sent["model"] != "llama3.2:3b" {
			t.Fatalf("%s: engine params %v", path, sent)
		}
	}
	rec := h.lastRecord(2)
	if rec.PromptTok != 7 || rec.OutputTok != 3 || rec.Backend != BackendLocal {
		t.Fatalf("record %+v", rec)
	}
}

func TestAnthropicMessagesLocalToolUse(t *testing.T) {
	h := newHarness(t, nil)
	h.eng.chatFn = func(w http.ResponseWriter, _ map[string]any) {
		io.WriteString(w, `{"id":"c","model":"llama3.2:3b","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}
	req := anthReq(map[string]any{
		"tools": []map[string]any{{"name": "get_weather", "description": "weather", "input_schema": map[string]any{"type": "object"}}},
		"messages": []map[string]any{
			{"role": "user", "content": "weather?"},
			{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "toolu_0", "name": "get_weather", "input": map[string]any{"city": "Rome"}}}},
			{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_0", "content": "sunny"}}},
		},
	})
	resp, body := h.raw("/anthropic/v1/messages", req)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var msg struct {
		Content []struct {
			Type, ID, Name string
			Input          map[string]any
		}
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" || msg.Content[0].ID != "call_1" ||
		msg.Content[0].Name != "get_weather" || msg.Content[0].Input["city"] != "Paris" || msg.StopReason != "tool_use" {
		t.Fatalf("tool_use response %s", body)
	}
	sent := h.eng.body()
	if tools := sent["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("engine tools %v", sent["tools"])
	}
	var sawCall, sawResult bool
	for _, m := range sent["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" && mm["tool_calls"] != nil {
			sawCall = true
		}
		if mm["role"] == "tool" && mm["tool_call_id"] == "toolu_0" && mm["content"] == "sunny" {
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("engine did not receive tool_use/tool_result history: %v", sent["messages"])
	}
}

func TestAnthropicMessagesLocalStreaming(t *testing.T) {
	h := newHarness(t, nil)
	h.eng.chatFn = func(w http.ResponseWriter, body map[string]any) {
		if body["stream"] != true {
			t.Errorf("engine request not streaming: %v", body)
		}
		if so, _ := body["stream_options"].(map[string]any); so["include_usage"] != true {
			t.Errorf("stream_options %v", body["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"id":"c1","model":"llama3.2:3b","choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
			`{"id":"c1","model":"llama3.2:3b","choices":[{"delta":{"content":"lo"}}]}`,
			`{"id":"c1","model":"llama3.2:3b","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","model":"llama3.2:3b","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
		} {
			io.WriteString(w, "data: "+c+"\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}
	resp, body := h.raw("/anthropic/v1/messages", anthReq(map[string]any{"stream": true}))
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	var events []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v\n%s", events, body)
	}
	if !strings.Contains(body, `"text":"Hel"`) || !strings.Contains(body, `"text":"lo"`) ||
		!strings.Contains(body, `"stop_reason":"end_turn"`) || !strings.Contains(body, `"output_tokens":2`) {
		t.Fatalf("stream body %s", body)
	}
	rec := h.lastRecord(1)
	if rec.PromptTok != 9 || rec.OutputTok != 2 {
		t.Fatalf("record tokens %+v", rec)
	}
}

func TestAnthropicMessagesLocalErrors(t *testing.T) {
	h := newHarness(t, nil)
	check := func(name string, resp *http.Response, body string, status int, typ string) {
		t.Helper()
		if resp.StatusCode != status {
			t.Fatalf("%s: status %d %s, want %d", name, resp.StatusCode, body, status)
		}
		var e struct {
			Type  string
			Error struct{ Type, Message string }
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil || e.Type != "error" || e.Error.Type != typ || e.Error.Message == "" {
			t.Fatalf("%s: not an Anthropic error envelope: %s", name, body)
		}
	}

	h.eng.chatFn = func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad param","type":"invalid_request_error"}}`)
	}
	resp, body := h.raw("/anthropic/v1/messages", anthReq(nil))
	check("engine 400", resp, body, 400, "invalid_request_error")
	if !strings.Contains(body, "bad param") {
		t.Fatalf("engine message lost: %s", body)
	}

	h.eng.chatFn = func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"model runner crashed"}`)
	}
	resp, body = h.raw("/anthropic/v1/messages", anthReq(nil))
	check("engine 500", resp, body, 500, "api_error")
	if !strings.Contains(body, "model runner crashed") {
		t.Fatalf("engine message lost: %s", body)
	}

	resp, body = h.raw("/anthropic/v1/messages", anthReq(map[string]any{"tool_choice": map[string]any{"type": "bogus"}}))
	check("bad tool_choice", resp, body, 400, "invalid_request_error")

	resp, body = h.raw("/anthropic/v1/messages", anthReq(map[string]any{"messages": []map[string]any{{"role": "system", "content": "x"}}}))
	check("bad role", resp, body, 400, "invalid_request_error")
}

func TestAnthropicMessagesLocalMemoryRefusalIsAnthropicShaped(t *testing.T) {
	h := newHarness(t, nil)
	resp, body := h.raw("/anthropic/v1/messages", anthReq(map[string]any{"model": "huge:1t"}))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"api_error"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// The OpenAI and Ollama paths are untouched by the Anthropic translation.
func TestOpenAIAndOllamaLocalPathsUnchangedByAnthropic(t *testing.T) {
	h := newHarness(t, nil)
	resp, body := h.raw("/v1/chat/completions", chat("llama3.2:3b"))
	if resp.StatusCode != 200 || body != `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}` {
		t.Fatalf("openai: %d %s", resp.StatusCode, body)
	}
	resp, body = h.raw("/api/chat", chat("llama3.2:3b"))
	if resp.StatusCode != 200 || body != `{"response":"ok","done":true,"prompt_eval_count":11,"eval_count":5}` {
		t.Fatalf("ollama: %d %s", resp.StatusCode, body)
	}
	resp, body = h.raw("/v1/chat/completions", chat("huge:1t"))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"bashy_broker"`) {
		t.Fatalf("openai error shape changed: %d %s", resp.StatusCode, body)
	}
}
