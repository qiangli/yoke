package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// TestAPIModelPassthrough: a fleet API model is forwarded unchanged (tools
// and all) to its base_url with the host's key; only the model is renamed to
// the provider id, and the client's door credential never reaches upstream.
func TestAPIModelPassthrough(t *testing.T) {
	var got struct {
		path, auth string
		body       map[string]any
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &got.body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"edit","arguments":"{}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))
	defer up.Close()
	minted := 0
	h := newHarness(t, func(o *Options) {
		o.API = &FleetAPI{
			lookup: func(name string) (fleet.Model, bool) {
				if name != "bedrock-glm-5" {
					return fleet.Model{}, false
				}
				return fleet.Model{Name: name, Kind: fleet.ModelKindAPI, BaseURL: up.URL + "/openai/v1", APIKeyRef: "aws-bedrock", UpstreamID: "zai.glm-5"}, true
			},
			resolveKey: func(ref string) (string, bool) { minted++; return "host-key", ref == "aws-bedrock" },
		}
	})
	req := chat("bedrock-glm-5")
	req["tools"] = []map[string]any{{"type": "function", "function": map[string]any{"name": "edit"}}}
	for i := 0; i < 2; i++ {
		resp, out := h.do("POST", "/v1/chat/completions", req, nil)
		if resp.StatusCode != 200 || resp.Header.Get(BackendHeader) != BackendAPI {
			t.Fatalf("status %d backend %q: %v", resp.StatusCode, resp.Header.Get(BackendHeader), out)
		}
		msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["tool_calls"] == nil {
			t.Fatalf("tool calls lost: %v", out)
		}
	}
	if got.path != "/openai/v1/chat/completions" || got.auth != "Bearer host-key" {
		t.Fatalf("upstream got path %q auth %q", got.path, got.auth)
	}
	if got.body["model"] != "zai.glm-5" || got.body["tools"] == nil {
		t.Fatalf("upstream body: %v", got.body)
	}
	if minted != 1 {
		t.Fatalf("key resolved %d times, want 1 (cached)", minted)
	}
	// A model that is not an API model keeps the existing routing.
	if resp, _ := h.do("POST", "/v1/chat/completions", chat("L4"), nil); resp.Header.Get(BackendHeader) == BackendAPI {
		t.Fatal("non-API model was passed through")
	}
}
