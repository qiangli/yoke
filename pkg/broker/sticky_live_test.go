package broker

// Opt-in live proof only: real subscription CLIs through this tree's
// broker+cligw over the exact sticky flows genie uses. Output is limited to
// status/timing; neither failures nor assertions print provider content.
// Uses subscription seats only (CLI logins); never metered API keys.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/cligw"
)

func TestLiveStickyDoor(t *testing.T) {
	if os.Getenv("CLIGW_LIVE_READINESS") != "1" {
		t.Skip("set CLIGW_LIVE_READINESS=1 for real subscription calls")
	}
	t.Setenv("BASHY_HOME", t.TempDir())
	cli, err := cligw.NewServer(cligw.ServerOptions{Token: "live"})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	go cli.Autoscaler().Run(context.Background())
	versions := &toolVersions{probe: cli.VersionProbeArgv}
	b, err := New(context.Background(), Options{
		Token: "live", CLI: cliAdapter{s: cli, versions: versions}, ToolVersion: versions.get,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b)
	defer srv.Close()

	post := func(t *testing.T, ctx context.Context, path string, body any) (int, string) {
		t.Helper()
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+path, &buf)
		req.Header.Set("Authorization", "Bearer live")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(io.LimitReader(resp.Body, 1<<20))
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(raw.Bytes(), &out)
		got := ""
		if len(out.Choices) == 1 {
			got = strings.TrimSpace(out.Choices[0].Message.Content)
		}
		if resp.StatusCode != 200 {
			snip := raw.String()
			if len(snip) > 300 {
				snip = snip[:300]
			}
			t.Logf("HTTP %d body: %s", resp.StatusCode, snip)
		}
		return resp.StatusCode, got
	}
	turn := func(model, content string, history ...string) map[string]any {
		var m []map[string]string
		for i, text := range append(append([]string{}, history...), content) {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			m = append(m, map[string]string{"role": role, "content": text})
		}
		return map[string]any{"model": model, "messages": m}
	}

	// One turn per seat over an identity binding: the exact genie-door
	// delegation for the three bindings of story 84389f905a84. Natural
	// content: adversarial echo probes trip provider safeguards.
	for _, tc := range []struct{ key, model string }{
		{"live-opus5", "claude-opus5"},
		{"live-gemini", "agy-gemini3.8-flash"},
		{"live-muse", "muse-spark1.3"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			if status, _ := post(t, ctx, "/v1/sticky", StickySpec{Key: tc.key, Model: tc.model}); status != 200 {
				t.Fatalf("create binding: HTTP %d", status)
			}
			start := time.Now()
			status, got := post(t, ctx, "/sticky/"+tc.key+"/v1/chat/completions",
				turn(tc.model, "What is 2+2? Reply with just the number and no other text."))
			if status != 200 || got != "4" {
				t.Fatalf("sticky turn: HTTP %d content=%q", status, got)
			}
			t.Logf("identity turn answered in %s", time.Since(start).Round(time.Millisecond))
		})
	}

	// Two turns on one held worker: the second turn reuses the warm CLI.
	t.Run("worker-two-turns", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		if status, _ := post(t, ctx, "/v1/sticky",
			StickySpec{Key: "live-worker", Model: "claude-opus5", Bind: BindWorker, Reset: ResetNone}); status != 200 {
			t.Fatalf("create worker binding: HTTP %d", status)
		}
		// Natural content: repeating an identical echo probe on consecutive
		// turns trips the provider's content filter (measured 2026-10-08: a
		// reasoning_extraction refusal on turn2), which is provider behavior
		// the door surfaces, not a transport defect.
		first := turn("claude-opus5", "What is 2+2? Reply with just the number and no other text.")
		start := time.Now()
		status, got := post(t, ctx, "/sticky/live-worker/v1/chat/completions", first)
		firstTook := time.Since(start)
		if status != 200 || got != "4" {
			t.Fatalf("worker turn1: HTTP %d content=%q", status, got)
		}
		second := turn("claude-opus5", "And 3+3? Reply with just the number and no other text.",
			"What is 2+2? Reply with just the number and no other text.", "4")
		start = time.Now()
		status, got = post(t, ctx, "/sticky/live-worker/v1/chat/completions", second)
		secondTook := time.Since(start)
		if status != 200 || got != "6" {
			t.Fatalf("worker turn2: HTTP %d content=%q", status, got)
		}
		t.Logf("worker turn1 (spawn) %s, turn2 (warm) %s",
			firstTook.Round(time.Millisecond), secondTook.Round(time.Millisecond))
	})
}
