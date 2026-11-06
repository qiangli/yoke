package binmgr

// Producer control for the P0 status seam: the readiness probe's polling must
// arrive as TYPED observations — first state + transitions, not one record per
// poll — with the HTTP answer as structured reported state and transport
// failures kept separate.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/qiangli/yoke/pkg/telemetry"
)

func withRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	return sr
}

// fail twice then succeed: records must be first(error HTTP 500) + recovery,
// with the repeated 500 collapsed.
func TestWaitHealthCollapsesRepeatsAndKeepsTransitions(t *testing.T) {
	// Fence the telemetry filesystem: the recorder is the only sink, and the
	// suppression state must not touch the operator's real spool directory.
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("BASHY_OTEL_SPOOL", filepath.Join(t.TempDir(), "fence", "spans.jsonl"))
	sr := withRecorder(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := waitHealth(context.Background(), "loom", srv.URL, 10*time.Second); err != nil {
		t.Fatalf("waitHealth: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("poll sequence (500, 500, 200) produced %d records, want 2 (first + recovery); the repeat collapse is the P0 requirement", len(spans))
	}
	first, recovery := map[string]string{}, map[string]string{}
	for _, kv := range spans[0].Attributes() {
		first[string(kv.Key)] = kv.Value.Emit()
	}
	for _, kv := range spans[1].Attributes() {
		recovery[string(kv.Key)] = kv.Value.Emit()
	}
	if first["status.state"] != telemetry.StatusError || first["status.detail"] != "HTTP 500" {
		t.Errorf("first record must be the structured reported state: %+v", first)
	}
	if recovery["status.prior_state"] != telemetry.StatusError || recovery["status.recovered"] != "true" {
		t.Errorf("recovery record wrong: %+v", recovery)
	}
	if spans[0].Name() != "service.status" {
		t.Errorf("records must come from the typed seam, got span %q", spans[0].Name())
	}
}

// A dead endpoint means the QUERY failed — no inferred service state, and the
// failure is its own record kind.
func TestWaitHealthRecordsQueryFailureSeparately(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("BASHY_OTEL_SPOOL", filepath.Join(t.TempDir(), "fence", "spans.jsonl"))
	sr := withRecorder(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens: every poll is a transport failure

	if err := waitHealth(context.Background(), "loom", url, 700*time.Millisecond); err == nil {
		t.Fatal("waitHealth unexpectedly succeeded against a dead endpoint")
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("repeated query failures produced %d records, want 1", len(spans))
	}
	if spans[0].Name() != "service.status_query_failed" {
		t.Errorf("query failure must be its own record kind, got %q", spans[0].Name())
	}
	var attrs map[string]string
	for _, kv := range spans[0].Attributes() {
		if attrs == nil {
			attrs = map[string]string{}
		}
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["status.service"] != "loom" {
		t.Errorf("service identity missing: %+v", attrs)
	}
	if _, hasState := attrs["status.state"]; hasState {
		t.Errorf("a failed query must not carry a reported state: %+v", attrs)
	}
}
