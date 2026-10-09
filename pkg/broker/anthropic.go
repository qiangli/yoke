package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/qiangli/yoke/pkg/llmgw/anthropic"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

// The local engine speaks the OpenAI and Ollama dialects only, so an
// Anthropic Messages request for a local model is translated to the engine's
// OpenAI-compatible chat API and the answer translated back. The request and
// response mapping is the one cligw's Anthropic surface uses (llmgw/anthropic).

func isAnthropicPath(p string) bool { return strings.HasSuffix(p, "/v1/messages") }

// anthropicToOpenAIPayload rewrites an Anthropic Messages payload into the
// OpenAI chat payload the engine serves.
func anthropicToOpenAIPayload(payload map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var req anthropic.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("invalid Anthropic Messages request: %w", err)
	}
	conv, err := anthropic.ToOpenAI(&req)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(conv)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if v, ok := payload["top_p"]; ok {
		out["top_p"] = v
	}
	if conv.Stream {
		out["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}
	return out, nil
}

// anthropicErrType is the Anthropic error.type for an HTTP status.
func anthropicErrType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusConflict, http.StatusRequestEntityTooLarge:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusGatewayTimeout:
		return "timeout_error"
	case 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

func writeAnthropicErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, anthropic.ErrorResponse{Type: "error", Error: anthropic.ErrorBody{Type: anthropicErrType(status), Message: msg}})
}

// engineErrMessage extracts the message from an engine error body, which is
// either {"error":{"message":...}} or {"error":"..."}.
func engineErrMessage(status int, body []byte) string {
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil && len(v.Error) > 0 {
		var s string
		if json.Unmarshal(v.Error, &s) == nil && s != "" {
			return s
		}
		var o struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(v.Error, &o) == nil && o.Message != "" {
			return o.Message
		}
	}
	if t := strings.TrimSpace(string(body)); t != "" {
		return t
	}
	return fmt.Sprintf("local engine answered %d", status)
}

// flushWriter flushes after every write so SSE events reach the client as the
// engine produces them.
type flushWriter struct {
	w http.ResponseWriter
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// tailWriter keeps the last max bytes written, for token accounting.
type tailWriter struct {
	buf []byte
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// serveAnthropicLocal sends body (an OpenAI chat request) to the engine and
// answers w in the Anthropic Messages dialect. It returns the token counts the
// engine reported.
func (b *Broker) serveAnthropicLocal(ctx context.Context, w http.ResponseWriter, body []byte, stream bool, clientModel string) (prompt, output int) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(b.engine.base, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		writeAnthropicErr(w, http.StatusInternalServerError, err.Error())
		return 0, 0
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadGateway, "local engine: "+err.Error())
		return 0, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		writeAnthropicErr(w, resp.StatusCode, engineErrMessage(resp.StatusCode, data))
		return 0, 0
	}

	if !stream {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			writeAnthropicErr(w, http.StatusBadGateway, "local engine: "+err.Error())
			return 0, 0
		}
		var comp openai.ChatCompletion
		if err := json.Unmarshal(data, &comp); err != nil {
			writeAnthropicErr(w, http.StatusBadGateway, "local engine: undecodable chat completion: "+err.Error())
			return 0, 0
		}
		msg := anthropic.FromOpenAI(&comp)
		if msg.Model == "" {
			msg.Model = clientModel
		}
		if msg.ID == "" {
			msg.ID = "msg_bashy_local"
		}
		writeJSON(w, http.StatusOK, msg)
		return msg.Usage.InputTokens, msg.Usage.OutputTokens
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	tail := &tailWriter{max: 64 << 10}
	if err := anthropic.ConvertStream(io.TeeReader(resp.Body, tail), flushWriter{w}); err != nil && ctx.Err() == nil {
		data, _ := json.Marshal(anthropic.ErrorEvent{Type: "error", Error: anthropic.ErrorBody{Type: "api_error", Message: "local engine: " + err.Error()}})
		_, _ = fmt.Fprintf(flushWriter{w}, "event: error\ndata: %s\n\n", data)
	}
	return parseTokens(tail.buf)
}
