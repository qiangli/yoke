package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/qiangli/yoke/pkg/llmgw/anthropic"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

const (
	AnthropicMessagesPath = "/anthropic/v1/messages"
	MessagesPath          = "/v1/messages"
)

type inferenceCodec struct {
	upstreamPath string
	decode       func([]byte) ([]byte, string, bool, error)
	prepare      func(*http.Request, bool)
	adapt        func(*http.Response, bool) error
	anthropic    bool
}

func openAICodec(path string, peek func([]byte) (string, error)) inferenceCodec {
	return inferenceCodec{
		upstreamPath: path,
		decode: func(body []byte) ([]byte, string, bool, error) {
			model, err := peek(body)
			if err != nil {
				return nil, "", false, err
			}
			var env struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal(body, &env)
			return body, model, env.Stream, nil
		},
	}
}

func anthropicCodec() inferenceCodec {
	return inferenceCodec{
		upstreamPath: ChatCompletionsPath,
		anthropic:    true,
		decode: func(body []byte) ([]byte, string, bool, error) {
			var req anthropic.MessagesRequest
			if err := json.Unmarshal(body, &req); err != nil {
				return nil, "", false, err
			}
			canonical, err := anthropic.ToOpenAI(&req)
			if err != nil {
				return nil, "", false, err
			}
			data, err := json.Marshal(canonical)
			return data, strings.TrimSpace(req.Model), req.Stream, err
		},
		prepare: func(r *http.Request, stream bool) {
			r.URL.Path, r.URL.RawPath = ChatCompletionsPath, ""
			if stream {
				r.Header.Set("Accept", "text/event-stream")
			}
		},
		adapt: adaptAnthropicResponse,
	}
}

func adaptAnthropicResponse(resp *http.Response, stream bool) error {
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBody))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		msg := openAIErrorMessage(body)
		data, _ := json.Marshal(anthropic.ErrorResponse{Type: "error", Error: anthropic.ErrorBody{Type: anthropicErrorType(resp.StatusCode), Message: msg}})
		resp.Body = io.NopCloser(bytes.NewReader(data))
		resp.ContentLength = int64(len(data))
		resp.Header.Set("Content-Type", "application/json; charset=utf-8")
		resp.Header.Set("Content-Length", fmt.Sprint(len(data)))
		return nil
	}
	if stream {
		resp.Body = anthropic.NewStreamReadCloser(resp.Body)
		resp.Header.Set("Content-Type", "text/event-stream")
		resp.Header.Set("Cache-Control", "no-cache")
		return nil
	}
	var comp openai.ChatCompletion
	if err := json.NewDecoder(resp.Body).Decode(&comp); err != nil {
		_ = resp.Body.Close()
		return fmt.Errorf("decode OpenAI completion: %w", err)
	}
	_ = resp.Body.Close()
	data, err := json.Marshal(anthropic.FromOpenAI(&comp))
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp.Header.Set("Content-Length", fmt.Sprint(len(data)))
	return nil
}

func (c inferenceCodec) writeError(w http.ResponseWriter, status int, value any) {
	if !c.anthropic {
		writeJSON(w, status, value)
		return
	}
	msg := errorMessage(value)
	writeJSON(w, status, anthropic.ErrorResponse{Type: "error", Error: anthropic.ErrorBody{Type: anthropicErrorType(status), Message: msg}})
}

func writeCatalogUnavailable(w http.ResponseWriter, codec inferenceCodec, detail string) {
	message := "model catalog unavailable"
	if detail = strings.TrimSpace(detail); detail != "" {
		message += ": " + detail
	}
	if codec.anthropic {
		writeJSON(w, http.StatusServiceUnavailable, anthropic.ErrorResponse{
			Type:  "error",
			Error: anthropic.ErrorBody{Type: "catalog_unavailable", Message: message},
		})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "catalog_unavailable",
		},
	})
}

func errorMessage(value any) string {
	switch v := value.(type) {
	case map[string]any:
		if msg, ok := v["error"].(string); ok {
			return msg
		}
	case string:
		return v
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func openAIErrorMessage(body []byte) string {
	var env struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil {
		switch e := env.Error.(type) {
		case string:
			if strings.TrimSpace(e) != "" {
				return e
			}
		case map[string]any:
			if msg, ok := e["message"].(string); ok && strings.TrimSpace(msg) != "" {
				return msg
			}
		}
	}
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return msg
	}
	return "upstream request failed"
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}
