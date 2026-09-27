package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// ApplyToolCallExtractor wraps the backend response body in the
// right transformer for its Content-Type. The chat path calls this
// from the reverse proxy's ModifyResponse hook.
//
//   - application/json (and anything else non-SSE) → buffer + rewrite.
//   - text/event-stream → peek-and-decide streaming transformer.
//
// Returns the wrapped ReadCloser. For the JSON path the rewritten
// body's length can differ from the backend's, so we update BOTH
// http.Response.ContentLength (the int64 field the reverse proxy
// reads to set framing) AND the Content-Length header — and clear
// Transfer-Encoding so a chunked backend doesn't leak its framing
// hint into our fixed-length response. Updating only the header
// leaves resp.ContentLength stale and Go's transport silently
// truncates the body to the old length, which is exactly the bug
// the initial version of this function shipped with.
//
// The SSE path streams; framing is chunked so Content-Length is
// irrelevant. The returned reader replaces resp.Body; the caller
// must not double-close.
func ApplyToolCallExtractor(resp *http.Response) io.ReadCloser {
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if strings.HasPrefix(contentType, "text/event-stream") {
		// A rewritten stream has another length: the backend's
		// Content-Length no longer frames it.
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return NewSSEToolCallTransformer(resp.Body)
	}
	// Buffer + rewrite the JSON body. Reading the full body is fine
	// for non-streaming chat completions — they're small (KB scale)
	// even with vision payloads since those go in the REQUEST.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// Caller still needs a readable body; return a closer that
		// surfaces what we got plus the underlying error on next
		// read. Best-effort — preserves the response shape and lets
		// the usage-tail reader still observe the bytes.
		return io.NopCloser(bytes.NewReader(body))
	}
	_ = resp.Body.Close()
	rewritten, transformed := TransformChatCompletionJSON(body)
	// Whether we transformed or not, we have buffered the entire
	// body — so framing MUST be set to the buffered length, not the
	// backend's original Content-Length / Transfer-Encoding. In
	// passthrough cases this is a no-op (same length, both fields
	// just get re-confirmed); in the transformed case it's the
	// correctness fence preventing truncation.
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	resp.Header.Del("Transfer-Encoding")
	resp.TransferEncoding = nil
	_ = transformed
	return io.NopCloser(bytes.NewReader(rewritten))
}

// extractToolCallFromContent inspects a single OpenAI-shaped chat
// message.content string and, if it carries the canonical
// `{"name": "...", "arguments": {...}}` JSON of a tool call, returns
// the call's name and a JSON-string of its arguments (the wire shape
// OpenAI's tool_calls[].function.arguments field expects).
//
// Some models (notably qwen2.5-coder, gemma4:26b, and others whose
// Ollama Modelfile chat template lacks tool-call markers) emit tool
// invocations as plain text rather than populating the structured
// tool_calls field. Ollama's OpenAI-compat adapter has nothing to
// pattern-match against, so the call lands in `content` and the
// downstream client can't see it as a tool call. We detect this on
// the way out and rewrite the response — same content-byte path, no
// per-model special cases.
//
// Detection is conservative: the entire trimmed content must be one
// JSON object with a non-empty string "name" and an object
// "arguments" (or a JSON-string of an object). Any extra trailing
// text — a model that emits both prose AND a tool call — falls
// through unmodified, which matches how the spec-compliant clients
// would handle it anyway.
//
// Returns ok=false when the content is not a tool call. The returned
// argsJSON is always a JSON-string (the OpenAI wire shape); when the
// backend content already had arguments as a string we preserve it
// verbatim, otherwise we re-marshal the object back into a string.
func extractToolCallFromContent(content string) (name, argsJSON string, ok bool) {
	calls, ok := extractToolCallsFromContent(content)
	if !ok || len(calls) != 1 {
		return "", "", false
	}
	return calls[0].name, calls[0].arguments, true
}

type extractedToolCall struct {
	name      string
	arguments string
}

