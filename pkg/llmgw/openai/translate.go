package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ServeTranslated handles a non-streaming provider request and writes an
// OpenAI-compatible JSON response or pseudo-stream. It always owns the response.
func ServeTranslated(w http.ResponseWriter, r *http.Request, baseURL, path, apiKey, provider, upstreamID string, body []byte) bool {
	if path != "/v1/chat/completions" {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":    "only chat completions are gateway-translated for provider " + provider,
			"provider": provider,
		})
		return true
	}

	var oreq ChatRequest
	if err := json.Unmarshal(body, &oreq); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parse chat body: " + err.Error()})
		return true
	}

	var completion *ChatCompletion
	var status int
	var vendorErr []byte
	var err error
	switch provider {
	case "anthropic":
		completion, status, vendorErr, err = CallAnthropic(r.Context(), http.DefaultClient, baseURL, apiKey, upstreamID, &oreq)
	case "gemini":
		completion, status, vendorErr, err = CallGemini(r.Context(), http.DefaultClient, baseURL, apiKey, upstreamID, &oreq)
	default:
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":    "gateway translation for provider " + provider + " is not available",
			"provider": provider,
		})
		return true
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return true
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":    "commercial upstream error: " + err.Error(),
			"provider": provider,
		})
		return true
	}
	if completion == nil {
		code := status
		if code < 400 {
			code = http.StatusBadGateway
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(NormalizeVendorError(provider, vendorErr))
		return true
	}

	if oreq.Stream {
		EmitStream(w, completion)
	} else {
		writeJSON(w, http.StatusOK, completion)
	}
	return true
}

// CallAnthropic translates an OpenAI request to the Anthropic Messages API and
// translates a successful response back. Non-2xx bodies are returned unchanged.
func CallAnthropic(ctx context.Context, client *http.Client, baseURL, apiKey, model string, oreq *ChatRequest) (*ChatCompletion, int, []byte, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	areq := struct {
		Model       string    `json:"model"`
		MaxTokens   int       `json:"max_tokens"`
		System      string    `json:"system,omitempty"`
		Messages    []message `json:"messages"`
		Temperature *float64  `json:"temperature,omitempty"`
		Stream      bool      `json:"stream"`
	}{Model: model, MaxTokens: oreq.EffectiveMaxTokens(), Temperature: oreq.Temperature, Stream: false}

	var systemParts []string
	for _, m := range oreq.Messages {
		text := FlattenContent(m.Content)
		switch m.Role {
		case "system":
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user", "assistant":
			areq.Messages = append(areq.Messages, message{Role: m.Role, Content: text})
		default:
			areq.Messages = append(areq.Messages, message{Role: "user", Content: text})
		}
	}
	areq.System = strings.Join(systemParts, "\n\n")

	reqBody, _ := json.Marshal(areq)
	url := joinURL(baseURL, "messages")
	headers := map[string]string{
		"x-api-key":         apiKey,
		"anthropic-version": "2023-06-01",
		"content-type":      "application/json",
	}
	status, respBody, err := doUpstream(ctx, client, url, headers, reqBody)
	if err != nil {
		return nil, 0, nil, err
	}
	if status < 200 || status >= 300 {
		return nil, status, respBody, nil
	}

	var aresp struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &aresp); err != nil {
		return nil, 0, nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	var text strings.Builder
	for _, block := range aresp.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	finish := MapAnthropicStop(aresp.StopReason)
	return &ChatCompletion{
		ID:      orDefault(aresp.ID, "chatcmpl-anthropic"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   oreq.Model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      &ChatResponseMessage{Role: "assistant", Content: text.String()},
			FinishReason: &finish,
		}},
		Usage: ChatUsage{
			PromptTokens:     aresp.Usage.InputTokens,
			CompletionTokens: aresp.Usage.OutputTokens,
			TotalTokens:      aresp.Usage.InputTokens + aresp.Usage.OutputTokens,
		},
	}, status, nil, nil
}

// MapAnthropicStop maps a provider stop reason to the OpenAI wire value.
func MapAnthropicStop(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// CallGemini translates an OpenAI request to generateContent and translates a
// successful response back. Non-2xx bodies are returned unchanged.
func CallGemini(ctx context.Context, client *http.Client, baseURL, apiKey, model string, oreq *ChatRequest) (*ChatCompletion, int, []byte, error) {
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	greq := struct {
		Contents          []content `json:"contents"`
		SystemInstruction *content  `json:"systemInstruction,omitempty"`
		GenerationConfig  struct {
			MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
			Temperature     *float64 `json:"temperature,omitempty"`
		} `json:"generationConfig"`
	}{}
	greq.GenerationConfig.MaxOutputTokens = oreq.EffectiveMaxTokens()
	greq.GenerationConfig.Temperature = oreq.Temperature

	var systemParts []string
	for _, m := range oreq.Messages {
		text := FlattenContent(m.Content)
		switch m.Role {
		case "system":
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "assistant":
			greq.Contents = append(greq.Contents, content{Role: "model", Parts: []part{{Text: text}}})
		default:
			greq.Contents = append(greq.Contents, content{Role: "user", Parts: []part{{Text: text}}})
		}
	}
	if len(systemParts) > 0 {
		greq.SystemInstruction = &content{Parts: []part{{Text: strings.Join(systemParts, "\n\n")}}}
	}

	reqBody, _ := json.Marshal(greq)
	url := strings.TrimRight(baseURL, "/") + "/models/" + model + ":generateContent"
	headers := map[string]string{
		"x-goog-api-key": apiKey,
		"content-type":   "application/json",
	}
	status, respBody, err := doUpstream(ctx, client, url, headers, reqBody)
	if err != nil {
		return nil, 0, nil, err
	}
	if status < 200 || status >= 300 {
		return nil, status, respBody, nil
	}

	var gresp struct {
		Candidates []struct {
			Content struct {
				Parts []part `json:"parts"`
				Role  string `json:"role"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(respBody, &gresp); err != nil {
		return nil, 0, nil, fmt.Errorf("decode gemini response: %w", err)
	}
	var text strings.Builder
	finish := "stop"
	if len(gresp.Candidates) > 0 {
		for _, p := range gresp.Candidates[0].Content.Parts {
			text.WriteString(p.Text)
		}
		finish = MapGeminiFinish(gresp.Candidates[0].FinishReason)
	}
	return &ChatCompletion{
		ID:      "chatcmpl-gemini",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   oreq.Model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      &ChatResponseMessage{Role: "assistant", Content: text.String()},
			FinishReason: &finish,
		}},
		Usage: ChatUsage{
			PromptTokens:     gresp.UsageMetadata.PromptTokenCount,
			CompletionTokens: gresp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      gresp.UsageMetadata.TotalTokenCount,
		},
	}, status, nil, nil
}

// MapGeminiFinish maps a provider finish reason to the OpenAI wire value.
func MapGeminiFinish(reason string) string {
	switch strings.ToUpper(reason) {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	default:
		return "stop"
	}
}

// NormalizeVendorError wraps a provider error in an OpenAI-compatible envelope.
func NormalizeVendorError(provider string, vendorBody []byte) []byte {
	msg := strings.TrimSpace(string(vendorBody))
	if msg == "" {
		msg = "commercial provider " + provider + " returned an error"
	}
	out, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message":  msg,
			"type":     "upstream_error",
			"provider": provider,
		},
	})
	return out
}

func doUpstream(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "llmgw-openai/1")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBody, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, _ := json.Marshal(value)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
