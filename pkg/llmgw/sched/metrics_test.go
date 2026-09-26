package sched

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewMetrics_IsolatedRegistries(t *testing.T) {
	NewMetrics(nil)
	NewMetrics(nil)
}

func TestMetricsHandler_ServesPrometheusFormat(t *testing.T) {
	metrics := NewMetrics(nil)
	// Drive a few metrics so the scrape body isn't empty.
	metrics.ObserveDispatch("alpha", "qwen:7b", "dispatched")
	metrics.ObserveDispatch("alpha", "qwen:7b", "rate_limited")
	metrics.ObserveSlotInFlight("alpha", "qwen:7b", 3)
	metrics.ObserveResolverStage("exact-match", "hit")
	metrics.ObserveResolvedModel("qwen-coder:7b", "qwen-coder:14b")
	metrics.ObserveRetryFreeIssued()

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/metrics", nil)
	metrics.Handler().ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	s := string(body)
	for _, want := range []string{
		"llm_dispatch_total",
		`outcome="dispatched"`,
		`outcome="rate_limited"`,
		"llm_slot_in_flight",
		"llm_resolver_stage_total",
		"llm_resolved_model_total",
		"llm_retry_free_nonces_issued_total",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
}

func TestVTCMaxMinRatio_NoDivideByZero(t *testing.T) {
	metrics := NewMetrics(nil)
	a := resetAdmitter(t)
	// Empty Admitter: should be a silent no-op (no panic).
	metrics.ObserveVTCMaxMinRatio(a)

	a.ChargeVTC("only", 500)
	metrics.ObserveVTCMaxMinRatio(a) // single principal — also no-op

	a.ChargeVTC("min", 100)
	a.ChargeVTC("max", 1000)
	metrics.ObserveVTCMaxMinRatio(a)
	// We can't easily peek at the gauge value via the wire format
	// without a registry-scrape parse; just confirming no panic on
	// the computation paths.
}

func TestObserveResolvedModel_SkipsEqual(t *testing.T) {
	metrics := NewMetrics(nil)
	// requested == resolved => no substitution => no metric.
	metrics.ObserveResolvedModel("same:7b", "same:7b")
	// No good way to assert "not incremented" without snapshotting
	// the counter; this test just exercises the early-return.
}
