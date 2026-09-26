package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExtractToolCallFromContent(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		wantName    string
		wantArgsKey string // a key the parsed args MUST contain
		wantOK      bool
	}{
		{
			name:        "bare object with args object",
			content:     `{"name": "bash", "arguments": {"command": "ls"}}`,
			wantName:    "bash",
			wantArgsKey: "command",
			wantOK:      true,
		},
		{
			name:        "args as stringified JSON",
			content:     `{"name": "bash", "arguments": "{\"command\":\"ls\"}"}`,
			wantName:    "bash",
			wantArgsKey: "command",
			wantOK:      true,
		},
		{
			name:    "plain text",
			content: "I'll list files for you.",
			wantOK:  false,
		},
		{
			name:    "empty",
			content: "",
			wantOK:  false,
		},
		{
			name:    "object without name",
			content: `{"arguments": {"command": "ls"}}`,
			wantOK:  false,
		},
		{
			name:    "object without arguments",
			content: `{"name": "bash"}`,
			wantOK:  false,
		},
		{
			name:    "object with trailing prose (not a pure tool call)",
			content: `{"name": "bash", "arguments": {"command": "ls"}} and that's the answer`,
			wantOK:  false,
		},
		{
			name:    "args stringified but not a JSON object",
			content: `{"name": "bash", "arguments": "ls"}`,
			wantOK:  false,
		},
		{
			name:        "leading whitespace ok",
			content:     "  \n\t" + `{"name":"bash","arguments":{"x":1}}` + "  ",
			wantName:    "bash",
			wantArgsKey: "x",
			wantOK:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, argsJSON, ok := extractToolCallFromContent(tc.content)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if name != tc.wantName {
				t.Errorf("name=%q, want %q", name, tc.wantName)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(argsJSON), &obj); err != nil {
				t.Fatalf("argsJSON %q is not a JSON object: %v", argsJSON, err)
			}
			if _, present := obj[tc.wantArgsKey]; !present {
				t.Errorf("expected key %q in args %v", tc.wantArgsKey, obj)
			}
		})
	}
}

func TestTransformChatCompletionJSON_ToolCall(t *testing.T) {
	in := []byte(`{
		"id":"chatcmpl-1",
		"object":"chat.completion",
		"created":1780914049,
		"model":"qwen2.5-coder:32b",
		"choices":[{
			"index":0,
			"message":{"role":"assistant","content":"{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}"},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":151,"completion_tokens":17,"total_tokens":168}
	}`)
	out, transformed := TransformChatCompletionJSON(in)
	if !transformed {
		t.Fatal("expected transformation, got passthrough")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	choices := doc["choices"].([]any)
	ch := choices[0].(map[string]any)
	if ch["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v, want tool_calls", ch["finish_reason"])
	}
	msg := ch["message"].(map[string]any)
	if msg["content"] != "" {
		t.Errorf("content should be cleared, got %v", msg["content"])
	}
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("fn.name=%v, want bash", fn["name"])
	}
	// arguments must be a JSON string, not the raw object.
	argStr, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("fn.arguments must be a string, got %T", fn["arguments"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argStr), &args); err != nil {
		t.Fatalf("fn.arguments is not parseable JSON: %v", err)
	}
	if args["command"] != "ls" {
		t.Errorf("command=%v, want ls", args["command"])
	}
	// Usage should be preserved.
	if _, ok := doc["usage"]; !ok {
		t.Errorf("usage block must be preserved")
	}
}

