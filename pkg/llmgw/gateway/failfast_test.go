package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/resolve"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

// chatOnlyHandler counts only inference requests. A two-candidate slate also
// draws live capacity probes on the upstreams; those are scheduling reads,
// not serving attempts, and must not count toward the failover budget.
func chatOnlyHandler(counter *atomic.Int64, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ChatCompletionsPath {
			counter.Add(1)
		}
		h(w, r)
	}
}

// timedUpstream is an httptest server plus a reverse-proxy backend with an
// explicit first-byte timeout, for the silent-upstream case.
func timedUpstream(t *testing.T, name string, h http.HandlerFunc, firstByte time.Duration) (*httptest.Server, Backend) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	return srv, ReverseProxyBackendWithTimeout(name, target, nil, firstByte)
}

func classifiedBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	dec := json.NewDecoder(strings.NewReader(w.Body.String()))
	// The gateway writes OpenAI-shape errors as {"error": {...}}; unwrap one level.
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	if inner, ok := raw["error"].(map[string]any); ok {
		body = inner
	} else {
		body = raw
	}
	return body
}

func twoKeyEnv(t *testing.T, hA, hB http.HandlerFunc, tweak func(*Config)) (*testEnv, *upstream, *upstream) {
	t.Helper()
	upA := newUpstream(t, "alpha", hA)
	upB := newUpstream(t, "beta", hB)
	rows := []resolve.ModelRow{row("glm-4", "alpha"), row("glm-4", "beta")}
	env := newEnv(t, rows, []*upstream{upA, upB}, tweak)
	return env, upA, upB
}

// An invalid metered key fails fast: 401 with a classified body, a single
// upstream attempt, well under a second.
func TestFailFast_InvalidKey(t *testing.T) {
	invalid := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`))
	}
	var aHits, bHits atomic.Int64
	env, _, _ := twoKeyEnv(t, chatOnlyHandler(&aHits, invalid), chatOnlyHandler(&bHits, invalid), nil)

	started := time.Now()
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("glm-4"), nil)
	elapsed := time.Since(started)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 (body=%s)", w.Code, w.Body.String())
	}
	body := classifiedBody(t, w)
	if body["type"] != "auth" {
		t.Errorf("type=%v, want auth (body=%s)", body["type"], w.Body.String())
	}
	if body["provider"] != "alpha" && body["provider"] != "beta" {
		t.Errorf("provider=%v, want the serving backend", body["provider"])
	}
	if body["model"] != "glm-4" {
		t.Errorf("model=%v, want glm-4", body["model"])
	}
	if body["retryable"] != false {
		t.Errorf("retryable=%v, want false", body["retryable"])
	}
	if body["hint"] == nil || body["hint"] == "" {
		t.Errorf("hint missing (body=%s)", w.Body.String())
	}
	if total := aHits.Load() + bHits.Load(); total != 1 {
		t.Errorf("upstream serves=%d, want exactly 1 (no retry on auth)", total)
	}
	if elapsed >= time.Second {
		t.Errorf("elapsed=%v, want < 1s", elapsed)
	}
}

// A balance-exhausted metered key fails fast as 402: the 429 billing body
// must never spend the failover budget on another metered key.
func TestFailFast_BalanceExhausted(t *testing.T) {
	dry := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`))
	}
	var aHits, bHits atomic.Int64
	env, _, _ := twoKeyEnv(t, chatOnlyHandler(&aHits, dry), chatOnlyHandler(&bHits, dry), nil)

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("glm-4"), nil)

	if w.Code != 402 {
		t.Fatalf("status=%d, want 402 (body=%s)", w.Code, w.Body.String())
	}
	body := classifiedBody(t, w)
	if body["type"] != "billing" {
		t.Errorf("type=%v, want billing", body["type"])
	}
	if body["retryable"] != false {
		t.Errorf("retryable=%v, want false", body["retryable"])
	}
	if total := aHits.Load() + bHits.Load(); total != 1 {
		t.Errorf("upstream serves=%d, want exactly 1 (no retry on billing)", total)
	}
}

