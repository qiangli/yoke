package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
	"github.com/qiangli/yoke/pkg/llmgw/responses"
)

type localResponses struct {
	stream bool
	custom map[string]bool
}

func responsesPayload(payload map[string]json.RawMessage) (map[string]json.RawMessage, *localResponses, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	chat, stream, err := responses.ToChat(body)
	if err != nil {
		return nil, nil, err
	}
	data, err := json.Marshal(chat)
	if err != nil {
		return nil, nil, err
	}
	var converted map[string]json.RawMessage
	if err := json.Unmarshal(data, &converted); err != nil {
		return nil, nil, err
	}
	return converted, &localResponses{stream: stream, custom: responses.CustomToolNames(body)}, nil
}

// Like cligw's Responses adapter, collect one chat completion before emitting
// Responses events. The normal local-device admission and proxy still apply.
func (b *Broker) serveResponsesLocal(w http.ResponseWriter, r *http.Request, body []byte, wire *localResponses, model string) (int, int) {
	chatReq := r.Clone(r.Context())
	chatReq.URL.Path, chatReq.URL.RawPath = "/v1/chat/completions", ""
	capture := httptest.NewRecorder()
	b.proxyEngine(capture, chatReq, body)
	result := capture.Result()
	defer result.Body.Close()
	for key, values := range result.Header {
		w.Header()[key] = append([]string(nil), values...)
	}
	// The translated body has a different length and content type.
	w.Header().Del("Content-Length")
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		w.WriteHeader(result.StatusCode)
		_, _ = io.Copy(w, result.Body)
		return 0, 0
	}
	var completion openai.ChatCompletion
	if err := json.NewDecoder(result.Body).Decode(&completion); err != nil {
		writeErr(w, r.URL.Path, http.StatusBadGateway, "invalid chat completion: "+err.Error())
		return 0, 0
	}
	if completion.Model == "" {
		completion.Model = model
	}
	translated, err := responses.FromChatWithCustom(&completion, wire.custom)
	if err != nil {
		writeErr(w, r.URL.Path, http.StatusBadGateway, err.Error())
		return 0, 0
	}
	if wire.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_ = responses.WriteSSE(w, translated)
	} else {
		writeJSON(w, http.StatusOK, translated)
	}
	return translated.Usage.InputTokens, translated.Usage.OutputTokens
}
