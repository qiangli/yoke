package responses

import (
	"encoding/json"
	"fmt"
	"io"
)

// WriteSSE emits a completed response as Responses events. The inference has
// already finished, so deltas are simulated from the final output.
func WriteSSE(w io.Writer, resp *Response) error {
	seq := 0
	emit := func(kind string, fields map[string]any) error {
		seq++
		fields["type"] = kind
		fields["sequence_number"] = seq
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		return err
	}
	created := *resp
	created.Status = "in_progress"
	created.Output = []OutputItem{}
	if err := emit("response.created", map[string]any{"response": &created}); err != nil {
		return err
	}
	for i, item := range resp.Output {
		if err := emit("response.output_item.added", map[string]any{"output_index": i, "item": map[string]any{"id": item.ID, "type": item.Type, "status": "in_progress"}}); err != nil {
			return err
		}
		if item.Type == "message" {
			for j, part := range item.Content {
				if err := emit("response.content_part.added", map[string]any{"output_index": i, "content_index": j, "part": map[string]string{"type": "output_text", "text": ""}}); err != nil {
					return err
				}
				if err := emit("response.output_text.delta", map[string]any{"output_index": i, "content_index": j, "delta": part["text"]}); err != nil {
					return err
				}
				if err := emit("response.output_text.done", map[string]any{"output_index": i, "content_index": j, "text": part["text"]}); err != nil {
					return err
				}
				if err := emit("response.content_part.done", map[string]any{"output_index": i, "content_index": j, "part": part}); err != nil {
					return err
				}
			}
		} else if item.Type == "function_call" {
			if err := emit("response.function_call_arguments.delta", map[string]any{"output_index": i, "delta": item.Arguments}); err != nil {
				return err
			}
			if err := emit("response.function_call_arguments.done", map[string]any{"output_index": i, "arguments": item.Arguments}); err != nil {
				return err
			}
		}
		if err := emit("response.output_item.done", map[string]any{"output_index": i, "item": item}); err != nil {
			return err
		}
	}
	return emit("response.completed", map[string]any{"response": resp})
}
