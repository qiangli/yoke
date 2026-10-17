package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A slow local engine (CPU prefill takes longer than the remote 30 s
// fail-fast) must be served, not answered 504. The sub-second delay is a
// short injectable stand-in for a 45 s-class prefill: the point is the
// local path does not apply the remote first-byte bound.
func TestLocalSlowFirstByteIsServed(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.LocalFirstByte = -1 })
	h.eng.chatFn = func(w http.ResponseWriter, _ map[string]any) {
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}
	start := time.Now()
	resp, _ := h.do(http.MethodPost, "/v1/chat/completions", chat("llama3.2:3b"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200: a slow local first byte must be served, not failed fast", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("elapsed=%v, want >= 500ms: the reply must come from the slow engine, not a fast failure", elapsed)
	}
}

// The local path honors its own separate bound: with a tiny injectable
// local bound a silent local engine still fails, but at the local bound —
// proving the 30 s remote default is not what governs the local engine.
func TestLocalFirstByteBoundIsSeparateFromRemote(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.LocalFirstByte = 100 * time.Millisecond })
	h.eng.chatFn = func(w http.ResponseWriter, _ map[string]any) {
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}
	start := time.Now()
	resp, out := h.do(http.MethodPost, "/v1/chat/completions", chat("llama3.2:3b"), nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status=%d, want 504 (body=%v)", resp.StatusCode, out)
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("elapsed=%v, want the local bound, not the engine silence", elapsed)
	}
}

// The default local bound is generous (10 min), not the remote 30 s, and
// the env override renames it per host.
func TestLocalFirstByteDefaultAndEnv(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.b.localFirstByte(); got != DefaultLocalFirstByteTimeout {
		t.Fatalf("default local first-byte=%v, want %v", got, DefaultLocalFirstByteTimeout)
	}
	if DefaultLocalFirstByteTimeout != 10*time.Minute {
		t.Fatalf("default=%v, want 10m", DefaultLocalFirstByteTimeout)
	}
	t.Setenv(LocalFirstByteEnv, "2m")
	if got := h.b.localFirstByte(); got != 2*time.Minute {
		t.Fatalf("env local first-byte=%v, want 2m", got)
	}
	t.Setenv(LocalFirstByteEnv, "0")
	if got := h.b.localFirstByte(); got >= 0 {
		t.Fatalf("env 0 local first-byte=%v, want disabled (negative)", got)
	}
}

// exitTestEngine is an engine whose process death the door can see.
type exitTestEngine struct {
	url     string
	exited  chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (e *exitTestEngine) Start(context.Context) (string, error) { return e.url, nil }
func (e *exitTestEngine) Stop(context.Context) error            { return nil }
func (e *exitTestEngine) Exited() <-chan struct{}               { return e.exited }

// An engine process that dies mid-request must fail fast with a classified
// error, not wait out any first-byte bound.
func TestLocalEngineExitFailsFast(t *testing.T) {
	exited := make(chan struct{})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"name":"llama3.2:3b"}]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
		case "/v1/chat/completions":
			once.Do(func() { close(entered) })
			// A dead engine's in-flight work terminates; release models
			// that (like closing the engine sockets) so the servers drain.
			select {
			case <-release:
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	eng := &exitTestEngine{url: srv.URL, exited: exited, entered: entered}

	h := newHarness(t, func(o *Options) {
		o.Engine = eng
		o.LocalFirstByte = -1 // no bound at all: only the exit watch can end this fast
	})

	type result struct {
		status int
		body   string
	}
	done := make(chan result, 1)
	go func() {
		data, _ := json.Marshal(chat("llama3.2:3b"))
		req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+testToken)
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			done <- result{status: -1, body: err.Error()}
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		done <- result{status: resp.StatusCode, body: string(raw)}
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("engine never received the proxied request")
	}
	start := time.Now()
	close(exited) // the engine process dies mid-request
	select {
	case r := <-done:
		close(release)
		srv.CloseClientConnections()
		elapsed := time.Since(start)
		if r.status != http.StatusBadGateway {
			t.Fatalf("status=%d, want 502 (body=%s)", r.status, r.body)
		}
		if !strings.Contains(r.body, "engine") {
			t.Fatalf("body=%s, want it to name the engine", r.body)
		}
		if elapsed >= 10*time.Second {
			t.Fatalf("elapsed=%v, want a fast classified failure, not the bound", elapsed)
		}
	case <-time.After(14 * time.Second):
		close(release)
		srv.CloseClientConnections()
		t.Fatal("no classified error after the engine exited")
	}
}
