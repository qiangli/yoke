package stack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	colltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

func TestOTLPGRPCBridgeRefusesNonLoopbackBind(t *testing.T) {
	bridge := NewOTLPGRPCBridge("0.0.0.0:4317", map[string]string{
		"/v1/traces": "http://127.0.0.1:1/insert/opentelemetry/v1/traces",
	})
	if err := bridge.Start(context.Background()); err == nil {
		_ = bridge.Stop(context.Background())
		t.Fatalf("expected non-loopback bind to be refused")
	} else if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOTLPGRPCBridgeForwardsTraceToLoopbackHTTP(t *testing.T) {
	var called bool
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	backend := map[string]string{"/v1/traces": srv.URL}
	host := "127.0.0.1:0"
	bridge := NewOTLPGRPCBridge(host, backend)
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("bridge start: %v", err)
	}
	defer bridge.Stop(context.Background())

	if !bridge.Healthy() {
		t.Fatalf("bridge not healthy after start")
	}
	if !strings.HasPrefix(bridge.addr, "127.0.0.1:") {
		t.Fatalf("bridge addr not loopback: %q", bridge.addr)
	}

	req := &colltrace.ExportTraceServiceRequest{}
	if err := bridge.forward(context.Background(), "/v1/traces", req); err != nil {
		t.Fatalf("forward failed: %v", err)
	}
	if !called {
		t.Fatalf("backend not called")
	}
	if gotCT != "application/x-protobuf" {
		t.Fatalf("Content-Type = %q, want application/x-protobuf", gotCT)
	}
}

func TestOTLPGRPCBridgeRefusesNonLoopbackForward(t *testing.T) {
	bridge := NewOTLPGRPCBridge("127.0.0.1:0", map[string]string{
		"/v1/traces": "http://203.0.113.1:4318/v1/traces",
	})
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("bridge start: %v", err)
	}
	defer bridge.Stop(context.Background())

	req := &colltrace.ExportTraceServiceRequest{}
	if err := bridge.forward(context.Background(), "/v1/traces", req); err == nil {
		t.Fatalf("expected non-loopback forward to be refused")
	} else if !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("unexpected error: %v", err)
	}
}