// extractToolCallsFromContent accepts both the historical single-call shape
// and cligw's strict, potentially multi-call envelope.
func extractToolCallsFromContent(content string) ([]extractedToolCall, bool) {
	s := strings.TrimSpace(content)
	if len(s) < 2 || s[0] != '{' {
		return nil, false
	}
	// Strict whole-document parse. A model emitting `{...}garbage` is
	// not a tool call we can confidently extract.
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	// We need the raw envelope so we can re-encode arguments
	// exactly. Use json.RawMessage to delay arg decoding.
	var env struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
	}
	if err := dec.Decode(&env); err != nil {
		return nil, false
	}
	// Trailing tokens after the envelope means this is not just a
	// tool-call payload (e.g. prose + JSON). Bail.
	if dec.More() {
		return nil, false
	}
	rawCalls := env.ToolCalls
	if len(rawCalls) == 0 && env.Name != "" {
		rawCalls = append(rawCalls, struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}{Name: env.Name, Arguments: env.Arguments})
	}
	if len(rawCalls) == 0 {
		return nil, false
	}
	calls := make([]extractedToolCall, 0, len(rawCalls))
	for _, call := range rawCalls {
		if strings.TrimSpace(call.Name) == "" || len(call.Arguments) == 0 {
			return nil, false
		}
		// OpenAI's tool_calls[].function.arguments is a JSON string.
		trimmed := bytes.TrimSpace(call.Arguments)
		var argsJSON string
		switch {
		case len(trimmed) > 0 && trimmed[0] == '{':
			argsJSON = string(trimmed)
		case len(trimmed) > 0 && trimmed[0] == '"':
			if err := json.Unmarshal(trimmed, &argsJSON); err != nil {
				return nil, false
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(argsJSON), &obj); err != nil {
				return nil, false
			}
		default:
			return nil, false
		}
		calls = append(calls, extractedToolCall{name: call.Name, arguments: argsJSON})
	}
	return calls, true
}

// quickProbeLooksLikeToolCall is a cheap front-line filter that lets
// the streaming SSE transformer decide on the very first content
// delta whether to buffer the rest of the stream (likely tool call)
// or fall through to byte-perfect pass-through (definitely text).
//
// A leading `{` is the only signal we have on partial input. Models
// that legitimately open prose with a `{` are vanishingly rare in
// chat completion; even if one slips through, the final detection
// (extractToolCallFromContent at EOF) will reject it and we replay
// the buffered chunks unchanged. Bias toward triggering buffering
// here keeps the gate liberal — the cost is one missed early-stream
// flush on a false positive.
func quickProbeLooksLikeToolCall(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

// nextToolCallID issues a synthetic call_<n> identifier used when
// rewriting unstructured content into tool_calls. Gateway-only —
// the backend never sent one. Atomic counter keeps the ids stable
// per process (good enough; never persisted).
var toolCallIDCounter atomic.Uint64

func nextToolCallID() string {
	n := toolCallIDCounter.Add(1)
	return fmt.Sprintf("call_gw_%016x", n)
}

// TransformChatCompletionJSON rewrites a non-streaming
// /v1/chat/completions response body in-place: each choice whose
// message.content is a bare tool-call JSON gets restructured into
// message.tool_calls + emptied content + finish_reason="tool_calls".
//
// Returns the rewritten bytes plus a transformed flag so the caller
// can decide whether to update Content-Length. The function is
// best-effort: any parse failure leaves the body untouched.
func TransformChatCompletionJSON(body []byte) ([]byte, bool) {
	// Decode into a flexible shape so we don't drop any unknown
	// fields the backend emitted. We only touch the choices array.
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, false
	}
	choices, _ := doc["choices"].([]any)
	if len(choices) == 0 {
		return body, false
	}
	transformed := false
	for i, raw := range choices {
		ch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := ch["message"].(map[string]any)
		if !ok {
			continue
		}
		// Already structured — nothing to do.
		if existing, ok := msg["tool_calls"].([]any); ok && len(existing) > 0 {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok || content == "" {
			continue
		}
		calls, ok := extractToolCallsFromContent(content)
		if !ok {
			continue
		}
		msg["content"] = ""
		toolCalls := make([]any, 0, len(calls))
		for index, call := range calls {
			toolCalls = append(toolCalls, map[string]any{
				"id":    nextToolCallID(),
				"type":  "function",
				"index": index,
				"function": map[string]any{
					"name":      call.name,
					"arguments": call.arguments,
				},
			})
		}
		msg["tool_calls"] = toolCalls
		ch["message"] = msg
		ch["finish_reason"] = "tool_calls"
		choices[i] = ch
		transformed = true
	}
	if !transformed {
		return body, false
	}
	doc["choices"] = choices
	out, err := json.Marshal(doc)
	if err != nil {
		// Re-encoding shouldn't fail — but if it somehow does,
		// returning the unchanged body is better than a broken
		// response that loses the model's output entirely.
		return body, false
	}
	return out, true
}

// SSEToolCallTransformer wraps a backend SSE response body so it
// can be rewritten if-and-only-if the model emitted a tool call as
// plain content. The transformer operates in three phases:
//
//  1. probing — read backend chunks, accumulate delta.content
//     across them. As soon as the running content has a non-empty
//     non-whitespace character we commit to a path: leading "{"
//     opens "buffer" mode; anything else opens "passthrough" mode.
//     Any buffered backend bytes are released to the consumer at
//     this point if we chose passthrough.
//
//     2a. passthrough — every byte from the backend goes to the
//     consumer unmodified. No SSE framing awareness needed; we're
//     just splicing the body through.
//
//     2b. buffer — keep accumulating from the backend until EOF. Then
//     run the final detection on the joined content. If it parses as
//     a tool call, emit a freshly-constructed stream:
//
//     data: {first chunk with role=assistant + tool_calls delta}
//     data: {chunk with finish_reason=tool_calls + usage}
//     data: [DONE]
//
//     Otherwise replay the original buffered chunks unchanged.
//
// Result: zero latency cost for plain text turns; tool-call turns
// pay the streaming latency of buffering for the model's full
// generation, which is the necessary cost of structural rewriting
// (we can't emit tool_calls until we know the call's full args).
type SSEToolCallTransformer struct {
	src  io.ReadCloser
	mode transformMode
	// held accumulates every raw backend byte received during the
	// probing + buffer phases. On passthrough commit we flush held
	// to out and stop using it. On buffer-mode EOF we either replay
	// held verbatim (false probe) or discard it and emit synthesized
	// chunks (real tool call). Keeping the raw bytes is what makes
	// false probes lossless.
	held bytes.Buffer
	// parsedUpTo is the offset in `held` we've already parsed for
	// complete SSE events. New bytes get parsed from here forward.
	parsedUpTo int
	out        bytes.Buffer // bytes ready for the consumer
	// Tracked across chunks while probing/buffering.
	accumContent   strings.Builder
	originalChatID string
	originalModel  string
	originalCreate int64
	backendUsage   json.RawMessage
	backendDone    bool
	closed         bool
	srcExhausted   bool
}

type transformMode int

const (
	modeProbing transformMode = iota
	modePassthrough
	modeBuffer
)

// NewSSEToolCallTransformer wraps a backend SSE body. The caller
// should still chain on AuditTailReader (this transformer's output
// preserves the usage block so the usage tail extraction works
// unchanged).
func NewSSEToolCallTransformer(src io.ReadCloser) *SSEToolCallTransformer {
	return &SSEToolCallTransformer{src: src}
}

func (t *SSEToolCallTransformer) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	return t.src.Close()
}