func TestTransformChatCompletionJSON_ToolCallsEnvelope(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":"{\"tool_calls\":[{\"name\":\"weather\",\"arguments\":{\"city\":\"Paris\"}},{\"name\":\"clock\",\"arguments\":{\"zone\":\"UTC\"}}]}"},"finish_reason":"stop"}]}`)
	out, transformed := TransformChatCompletionJSON(in)
	if !transformed {
		t.Fatal("expected tool_calls envelope transformation")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	choice := doc["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if calls := message["tool_calls"].([]any); len(calls) != 2 {
		t.Fatalf("tool_calls = %#v", calls)
	}
}

func TestTransformChatCompletionJSON_PlainText_NoChange(t *testing.T) {
	in := []byte(`{
		"id":"chatcmpl-2",
		"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]
	}`)
	out, transformed := TransformChatCompletionJSON(in)
	if transformed {
		t.Errorf("expected no transformation for plain text")
	}
	if !bytes.Equal(out, in) {
		t.Errorf("expected body to be returned unchanged")
	}
}

func TestTransformChatCompletionJSON_AlreadyStructured_NoChange(t *testing.T) {
	in := []byte(`{
		"choices":[{
			"message":{
				"role":"assistant",
				"content":"",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]
			},
			"finish_reason":"tool_calls"
		}]
	}`)
	_, transformed := TransformChatCompletionJSON(in)
	if transformed {
		t.Errorf("expected no transformation when tool_calls already structured")
	}
}

// ApplyToolCallExtractor: SSE Content-Type → wraps in streaming transformer.
func TestApplyToolCallExtractor_SSEPath(t *testing.T) {
	resp := &http.Response{
		Body:   io.NopCloser(strings.NewReader("data: {}\n\n")),
		Header: http.Header{},
	}
	resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
	wrapped := ApplyToolCallExtractor(resp)
	if _, ok := wrapped.(*SSEToolCallTransformer); !ok {
		t.Errorf("expected SSEToolCallTransformer for SSE Content-Type, got %T", wrapped)
	}
}

func TestApplyToolCallExtractor_JSONPath_RewritesAndSyncsFraming(t *testing.T) {
	in := `{"choices":[{"message":{"role":"assistant","content":"{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}"},"finish_reason":"stop"}]}`
	resp := &http.Response{
		Body:             io.NopCloser(strings.NewReader(in)),
		Header:           http.Header{},
		ContentLength:    int64(len(in)),
		TransferEncoding: []string{"chunked"},
	}
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", "999")
	resp.Header.Set("Transfer-Encoding", "chunked")
	wrapped := ApplyToolCallExtractor(resp)
	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"tool_calls"`) {
		t.Errorf("expected tool_calls in rewritten body, got: %s", string(out))
	}
	// Body length must be reflected in BOTH the int64 field (which
	// Go's reverse proxy honors for framing — leaving it stale was
	// the original truncation bug) and the header.
	if resp.ContentLength != int64(len(out)) {
		t.Errorf("resp.ContentLength=%d, want %d", resp.ContentLength, len(out))
	}
	gotHdr := resp.Header.Get("Content-Length")
	var n int
	_, _ = jsonAtoi(gotHdr, &n)
	if n != len(out) {
		t.Errorf("Content-Length header=%s, but body is %d bytes", gotHdr, len(out))
	}
	// Transfer-Encoding must be cleared since we've buffered to a
	// fixed length — otherwise a chunked-from-backend response
	// flows out with mismatched framing.
	if got := resp.Header.Get("Transfer-Encoding"); got != "" {
		t.Errorf("Transfer-Encoding should be cleared, got %q", got)
	}
	if len(resp.TransferEncoding) != 0 {
		t.Errorf("resp.TransferEncoding should be nil, got %v", resp.TransferEncoding)
	}
}

// Passthrough (non-tool-call) JSON also gets framing locked to the
// buffered length. Even though the body bytes are identical, the
// transferring transport gets the same correct framing as the
// rewritten path.
func TestApplyToolCallExtractor_JSONPath_PlainTextStillSyncsFraming(t *testing.T) {
	in := `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`
	resp := &http.Response{
		Body:          io.NopCloser(strings.NewReader(in)),
		Header:        http.Header{},
		ContentLength: -1, // the backend sent chunked
	}
	resp.Header.Set("Content-Type", "application/json")
	wrapped := ApplyToolCallExtractor(resp)
	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Errorf("plain-text body must be unchanged, got: %s", string(out))
	}
	if resp.ContentLength != int64(len(in)) {
		t.Errorf("resp.ContentLength=%d, want %d", resp.ContentLength, len(in))
	}
}

