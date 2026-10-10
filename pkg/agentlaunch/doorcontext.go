package agentlaunch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
)

// ensureDoorContextBinding creates (idempotently) the sticky binding a
// launch's context ask rides on: POST /v1/sticky with the model and num_ctx —
// the same call `bashy llm sticky create KEY --model M --num-ctx N` makes. A
// variable so tests intercept it; this is the launcher's only HTTP call, and
// nothing else may make it for a launch.
var ensureDoorContextBinding = func(key, model string, numCtx int64) error {
	token, err := door.Token()
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"key":     key,
		"model":   model,
		"options": map[string]any{"num_ctx": float64(numCtx)},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, door.BaseURL()+"/v1/sticky", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("reach the model door at %s: %w (start it: bashy llm up)", door.BaseURL(), err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("%s", e.Error.Message)
		}
		return fmt.Errorf("POST /v1/sticky: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}
