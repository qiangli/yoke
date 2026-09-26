package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
	"github.com/qiangli/yoke/pkg/llmgw/resolve"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

// fakeCatalog is a static resolve.Catalog over a row slice.
type fakeCatalog struct {
	rows    []resolve.ModelRow
	err     error
	aliases map[string]struct {
		class   int
		domains []string
	}
}

func (c *fakeCatalog) Rows(context.Context, string) []resolve.ModelRow { return c.rows }

func (c *fakeCatalog) RowsErr(context.Context, string) ([]resolve.ModelRow, error) {
	return c.rows, c.err
}

func (c *fakeCatalog) Alias(_ context.Context, name string) (int, []string, bool) {
	a, ok := c.aliases[name]
	if !ok {
		return 0, nil, false
	}
	return a.class, a.domains, true
}

// upstream is an httptest server standing in for one backend, plus the
// Backend that proxies to it.
type upstream struct {
	name    string
	server  *httptest.Server
	backend Backend
	hits    atomic.Int64
	lastURL atomic.Value
	lastHdr atomic.Value
}

func newUpstream(t *testing.T, name string, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{name: name}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.lastURL.Store(r.URL.Path)
		u.lastHdr.Store(r.Header.Clone())
		h(w, r)
	}))
	t.Cleanup(u.server.Close)
	target, err := url.Parse(u.server.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	u.backend = ReverseProxyBackend(name, target, nil)
	return u
}

func (u *upstream) path() string {
	v, _ := u.lastURL.Load().(string)
	return v
}

