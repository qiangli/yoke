package cligw

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt in only: authenticated, real subscription calls. Output is limited to
// status/timing; neither failures nor assertions print provider content.
func TestLiveDoorReadiness(t *testing.T) {
	if os.Getenv("CLIGW_LIVE_READINESS") != "1" {
		t.Skip("set CLIGW_LIVE_READINESS=1 for real subscription calls")
	}
	t.Setenv("BASHY_HOME", t.TempDir())
	server, err := NewServer(ServerOptions{})
	if err != nil {
		t.Fatal("gateway startup failed")
	}
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	for _, agent := range []string{"claude-opus5", "agy-gemini3.8-flash", "muse-spark1.3"} {
		t.Run(agent, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			body, _ := json.Marshal(map[string]any{"model": agent, "messages": []map[string]string{{"role": "user", "content": "Reply with exactly PROBE_OK and nothing else. Do not use tools."}}})
			req, _ := http.NewRequestWithContext(ctx, "POST", httpServer.URL+"/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+server.Token())
			req.Header.Set("Content-Type", "application/json")
			start := time.Now()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed (deadline=%v)", ctx.Err() != nil)
			}
			defer resp.Body.Close()
			var got struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("invalid response; HTTP %d", resp.StatusCode)
			}
			if resp.StatusCode != http.StatusOK || len(got.Choices) != 1 || strings.TrimSpace(got.Choices[0].Message.Content) != "PROBE_OK" {
				t.Fatalf("readiness answer mismatch; HTTP %d", resp.StatusCode)
			}
			t.Logf("authenticated answer verified in %s", time.Since(start).Round(time.Millisecond))
		})
	}
}
