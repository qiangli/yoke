package weave

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestWeaveOtelTraceparentPropagation(t *testing.T) {
	if os.Getenv("TEST_AS_AGENT") == "1" {
		tp := os.Getenv("TRACEPARENT")
		if tp == "" {
			fmt.Fprintln(os.Stderr, "no traceparent")
			os.Exit(1)
		}
		sr := tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
		otel.SetTextMapPropagator(propagation.TraceContext{})

		ctx := context.Background()
		carrier := propagation.MapCarrier{}
		carrier.Set("traceparent", tp)
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)

		_, span := otel.Tracer("test-agent").Start(ctx, "agent.turn")
		span.End()

		spans := sr.Ended()
		if len(spans) != 1 {
			fmt.Fprintf(os.Stderr, "agent spans=%d\n", len(spans))
			os.Exit(1)
		}
		evidence := map[string]string{
			"traceparent":    tp,
			"trace_id":       spans[0].SpanContext().TraceID().String(),
			"span_id":        spans[0].SpanContext().SpanID().String(),
			"parent_span_id": spans[0].Parent().SpanID().String(),
		}
		b, _ := json.Marshal(evidence)
		os.WriteFile(os.Getenv("TRACE_OUT_FILE"), b, 0644)
		exec.Command("git", "commit", "--allow-empty", "-m", "mock commit").Run()
		fmt.Println(`{"type":"result","num_turns":3}`)
		os.Exit(0)
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("system git not available")
	}

	// Because telemetry is initialized globally in the process, re-exec the test
	// so the recorder/providers below cannot inherit state from another test.
	if os.Getenv("TEST_OTEL_ISOLATION") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=TestWeaveOtelTraceparentPropagation")
		cmd.Env = append(os.Environ(), "TEST_OTEL_ISOLATION=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated test failed: %v\n%s", err, out)
		}
		return
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	repo := t.TempDir()
	gitE2E(t, repo, "init", "-q", "-b", "main")
	gitE2E(t, repo, "config", "user.email", "test@test.local")
	gitE2E(t, repo, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello"), 0644)
	gitE2E(t, repo, "add", "-A")
	gitE2E(t, repo, "commit", "-q", "-m", "init")
	t.Chdir(repo)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	oldTracerProvider := otel.GetTracerProvider()
	oldPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(oldTracerProvider)
		otel.SetTextMapPropagator(oldPropagator)
	})

	metricReader := sdkmetric.NewManualReader()
	oldMeterProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader)))
	t.Cleanup(func() { otel.SetMeterProvider(oldMeterProvider) })

	runWeave(t, "add", "test issue")

	traceOutFile := filepath.Join(home, "trace.out")
	t.Setenv("TRACE_OUT_FILE", traceOutFile)
	t.Setenv("TEST_AS_AGENT", "1")

	out, code := runWeave(t, "start", "--pty", "never", "--", os.Args[0], "-test.run=TestWeaveOtelTraceparentPropagation")
	if code != 0 {
		t.Fatalf("start failed: %d\n%s", code, out)
	}

	tpBytes, err := os.ReadFile(traceOutFile)
	if err != nil {
		t.Fatalf("agent did not write traceparent: %v", err)
	}
	var child struct {
		Traceparent  string `json:"traceparent"`
		TraceID      string `json:"trace_id"`
		ParentSpanID string `json:"parent_span_id"`
	}
	if err := json.Unmarshal(tpBytes, &child); err != nil {
		t.Fatalf("agent trace evidence was not JSON: %v\n%s", err, tpBytes)
	}
	parts := strings.Split(child.Traceparent, "-")
	if len(parts) < 3 {
		t.Fatalf("invalid traceparent parts: %s", child.Traceparent)
	}
	extractedTraceID := parts[1]
	extractedParentSpanID := parts[2]

	var weaveRunTraceID, weaveRunSpanID string
	var weaveRunFound bool
	var outcomeConverged bool
	var outcome string

	for _, span := range sr.Ended() {
		if span.Name() != "weave.run" {
			continue
		}
		weaveRunFound = true
		weaveRunTraceID = span.SpanContext().TraceID().String()
		weaveRunSpanID = span.SpanContext().SpanID().String()
		for _, attr := range span.Attributes() {
			if attr.Key == "converged" {
				outcomeConverged = attr.Value.AsBool()
			}
			if attr.Key == "outcome" {
				outcome = attr.Value.AsString()
			}
		}
	}

	if !weaveRunFound {
		t.Errorf("weave.run span not found")
	} else if !outcomeConverged {
		t.Errorf("weave.run span was not flagged as converged despite exiting 0")
	} else if outcome == "" {
		t.Errorf("weave.run span did not carry outcome")
	}

	if weaveRunTraceID != child.TraceID {
		t.Errorf("trace IDs do not match! weave.run=%s, agent.turn=%s", weaveRunTraceID, child.TraceID)
	}
	if weaveRunTraceID != extractedTraceID {
		t.Errorf("trace ID from environment does not match! env=%s, weave.run=%s", extractedTraceID, weaveRunTraceID)
	}
	if weaveRunSpanID != extractedParentSpanID || weaveRunSpanID != child.ParentSpanID {
		t.Errorf("agent.turn parent does not match weave.run span: weave=%s env=%s child=%s", weaveRunSpanID, extractedParentSpanID, child.ParentSpanID)
	}

	var rm metricdata.ResourceMetrics
	if err := metricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	if !hasFleetRunTurnsMetric(rm) {
		t.Errorf("fleet.run.turns histogram was not emitted")
	}
}

func hasFleetRunTurnsMetric(rm metricdata.ResourceMetrics) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "fleet.run.turns" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			if !ok {
				return false
			}
			for _, dp := range h.DataPoints {
				if dp.Count == 0 {
					continue
				}
				if hasAttr(dp.Attributes.ToSlice(), "agent") && hasAttr(dp.Attributes.ToSlice(), "band") {
					return true
				}
			}
		}
	}
	return false
}

func hasAttr(attrs []attribute.KeyValue, key string) bool {
	for _, attr := range attrs {
		if string(attr.Key) == key {
			return true
		}
	}
	return false
}

func TestWeaveOtelNoop(t *testing.T) {
	if os.Getenv("TEST_OTEL_ISOLATION_NOOP") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=TestWeaveOtelNoop")
		cmd.Env = append(os.Environ(), "TEST_OTEL_ISOLATION_NOOP=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated test failed: %v\n%s", err, out)
		}
		return
	}

	// Telemetry defaults to the local file spool. The standard "none" exporter
	// value is the explicit no-op contract.
	os.Setenv("OTEL_TRACES_EXPORTER", "none")
	os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")

	// This should not panic or start any exporters.
	shutdown := telemetry.Init(context.Background())
	defer shutdown(context.Background())

	if telemetry.Enabled() {
		t.Fatalf("telemetry should be disabled when OTEL_TRACES_EXPORTER=none")
	}
}
