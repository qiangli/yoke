package cligw

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/responses"
)

func TestResponsesRouteAndSSE(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})
	for _, stream := range []bool{false, true} {
		body := `{"model":"L4","input":[{"role":"user","content":"say ok"}],"instructions":"Be concise","reasoning":{"effort":"high"},"store":false}`
		if stream {
			body = strings.TrimSuffix(body, "}") + `,"stream":true}`
		}
		resp := ts.do(t, http.MethodPost, "/v1/responses", ts.Token(), body)
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream=%v: %s: %s", stream, resp.Status, data)
		}
		if !strings.HasPrefix(resp.Header.Get(RoutedHeader), "agent=warm-four, band=L4") {
			t.Fatalf("routed header = %q", resp.Header.Get(RoutedHeader))
		}
		if stream {
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
				t.Fatalf("content type = %q", ct)
			}
			for _, event := range []string{"response.created", "response.output_text.delta", "response.output_item.done", "response.completed"} {
				if !strings.Contains(string(data), "event: "+event) {
					t.Fatalf("missing %s: %s", event, data)
				}
			}
		} else {
			var result responses.Response
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Object != "response" || result.Status != "completed" || len(result.Output) == 0 || result.Output[0].Content[0]["type"] != "output_text" {
				t.Fatalf("response = %s", data)
			}
		}
	}
}

func TestResponsesRejectState(t *testing.T) {
	ts := newTestServer(t, nil)
	for _, body := range []string{
		`{"model":"L4","input":"hi","previous_response_id":"resp_1"}`,
		`{"model":"L4","input":"hi","store":true}`,
	} {
		resp := ts.do(t, http.MethodPost, "/v1/responses", ts.Token(), body)
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "server-side response state") {
			t.Fatalf("status=%s body=%s", resp.Status, data)
		}
	}
	if len(ts.Health().Autoscale.Pools) != 0 {
		t.Fatal("rejected requests created a pool")
	}
}
