package openai

import (
	"encoding/json"
	"net/http"
)

// EmitStream replays a completed response as one content chunk, one finish
// chunk, and the OpenAI [DONE] marker.
func EmitStream(w http.ResponseWriter, comp *ChatCompletion) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	content := ""
	var finish *string
	if len(comp.Choices) > 0 {
		content = comp.Choices[0].Message.Content
		finish = comp.Choices[0].FinishReason
	}
	flush, _ := w.(http.Flusher)
	writeChunk := func(delta *ChatResponseMessage, fin *string) {
		chunk := ChatCompletion{
			ID:      comp.ID,
			Object:  "chat.completion.chunk",
			Created: comp.Created,
			Model:   comp.Model,
			Choices: []ChatChoice{{Index: 0, Delta: delta, FinishReason: fin}},
		}
		body, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(body)
		_, _ = w.Write([]byte("\n\n"))
		if flush != nil {
			flush.Flush()
		}
	}
	writeChunk(&ChatResponseMessage{Role: "assistant", Content: content}, nil)
	writeChunk(&ChatResponseMessage{}, finish)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flush != nil {
		flush.Flush()
	}
}