// echoJSON is the common upstream: a 200 chat-completion with usage.
func echoJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":`+
		`[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
}

// testEnv wires a gateway over a set of upstreams.
type testEnv struct {
	handler   http.Handler
	cfg       *Config
	upstreams map[string]*upstream
}

func newEnv(t *testing.T, rows []resolve.ModelRow, ups []*upstream, tweak func(*Config)) *testEnv {
	t.Helper()
	byName := make(map[string]*upstream, len(ups))
	for _, u := range ups {
		byName[u.name] = u
	}
	cfg := Config{
		Catalog: &fakeCatalog{rows: rows},
		Backend: func(name string) (Backend, bool) {
			u, ok := byName[name]
			if !ok {
				return nil, false
			}
			return u.backend, true
		},
		Backends: func() []Backend {
			out := make([]Backend, 0, len(ups))
			for _, u := range ups {
				out = append(out, u.backend)
			}
			return out
		},
		Authorize: func(*http.Request) (string, int, error) { return "alice", 100, nil },
		// Isolate the scheduler state per test so admission counters
		// and breaker trips do not leak between cases.
		Admitter: sched.NewAdmitter(),
		Slots:    sched.NewSlotTable(),
		Breaker:  sched.NewBreaker(),
		Affinity: sched.NewAffinityCache(),
		History:  sched.NewHistoryBuffer(),
		Jobs:     sched.NewJobTable(),
		Metrics:  sched.NewMetrics(nil),
		Capacity: NewCapacityCache(DefaultCapacityTTL),
		Loaded:   NewLoadedCache(DefaultLoadedTTL, DefaultLoadedPollInterval),
	}
	if tweak != nil {
		tweak(&cfg)
	}
	return &testEnv{handler: New(cfg), cfg: &cfg, upstreams: byName}
}

func (e *testEnv) do(t *testing.T, method, path string, payload any, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if payload != nil {
		enc, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		body = bytes.NewReader(enc)
	}
	r := httptest.NewRequest(method, path, body)
	if payload != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return w
}

func chatPayload(model string) map[string]any {
	return map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}
}

func row(name, backend string) resolve.ModelRow {
	return resolve.ModelRow{
		Name:         name,
		Backend:      backend,
		Capabilities: []string{"completion", "tools"},
		ContextLen:   8192,
		Class:        resolve.TierL2,
		Domains:      []string{resolve.DomainGeneral},
		UpdatedAt:    time.Unix(1700000000, 0),
	}
}

func TestChatCompletions_RoutesToBackend(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, nil)

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := up.hits.Load(); got != 1 {
		t.Errorf("upstream hits=%d, want 1", got)
	}
	if got := up.path(); got != ChatCompletionsPath {
		t.Errorf("upstream path=%q, want %q", got, ChatCompletionsPath)
	}
	if got := w.Header().Get(resolve.ResolvedModelHeader); got != "llama3.2:1b" {
		t.Errorf("%s=%q", resolve.ResolvedModelHeader, got)
	}
	if got := w.Header().Get(resolve.ResolvedReasonHeader); got != resolve.ReasonExactMatch {
		t.Errorf("%s=%q, want %q", resolve.ResolvedReasonHeader, got, resolve.ReasonExactMatch)
	}
	if got := w.Header().Get(sched.JobIDHeader); got == "" {
		t.Errorf("%s not stamped", sched.JobIDHeader)
	}
	if got := w.Header().Get(sched.QueuePositionHeader); got == "" {
		t.Errorf("%s not stamped", sched.QueuePositionHeader)
	}
	if got := w.Header().Get(PriorityHeader); got != "100" {
		t.Errorf("%s=%q, want 100", PriorityHeader, got)
	}
}

func TestChatCompletions_AliasPrefix(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, nil)

	w := env.do(t, http.MethodPost, AliasPrefix+ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := up.hits.Load(); got != 1 {
		t.Errorf("upstream hits=%d, want 1", got)
	}
}

func TestEmbeddings_RoutesToBackend(t *testing.T) {
	up := newUpstream(t, "alpha", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
	})
	env := newEnv(t, []resolve.ModelRow{row("nomic-embed", "alpha")}, []*upstream{up}, nil)

	w := env.do(t, http.MethodPost, EmbeddingsPath,
		map[string]any{"model": "nomic-embed", "input": "hello world"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := up.path(); got != EmbeddingsPath {
		t.Errorf("upstream path=%q, want %q", got, EmbeddingsPath)
	}
}

func TestChatCompletions_404UnknownModel(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, nil)

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("no-such-model"), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", w.Code, w.Body.String())
	}
	if up.hits.Load() != 0 {
		t.Errorf("upstream must not be hit for an unknown model")
	}
}

func TestChatCompletions_RequiresModelField(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, nil)

	w := env.do(t, http.MethodPost, ChatCompletionsPath,
		map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}

func TestAuthorizeError_401(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.Authorize = func(*http.Request) (string, int, error) {
			return "", 0, errors.New("bad token")
		}
	})

	for _, path := range []string{ChatCompletionsPath, ModelsPath} {
		method := http.MethodPost
		var payload any = chatPayload("llama3.2:1b")
		if path == ModelsPath {
			method, payload = http.MethodGet, nil
		}
		w := env.do(t, method, path, payload, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d, want 401", path, w.Code)
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s decode: %v", path, err)
		}
		if got["error"] != "bad token" {
			t.Errorf("%s error=%v", path, got["error"])
		}
	}
	if up.hits.Load() != 0 {
		t.Errorf("unauthorized request must not reach a backend")
	}
}

func TestAuthorizeEmptyPrincipal_401(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.Authorize = func(*http.Request) (string, int, error) { return "  ", 0, nil }
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", w.Code)
	}
}

func TestAllowModel_403(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.AllowModel = func(_, model string) bool { return model != "llama3.2:1b" }
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", w.Code, w.Body.String())
	}
	if up.hits.Load() != 0 {
		t.Errorf("forbidden request must not reach a backend")
	}
}

// TestFailover_FirstBackendFailsBeforeFirstByte is the end-to-end case: two
// backends carry the model, the first dies before writing a byte, and the
// second serves the client transparently.
func TestFailover_FirstBackendFailsBeforeFirstByte(t *testing.T) {
	dead := newUpstream(t, "alpha", func(w http.ResponseWriter, _ *http.Request) {
		// 503 before any body byte: a clean failover candidate.
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	good := newUpstream(t, "beta", echoJSON)
	rows := []resolve.ModelRow{row("llama3.2:1b", "alpha"), row("llama3.2:1b", "beta")}

	var served atomic.Value
	env := newEnv(t, rows, []*upstream{dead, good}, func(c *Config) {
		// Pin the primary to the failing backend so the test exercises
		// the retry rather than the capacity ranking.
		c.Affinity.Stick(sched.DeriveSessionID(mustJSON(t, chatPayload("llama3.2:1b"))), "alpha", "llama3.2:1b")
		c.OnUsage = func(_, _, backend string, _ openai.Usage, _ time.Duration) {
			served.Store(backend)
		}
	})

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 after failover", w.Code, w.Body.String())
	}
	if dead.hits.Load() != 1 {
		t.Errorf("failing backend hits=%d, want 1", dead.hits.Load())
	}
	if good.hits.Load() != 1 {
		t.Errorf("surviving backend hits=%d, want 1", good.hits.Load())
	}
	if !strings.Contains(w.Body.String(), `"chat.completion"`) {
		t.Errorf("body did not come from the surviving backend: %s", w.Body.String())
	}
	// The usage hook must attribute to the backend that delivered, not
	// the one that died. It fires off the body-close path, so wait.
	waitFor(t, func() bool { _, ok := served.Load().(string); return ok })
	if got, _ := served.Load().(string); got != "beta" {
		t.Errorf("OnUsage backend=%q, want beta", got)
	}
	if got := w.Header().Values(sched.JobIDHeader); len(got) != 1 {
		t.Errorf("%s stamped %d times, want once: %v", sched.JobIDHeader, len(got), got)
	}
}

// TestToolCallAsText_BecomesToolCalls covers the extraction pass: a backend
// that emits a tool invocation as JSON text in message.content must reach
// the client as a structured tool_calls array.
func TestToolCallAsText_BecomesToolCalls(t *testing.T) {
	up := newUpstream(t, "alpha", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		inner := `{"name": "get_weather", "arguments": {"city": "Paris"}}`
		payload := map[string]any{
			"id":     "c1",
			"object": "chat.completion",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": inner},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		}
		_, _ = w.Write(mustJSON(t, payload))
	})
	env := newEnv(t, []resolve.ModelRow{row("qwen2.5-coder:7b", "alpha")}, []*upstream{up}, nil)

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("qwen2.5-coder:7b"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices=%d, want 1", len(got.Choices))
	}
	calls := got.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%d, want 1; body=%s", len(calls), w.Body.String())
	}
	if calls[0].Function.Name != "get_weather" {
		t.Errorf("tool name=%q, want get_weather", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "Paris") {
		t.Errorf("arguments=%q, want the city", calls[0].Function.Arguments)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason=%q, want tool_calls", got.Choices[0].FinishReason)
	}
}

// TestEmbeddings_NoToolCallExtraction pins that the extractor is chat-only.
func TestEmbeddings_NoToolCallExtraction(t *testing.T) {
	const raw = `{"name": "get_weather", "arguments": {"city": "Paris"}}`
	up := newUpstream(t, "alpha", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, raw)
	})
	env := newEnv(t, []resolve.ModelRow{row("nomic-embed", "alpha")}, []*upstream{up}, nil)

	w := env.do(t, http.MethodPost, EmbeddingsPath,
		map[string]any{"model": "nomic-embed", "input": "x"}, nil)
	if strings.TrimSpace(w.Body.String()) != raw {
		t.Errorf("embeddings body rewritten: %s", w.Body.String())
	}
}

func TestFilterCandidates_ExcludedAllGives429(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.FilterCandidates = func(context.Context, string, string, []string) []string { return nil }
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want 429", w.Code, w.Body.String())
	}
	if up.hits.Load() != 0 {
		t.Errorf("excluded backend must not be hit")
	}
}

func TestFilterCandidatesStatus_EmptyUsesStatusAndReason(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		want   int
	}{
		{name: "not found", status: http.StatusNotFound, want: http.StatusNotFound},
		{name: "rate limited", status: http.StatusTooManyRequests, want: http.StatusTooManyRequests},
		{name: "zero defaults to rate limited", status: 0, want: http.StatusTooManyRequests},
	} {
		t.Run(tt.name, func(t *testing.T) {
			up := newUpstream(t, "alpha", echoJSON)
			env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
				c.FilterCandidatesStatus = func(context.Context, string, string, []string) ([]string, int, string) {
					return nil, tt.status, "capacity policy rejected every backend"
				}
			})
			w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
			if w.Code != tt.want {
				t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body.String(), tt.want)
			}
			if !strings.Contains(w.Body.String(), "capacity policy rejected every backend") {
				t.Errorf("body=%s, want policy reason", w.Body.String())
			}
			if up.hits.Load() != 0 {
				t.Errorf("excluded backend must not be hit")
			}
		})
	}
}

func TestFilterCandidatesStatus_PartialProceeds(t *testing.T) {
	alpha := newUpstream(t, "alpha", echoJSON)
	beta := newUpstream(t, "beta", echoJSON)
	rows := []resolve.ModelRow{
		row("llama3.2:1b", "alpha"),
		row("llama3.2:1b", "beta"),
	}
	env := newEnv(t, rows, []*upstream{alpha, beta}, func(c *Config) {
		c.FilterCandidates = func(context.Context, string, string, []string) []string {
			return nil
		}
		c.FilterCandidatesStatus = func(context.Context, string, string, []string) ([]string, int, string) {
			return []string{"beta"}, http.StatusServiceUnavailable, "ignored"
		}
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if alpha.hits.Load() != 0 || beta.hits.Load() != 1 {
		t.Errorf("hits alpha=%d beta=%d, want 0 and 1", alpha.hits.Load(), beta.hits.Load())
	}
}

func TestCatalogFailure_ChatReturns503(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.Catalog.(*fakeCatalog).err = errors.New("inventory offline")
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", w.Code, w.Body.String())
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Error.Type != "catalog_unavailable" || !strings.Contains(got.Error.Message, "inventory offline") {
		t.Errorf("error=%+v", got.Error)
	}
	if up.hits.Load() != 0 {
		t.Errorf("backend must not be hit")
	}
}

func TestFallback_ServesWhenPoolCannot(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.Fallback = func(w http.ResponseWriter, _ *http.Request, principal, model string, _ []byte) bool {
			writeJSON(w, http.StatusOK, map[string]any{"served_by": "fallback", "principal": principal, "model": model})
			return true
		}
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("remote-only-model"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want the fallback's 200", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fallback") {
		t.Errorf("body=%s", w.Body.String())
	}
}

// TestPrivacyPolicy_SkipsFallback: the privacy posture keeps the request in
// the pool, so the fallback is never consulted and the 404 names the knob.
func TestPrivacyPolicy_SkipsFallback(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	called := false
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.Fallback = func(http.ResponseWriter, *http.Request, string, string, []byte) bool {
			called = true
			return true
		}
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("remote-only-model"),
		map[string]string{resolve.RouteHeader: resolve.RoutePrivacy})
	if called {
		t.Fatalf("privacy posture must not reach the fallback")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "privacy route policy") {
		t.Errorf("404 body should name the policy: %s", w.Body.String())
	}
	// The per-request opt-in re-enables it.
	called = false
	w = env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("remote-only-model"),
		map[string]string{resolve.RouteHeader: resolve.RoutePrivacy, resolve.AllowRemoteHeader: "true"})
	if !called || w.Code != http.StatusOK {
		t.Errorf("allow-remote opt-in: called=%v status=%d", called, w.Code)
	}
}

func TestOnUsage_ReceivesParsedTail(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	type seen struct {
		principal, model, backend string
		usage                     openai.Usage
	}
	got := make(chan seen, 1)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.OnUsage = func(principal, model, backend string, u openai.Usage, _ time.Duration) {
			got <- seen{principal, model, backend, u}
		}
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	select {
	case s := <-got:
		if s.principal != "alice" || s.model != "llama3.2:1b" || s.backend != "alpha" {
			t.Errorf("OnUsage=%+v", s)
		}
		if s.usage.TotalTokens != 7 {
			t.Errorf("total_tokens=%d, want 7", s.usage.TotalTokens)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnUsage never fired")
	}
}

func TestAdmission_CapReached429(t *testing.T) {
	release := make(chan struct{})
	up := newUpstream(t, "alpha", func(w http.ResponseWriter, _ *http.Request) {
		<-release
		echoJSON(w, nil)
	})
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.AdmissionLimit = func(string) int { return 1 }
	})
	defer close(release)

	inFlight := make(chan struct{})
	go func() {
		close(inFlight)
		// A distinct job id, so this request runs full admission.
		env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"),
			map[string]string{sched.JobIDHeader: "job-a"})
	}()
	<-inFlight
	waitFor(t, func() bool { return env.cfg.Admitter.InFlight("alice") == 1 })

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"),
		map[string]string{sched.JobIDHeader: "job-b"})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want 429", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After not stamped on the 429")
	}
}

func TestAutoSuffix_StrippedBeforeUpstream(t *testing.T) {
	var upstreamModel atomic.Value
	up := newUpstream(t, "alpha", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		upstreamModel.Store(env.Model)
		echoJSON(w, r)
	})
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, nil)

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"+resolve.AutoSuffix), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got, _ := upstreamModel.Load().(string); got != "llama3.2:1b" {
		t.Errorf("upstream saw model=%q, want the bare name", got)
	}
}

func TestBodyLimit_RejectsOversizedPayload(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, nil)

	// A body larger than the cap is truncated mid-JSON, so the model
	// peek fails and the request is rejected before any routing.
	huge := bytes.NewBuffer(nil)
	huge.WriteString(`{"model":"llama3.2:1b","messages":[{"role":"user","content":"`)
	huge.Write(bytes.Repeat([]byte("x"), maxRequestBody))
	huge.WriteString(`"}]}`)
	r := httptest.NewRequest(http.MethodPost, ChatCompletionsPath, huge)
	w := httptest.NewRecorder()
	env.handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
	if up.hits.Load() != 0 {
		t.Errorf("oversized payload must not reach a backend")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	enc, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return enc
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within the deadline")
}

// TestFilterCandidatesStatus_NotFoundWipeReachesFallback: a model no local
// backend can hold is a 404 wipe, and that is the same story as an empty
// pool — the host's remote path must still get its turn.
func TestFilterCandidatesStatus_NotFoundWipeReachesFallback(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	var gotModel string
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.FilterCandidatesStatus = func(context.Context, string, string, []string) ([]string, int, string) {
			return nil, http.StatusNotFound, "no local backend can hold llama3.2:1b"
		}
		c.Fallback = func(w http.ResponseWriter, _ *http.Request, _, model string, _ []byte) bool {
			gotModel = model
			writeJSON(w, http.StatusOK, map[string]any{"served_by": "fallback", "model": model})
			return true
		}
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want the fallback's 200", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fallback") {
		t.Errorf("body=%s, want the fallback's response", w.Body.String())
	}
	if gotModel != "llama3.2:1b" {
		t.Errorf("fallback model=%q, want llama3.2:1b", gotModel)
	}
	if up.hits.Load() != 0 {
		t.Errorf("excluded backend must not be hit")
	}
}

// TestFilterCandidatesStatus_NotFoundWipeFallbackDeclines: a fallback that
// declines leaves the hook's own 404 reason on the wire, not a generic one.
func TestFilterCandidatesStatus_NotFoundWipeFallbackDeclines(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	called := false
	env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
		c.FilterCandidatesStatus = func(context.Context, string, string, []string) ([]string, int, string) {
			return nil, http.StatusNotFound, "no local backend can hold llama3.2:1b"
		}
		c.Fallback = func(http.ResponseWriter, *http.Request, string, string, []byte) bool {
			called = true
			return false
		}
	})
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
	if !called {
		t.Fatalf("404 wipe must consult the fallback")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no local backend can hold llama3.2:1b") {
		t.Errorf("body=%s, want the hook's reason", w.Body.String())
	}
	if up.hits.Load() != 0 {
		t.Errorf("excluded backend must not be hit")
	}
}

