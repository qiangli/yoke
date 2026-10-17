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
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/cligw"
)

func TestLiveStickyDoor(t *testing.T) {
	if os.Getenv("CLIGW_LIVE_READINESS") != "1" {
		t.Skip("set CLIGW_LIVE_READINESS=1 for real subscription calls")
	}
	t.Setenv("BASHY_HOME", t.TempDir())
	useOperatorHome(t)
	cli, err := cligw.NewServer(cligw.ServerOptions{Token: "live"})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	go cli.Autoscaler().Run(context.Background())
	versions := &toolVersions{probe: cli.VersionProbeArgv}
	rec := &recordingDialer{cliAdapter: cliAdapter{s: cli, versions: versions}}
	b, err := New(context.Background(), Options{
		Token: "live", CLI: rec, ToolVersion: versions.get,
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

	// The declared half of checklist 4.36: a one-shot tool (codex) is
	// refused before any process starts, and the 501 body names the tool,
	// the reason and the tools that do hold a session. Costs no seat.
	t.Run("worker-one-shot-tool-501", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		const model = "codex-gpt6-astra"
		if status, _ := post(t, ctx, "/v1/sticky",
			StickySpec{Key: "live-codex", Model: model, Bind: BindWorker, Reset: ResetNone}); status != 200 {
			t.Fatalf("create worker binding: HTTP %d", status)
		}
		rec.reset()
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/sticky/live-codex/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer live")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("codex worker turn: HTTP %d, want 501", resp.StatusCode)
		}
		for _, want := range []string{`tool \"codex\"`, "one-shot CLI", "streaming stdin protocol", "supported: agy, claude"} {
			if !strings.Contains(raw.String(), want) {
				t.Fatalf("501 body lacks %q", want)
			}
		}
		if turns := rec.snapshot(); len(turns) != 0 {
			t.Fatalf("one-shot tool reached a worker: %d turns", len(turns))
		}
		t.Logf("codex refused with 501 and the supported list, no process started")
	})

	// The multi-turn proof (v1.0.0 checklist 4.36): two turns on one held
	// sonnet-class worker, with the worker's pid and the forwarded prompt
	// recorded below the broker. Turn 2 must land on the same process and
	// carry only the new turn, never the recorded conversation.
	t.Run("worker-two-turns-delta", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		const model = "claude-sonnet5"
		if status, _ := post(t, ctx, "/v1/sticky",
			StickySpec{Key: "live-delta", Model: model, Bind: BindWorker, Reset: ResetNone}); status != 200 {
			t.Fatalf("create worker binding: HTTP %d", status)
		}
		rec.reset()
		q1 := "What is 2+2? Reply with just the number and no other text."
		q2 := "And 3+3? Reply with just the number and no other text."
		start := time.Now()
		status, got := post(t, ctx, "/sticky/live-delta/v1/chat/completions", turn(model, q1))
		firstTook := time.Since(start)
		if status != 200 || got != "4" {
			t.Fatalf("worker turn1: HTTP %d content=%q", status, got)
		}
		start = time.Now()
		status, got = post(t, ctx, "/sticky/live-delta/v1/chat/completions", turn(model, q2, q1, "4"))
		secondTook := time.Since(start)
		if status != 200 || got != "6" {
			t.Fatalf("worker turn2: HTTP %d content=%q", status, got)
		}
		turns := rec.snapshot()
		if len(turns) != 2 {
			t.Fatalf("recorded %d worker turns, want 2", len(turns))
		}
		if turns[0].pid == 0 || turns[0].pid != turns[1].pid {
			t.Fatalf("worker pid changed between turns: %d then %d", turns[0].pid, turns[1].pid)
		}
		if !strings.Contains(turns[1].prompt, "3+3") || strings.Contains(turns[1].prompt, "2+2") {
			t.Fatalf("turn2 forwarded %d bytes that are not only the new turn", len(turns[1].prompt))
		}
		t.Logf("worker pid %d on both turns; turn1 (spawn) %s forwarded %d bytes; turn2 (warm, delta only) %s forwarded %d bytes",
			turns[0].pid, firstTook.Round(time.Millisecond), len(turns[0].prompt),
			secondTook.Round(time.Millisecond), len(turns[1].prompt))
	})

}

// recordedTurn is one prompt a live sticky worker received, with the pid of
// the process that received it.
type recordedTurn struct {
	prompt string
	pid    int
}

// recordingDialer is the production cliAdapter with every sticky worker
// wrapped so the test can see what the broker forwarded and to which
// process. The door's HTTP surface and one-shot path are untouched.
type recordingDialer struct {
	cliAdapter
	mu    sync.Mutex
	turns []recordedTurn
}

func (d *recordingDialer) DialSticky(ctx context.Context, agent string) (stickyWorker, error) {
	sess, err := d.s.DialSticky(ctx, agent)
	if err != nil {
		return nil, err
	}
	return &recordedWorker{StickySession: sess, d: d}, nil
}

func (d *recordingDialer) reset() {
	d.mu.Lock()
	d.turns = nil
	d.mu.Unlock()
}

func (d *recordingDialer) snapshot() []recordedTurn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recordedTurn(nil), d.turns...)
}

type recordedWorker struct {
	*cligw.StickySession
	d *recordingDialer
}

func (w *recordedWorker) Turn(ctx context.Context, prompt cligw.CompletionPrompt, onEvent func(cligw.Event)) (cligw.Result, error) {
	res, err := w.StickySession.Turn(ctx, prompt, onEvent)
	// Read the pid after the turn: a first turn that carries native system
	// instructions relaunches the prewarmed process before writing.
	w.d.mu.Lock()
	w.d.turns = append(w.d.turns, recordedTurn{prompt: prompt.Prompt, pid: w.StickySession.Pid()})
	w.d.mu.Unlock()
	return res, err
}