// A retryable rate limit still fails over: the next candidate serves.
func TestFailFast_RateLimitedStillFailsover(t *testing.T) {
	limited := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached, slow down","type":"rate_limit_exceeded"}}`))
	}
	var aHits, bHits atomic.Int64
	upA := newUpstream(t, "alpha", chatOnlyHandler(&aHits, limited))
	upB := newUpstream(t, "beta", chatOnlyHandler(&bHits, echoJSON))
	rows := []resolve.ModelRow{row("glm-4", "alpha"), row("glm-4", "beta")}
	env := newEnv(t, rows, []*upstream{upA, upB}, nil)
	// Pin the primary to the rate-limited backend so the test does not
	// depend on the capacity tie-break between equal candidates.
	raw, _ := json.Marshal(chatPayload("glm-4"))
	env.cfg.Affinity.Stick(sched.DeriveSessionID(raw), "alpha", "glm-4")

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("glm-4"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if total := aHits.Load() + bHits.Load(); total != 2 {
		t.Errorf("upstream serves=%d, want 2 (failover on rate limit)", total)
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Errorf("serves alpha=%d beta=%d, want 1 each", aHits.Load(), bHits.Load())
	}
}

// With an operator-configured fallback, a billing failure on a metered key
// is served once by the fallback and carries the audit header.
func TestMeteredFallback_ServesWithHeader(t *testing.T) {
	dry := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance","type":"insufficient_balance"}}`))
	}
	fallbackCalls := 0
	tweak := func(c *Config) {
		c.MeteredFallback = func(w http.ResponseWriter, _ *http.Request, _, model string, _ []byte, f UpstreamFailure) (string, bool) {
			fallbackCalls++
			if f.Class != UpstreamBilling {
				t.Errorf("fallback class=%q, want billing", f.Class)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(FallbackHeader, f.Provider+" -> local")
			_, _ = w.Write([]byte(`{"id":"fb1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fallback"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return "local", true
		}
	}
	var aHits, bHits atomic.Int64
	env, _, _ := twoKeyEnv(t, chatOnlyHandler(&aHits, dry), chatOnlyHandler(&bHits, dry), tweak)

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("glm-4"), nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 from fallback (body=%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get(FallbackHeader); !strings.Contains(got, "->") || !strings.Contains(got, "local") {
		t.Errorf("%s=%q, want <from> -> local", FallbackHeader, got)
	}
	if fallbackCalls != 1 {
		t.Errorf("fallback calls=%d, want exactly 1", fallbackCalls)
	}
	if total := aHits.Load() + bHits.Load(); total != 1 {
		t.Errorf("upstream serves=%d, want 1 (fallback, not another metered key)", total)
	}
}

// Without operator config there is no fallback: a billing failure fails fast.
func TestFailFast_NoFallbackConfigured(t *testing.T) {
	dry := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance","type":"insufficient_balance"}}`))
	}
	env, upA, _ := twoKeyEnv(t, dry, dry, nil)

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("glm-4"), nil)

	if w.Code != 402 {
		t.Fatalf("status=%d, want 402 (body=%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get(FallbackHeader); got != "" {
		t.Errorf("%s=%q, want empty without configured fallback", FallbackHeader, got)
	}
	if total := upA.hits.Load(); total < 1 {
		t.Errorf("upstream hits=%d, want >= 1", total)
	}
}

// A silent upstream cannot hang the caller: the first-byte bound turns it
// into a timeout error.
func TestFailFast_SilentUpstream(t *testing.T) {
	silent := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			_, _ = w.Write([]byte(`too late`))
		case <-r.Context().Done():
		}
	}
	_, backend := timedUpstream(t, "alpha", silent, 300*time.Millisecond)
	env := newEnv(t, []resolve.ModelRow{row("glm-4", "alpha")}, nil, func(c *Config) {
		c.Backend = func(name string) (Backend, bool) {
			if name == "alpha" {
				return backend, true
			}
			return nil, false
		}
		c.Backends = func() []Backend { return []Backend{backend} }
	})

	started := time.Now()
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("glm-4"), nil)
	elapsed := time.Since(started)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d, want 504 (body=%s)", w.Code, w.Body.String())
	}
	body := classifiedBody(t, w)
	if body["type"] != "timeout" {
		t.Errorf("type=%v, want timeout", body["type"])
	}
	if elapsed >= 3*time.Second {
		t.Errorf("elapsed=%v, want well under the upstream silence", elapsed)
	}
}
