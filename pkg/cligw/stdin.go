package cligw

import (
	"encoding/json"
	"fmt"
)

func stdinMessage(format, prompt string) ([]byte, error) {
	var v any
	switch format {
	case "claude-stream-json":
		v = map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}}
	case "agy-steps":
		v = map[string]any{"event": "user", "message": map[string]any{"content": prompt}}
	default:
		return nil, fmt.Errorf("cligw: stdin-stream-json has no measured message shape: %q", format)
	}
	body, err := json.Marshal(v)
	return append(body, '\n'), err
}