// SSE streaming: tool-call response gets rewritten end-to-end.
func TestSSEToolCallTransformer_RewritesToolCallStream(t *testing.T) {
	// Two-chunk stream — model emits a tool-call JSON in delta.content
	// across two deltas (mimicking how Ollama can split mid-string).
	sse := strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"qwen2.5-coder:32b","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":"{\"name\":\"bash\",\"arguments\""},"finish_reason":null}]}`,
		``,
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":":{\"command\":\"ls\"}}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")
	src := io.NopCloser(strings.NewReader(sse))
	tr := NewSSEToolCallTransformer(src)
	out, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `"tool_calls"`) {
		t.Errorf("expected tool_calls in transformed SSE, got:\n%s", got)
	}
	if !strings.Contains(got, `"name":"bash"`) {
		t.Errorf("expected name=bash in transformed SSE, got:\n%s", got)
	}
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Errorf("expected finish_reason=tool_calls in transformed SSE, got:\n%s", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("expected [DONE] marker preserved, got:\n%s", got)
	}
	// Usage block must be preserved.
	if !strings.Contains(got, `"usage"`) || !strings.Contains(got, `"prompt_tokens":1`) {
		t.Errorf("expected usage preserved, got:\n%s", got)
	}
}

// SSE streaming: plain text response passes through unchanged.
func TestSSEToolCallTransformer_PassthroughForPlainText(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"y","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"y","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")
	src := io.NopCloser(strings.NewReader(sse))
	tr := NewSSEToolCallTransformer(src)
	out, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, `"tool_calls"`) {
		t.Errorf("plain text must not be rewritten as tool_calls, got:\n%s", got)
	}
	if !strings.Contains(got, `"content":"Hello"`) {
		t.Errorf("original content must be preserved, got:\n%s", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("[DONE] must be preserved, got:\n%s", got)
	}
}

// SSE streaming: a backend that already emits structured tool_calls
// must pass through unchanged.
func TestSSEToolCallTransformer_AlreadyStructured_Passthrough(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"z","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_orig","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"z","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")
	src := io.NopCloser(strings.NewReader(sse))
	tr := NewSSEToolCallTransformer(src)
	out, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `"id":"call_orig"`) {
		t.Errorf("original call id must be preserved (passthrough), got:\n%s", got)
	}
}

// SSE streaming: a model that emits JSON-shaped content that turns
// out NOT to be a tool call (e.g. emits prose after) should fall
// back to replaying the buffered chunks rather than dropping them.
func TestSSEToolCallTransformer_FalseProbe_ReplaysOriginal(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"f","choices":[{"index":0,"delta":{"role":"assistant","content":"{partial but"},"finish_reason":null}]}`,
		``,
		`data: {"id":"f","choices":[{"index":0,"delta":{"content":" not a tool call"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")
	src := io.NopCloser(strings.NewReader(sse))
	tr := NewSSEToolCallTransformer(src)
	out, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, `"tool_calls"`) {
		t.Errorf("must not synthesize tool_calls from non-conforming content, got:\n%s", got)
	}
	if !strings.Contains(got, "not a tool call") {
		t.Errorf("original text must be replayed when probe was a false positive, got:\n%s", got)
	}
}

// jsonAtoi is a tiny helper avoiding strconv to keep imports lean
// inside the test file. Sets *out to the parsed number; returns
// (consumed, err).
func jsonAtoi(s string, out *int) (int, error) {
	*out = 0
	for i, r := range s {
		if r < '0' || r > '9' {
			return i, nil
		}
		*out = *out*10 + int(r-'0')
	}
	return len(s), nil
}