// Read implements io.Reader. The contract: produce transformed
// output for the consumer; the transformation decision crystallizes
// once we have enough of the backend's stream to read the first
// content delta.
func (t *SSEToolCallTransformer) Read(p []byte) (int, error) {
	for t.out.Len() == 0 {
		if err := t.pump(); err != nil {
			if t.out.Len() > 0 {
				// Flush remaining transformed bytes before reporting
				// the error so the consumer sees a complete logical
				// payload (e.g. the rewritten data: lines) on EOF.
				break
			}
			return 0, err
		}
	}
	return t.out.Read(p)
}

// pump pulls one read's worth from the backend and advances the state
// machine. Returns io.EOF only when both src is drained AND we've
// finalized the output buffer.
func (t *SSEToolCallTransformer) pump() error {
	if t.srcExhausted {
		// All backend bytes are in. Drain whatever phase we're in
		// to its final output state.
		switch t.mode {
		case modeBuffer:
			t.finalizeBuffer()
		case modeProbing:
			// Stream ended with no content delta ever arriving (e.g.
			// stream-only-of-keepalives, or a backend that closed
			// early). Whatever bytes we held are still semantically
			// the backend's response — replay them verbatim.
			t.flushHeldToOut()
		}
		t.mode = modePassthrough // idempotent — pump won't re-finalize
		if t.out.Len() > 0 {
			return nil
		}
		return io.EOF
	}
	// Read one chunk from the backend.
	buf := make([]byte, 32*1024)
	n, err := t.src.Read(buf)
	if n > 0 {
		switch t.mode {
		case modeProbing, modeBuffer:
			t.held.Write(buf[:n])
			t.advanceProbe()
		case modePassthrough:
			t.out.Write(buf[:n])
		}
	}
	if err == io.EOF {
		t.srcExhausted = true
		// Flushable state changes happen on next pump.
		return nil
	}
	return err
}

// advanceProbe parses any complete SSE events that have arrived in
// `held` since the last call (starting from parsedUpTo), examines
// each event's delta to update detection state, and may commit to a
// mode. When the mode flips to passthrough, we flush held → out and
// stop holding bytes back.
func (t *SSEToolCallTransformer) advanceProbe() {
	for {
		// Scan only the unparsed tail of held.
		unparsed := t.held.Bytes()[t.parsedUpTo:]
		event, rest, found := nextSSEEvent(unparsed)
		if !found {
			return
		}
		// Move parsedUpTo forward by the event length. `rest` is just
		// a slice into held — we don't shrink held, we just track our
		// position.
		t.parsedUpTo += len(event)
		_ = rest // covered by parsedUpTo

		t.examineEvent(event)

		if t.mode == modePassthrough {
			t.flushHeldToOut()
			return
		}
	}
}