// TestFilterCandidatesStatus_QuotaWipeSkipsFallback: 429 (and 503) mean the
// backends exist and a retry will work — leaving the pool would silently
// move the request off it, so the fallback stays untouched.
func TestFilterCandidatesStatus_QuotaWipeSkipsFallback(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		want   int
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, want: http.StatusTooManyRequests},
		{name: "zero defaults to rate limited", status: 0, want: http.StatusTooManyRequests},
		{name: "unavailable", status: http.StatusServiceUnavailable, want: http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			up := newUpstream(t, "alpha", echoJSON)
			called := false
			env := newEnv(t, []resolve.ModelRow{row("llama3.2:1b", "alpha")}, []*upstream{up}, func(c *Config) {
				c.FilterCandidatesStatus = func(context.Context, string, string, []string) ([]string, int, string) {
					return nil, tt.status, "over budget"
				}
				c.Fallback = func(w http.ResponseWriter, _ *http.Request, _, _ string, _ []byte) bool {
					called = true
					writeJSON(w, http.StatusOK, map[string]any{"served_by": "fallback"})
					return true
				}
			})
			w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), nil)
			if called {
				t.Fatalf("status=%d wipe must not reach the fallback", tt.status)
			}
			if w.Code != tt.want {
				t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body.String(), tt.want)
			}
			if !strings.Contains(w.Body.String(), "over budget") {
				t.Errorf("body=%s, want the hook's reason", w.Body.String())
			}
			if up.hits.Load() != 0 {
				t.Errorf("excluded backend must not be hit")
			}
		})
	}
}
