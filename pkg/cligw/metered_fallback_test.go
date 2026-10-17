package cligw

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/gateway"
)

type sliceRecorder struct {
	mu        sync.Mutex
	decisions []Decision
}

func (r *sliceRecorder) Record(_ context.Context, d Decision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, d)
	return nil
}

func TestMeteredFallback_NilWithoutConfig(t *testing.T) {
	s := &Server{policy: DefaultPolicy(), log: slog.Default()}
	var routed http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if fn := s.meteredFallback(&routed); fn != nil {
		t.Fatalf("meteredFallback without config = non-nil, want nil (fail fast)")
	}
}

func TestPolicy_MeteredFallbackValidation(t *testing.T) {
	for _, band := range []string{"L2", "L1", "L5", " l3 "} {
		p := DefaultPolicy()
		p.MeteredFallback = band
		if err := p.Validate(); err != nil {
			t.Errorf("band %q rejected: %v", band, err)
		}
		if strings.TrimSpace(p.MeteredFallback) == "" {
			t.Errorf("band %q was not normalized", band)
		}
	}
	for _, bad := range []string{"L2+", "L0", "L9", "glm-4", "band:L2"} {
		p := DefaultPolicy()
		p.MeteredFallback = bad
		if err := p.Validate(); err == nil {
			t.Errorf("band %q accepted, want rejection", bad)
		}
	}
}

func TestMeteredFallback_ServesOnceWithHeaderAndAudit(t *testing.T) {
	recorder := &sliceRecorder{}
	s := &Server{policy: DefaultPolicy(), recorder: recorder, log: slog.Default()}
	s.policy.MeteredFallback = "L2"
	if err := s.policy.Validate(); err != nil {
		t.Fatalf("validate policy: %v", err)
	}

	var innerModel string
	var innerGuard bool
	var innerCalls int
	var routed http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerCalls++
		innerGuard = r.Header.Get(fallbackAttemptHeader) == "1"
		raw, _ := io.ReadAll(r.Body)
		var envelope struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &envelope)
		innerModel = envelope.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	fn := s.meteredFallback(&routed)
	if fn == nil {
		t.Fatal("meteredFallback with config = nil, want hook")
	}
	failure := gateway.UpstreamFailure{Class: gateway.UpstreamBilling, Status: 402}.WithEndpoint("alpha", "glm-4")

	body := []byte(`{"model":"glm-4","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

	to, served := fn(w, r, "owner", "glm-4", body, failure)
	if !served || to != "band:L2" {
		t.Fatalf("to=%q served=%v, want band:L2 true", to, served)
	}
	if got := w.Header().Get(gateway.FallbackHeader); got != "alpha -> band:L2" {
		t.Errorf("%s=%q, want %q", gateway.FallbackHeader, got, "alpha -> band:L2")
	}
	if innerCalls != 1 {
		t.Fatalf("inner calls=%d, want 1", innerCalls)
	}
	if innerModel != "L2" {
		t.Errorf("inner model=%q, want pinned band L2", innerModel)
	}
	if !innerGuard {
		t.Errorf("inner request missing %s guard", fallbackAttemptHeader)
	}
	if len(recorder.decisions) != 1 {
		t.Fatalf("audit decisions=%d, want 1", len(recorder.decisions))
	}
	if !strings.Contains(recorder.decisions[0].Reason, "metered-fallback") {
		t.Errorf("audit reason=%q, want metered-fallback marker", recorder.decisions[0].Reason)
	}

	// A repeated auth/billing failure on the guarded request declines: the
	// fallback runs at most once.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	r2.Header.Set(fallbackAttemptHeader, "1")
	if to, served := fn(w2, r2, "owner", "L2", body, failure); served || to != "" {
		t.Errorf("guarded retry to=%q served=%v, want declined", to, served)
	}
	if innerCalls != 1 {
		t.Errorf("inner calls=%d after guarded retry, want still 1", innerCalls)
	}
}