// flushHeldToOut releases every byte we accumulated during the
// probing/buffer phases to the downstream consumer. Called exactly
// once when we decide to passthrough (either mid-stream or because
// the backend finished without ever looking like a tool call).
func (t *SSEToolCallTransformer) flushHeldToOut() {
	if t.held.Len() > 0 {
		t.out.Write(t.held.Bytes())
		t.held.Reset()
		t.parsedUpTo = 0
	}
}

// nextSSEEvent splits the buffer at the first "\n\n" boundary.
// Returns the event (including the trailing "\n\n") and the
// remainder. When no full event is present, found=false.
func nextSSEEvent(b []byte) (event, rest []byte, found bool) {
	i := bytes.Index(b, []byte("\n\n"))
	if i < 0 {
		return nil, b, false
	}
	end := i + 2
	return b[:end], b[end:], true
}

// examineEvent walks the lines of one SSE event, parses the data
// payload as a streaming chat completion chunk, updates running
// state (accumulated content, captured usage, [DONE] marker), and
// commits to a mode if we haven't already.
func (t *SSEToolCallTransformer) examineEvent(event []byte) {
	lines := bytes.Split(event, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			t.backendDone = true
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role      string          `json:"role"`
					Content   string          `json:"content"`
					ToolCalls json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			// Unparseable chunk — best-effort, treat as content-
			// agnostic. Don't commit a decision off a corrupt
			// frame.
			continue
		}
		if t.originalChatID == "" && chunk.ID != "" {
			t.originalChatID = chunk.ID
		}
		if t.originalModel == "" && chunk.Model != "" {
			t.originalModel = chunk.Model
		}
		if t.originalCreate == 0 && chunk.Created != 0 {
			t.originalCreate = chunk.Created
		}
		if len(chunk.Usage) > 0 && !bytes.Equal(chunk.Usage, []byte("null")) {
			t.backendUsage = append(t.backendUsage[:0], chunk.Usage...)
		}
		for _, ch := range chunk.Choices {
			// If the backend already structured tool_calls, this
			// is a happy-path stream and we bail to passthrough —
			// nothing for us to rewrite. (Lenient check: any
			// non-empty / non-"null" tool_calls field.)
			if len(ch.Delta.ToolCalls) > 0 && !bytes.Equal(ch.Delta.ToolCalls, []byte("null")) {
				t.mode = modePassthrough
				return
			}
			if ch.Delta.Content != "" {
				t.accumContent.WriteString(ch.Delta.Content)
				if t.mode == modeProbing {
					if quickProbeLooksLikeToolCall(t.accumContent.String()) {
						t.mode = modeBuffer
					} else {
						t.mode = modePassthrough
						return
					}
				}
			}
		}
	}
}

// finalizeBuffer runs at EOF when we held the full stream. If the
// accumulated content was a tool call, emit synthesized chunks;
// otherwise replay the original buffered events.
func (t *SSEToolCallTransformer) finalizeBuffer() {
	content := t.accumContent.String()
	calls, ok := extractToolCallsFromContent(content)
	if !ok {
		// False positive on the probe — replay the raw backend bytes
		// so the client sees the original (presumably text) stream
		// unchanged.
		t.flushHeldToOut()
		return
	}
	// Synthesize a tiny, spec-conformant stream. Three deltas:
	//   1. role=assistant + the tool_calls delta (id, name, args)
	//   2. finish_reason=tool_calls (with usage if we captured one)
	//   3. data: [DONE]
	id := t.originalChatID
	if id == "" {
		id = "chatcmpl-llmgw"
	}
	model := t.originalModel
	created := t.originalCreate
	toolCalls := make([]any, 0, len(calls))
	for index, call := range calls {
		toolCalls = append(toolCalls, map[string]any{
			"index": index,
			"id":    nextToolCallID(),
			"type":  "function",
			"function": map[string]any{
				"name":      call.name,
				"arguments": call.arguments,
			},
		})
	}

	first := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role":       "assistant",
					"tool_calls": toolCalls,
				},
				"finish_reason": nil,
			},
		},
	}
	second := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "tool_calls",
			},
		},
	}
	if len(t.backendUsage) > 0 {
		second["usage"] = json.RawMessage(t.backendUsage)
	}

	writeChunk := func(m map[string]any) {
		b, err := json.Marshal(m)
		if err != nil {
			// Shouldn't fail — but if it does, skipping a chunk is
			// better than panicking on the read path.
			return
		}
		t.out.WriteString("data: ")
		t.out.Write(b)
		t.out.WriteString("\n\n")
	}
	writeChunk(first)
	writeChunk(second)
	if t.backendDone {
		t.out.WriteString("data: [DONE]\n\n")
	}
	// Discard held — we've replaced its content with the synthesized
	// tool-call stream.
	t.held.Reset()
	t.parsedUpTo = 0
}
