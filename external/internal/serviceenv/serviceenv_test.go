package serviceenv

import (
	"strings"
	"testing"
)

func TestLocalOTLPReplacesInheritedEndpoints(t *testing.T) {
	env := LocalOTLP([]string{
		"PATH=/bin",
		"OTEL_EXPORTER_OTLP_ENDPOINT=https://vendor.example",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://traces.vendor.example/v1/traces",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=grpc",
		"OTEL_SERVICE_NAME=parent",
	}, "loom")

	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "vendor.example") {
		t.Fatalf("vendor endpoint survived:\n%s", joined)
	}
	for _, want := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + LocalCollectorEndpoint,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + LocalCollectorEndpoint + "/v1/traces",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=" + LocalCollectorEndpoint + "/v1/metrics",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=" + LocalCollectorEndpoint + "/v1/logs",
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
		"OTEL_SERVICE_NAME=loom",
	} {
		if !containsExact(env, want) {
			t.Errorf("missing %q in %v", want, env)
		}
	}
}

func TestLocalOTLPForKopiaUsesGRPCLoopback(t *testing.T) {
	env := LocalOTLP([]string{
		"PATH=/bin",
		"OTEL_EXPORTER_OTLP_ENDPOINT=https://vendor.example",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://traces.vendor.example/v1/traces",
		"OTEL_SERVICE_NAME=parent",
	}, "kopia")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "vendor.example") {
		t.Fatalf("vendor endpoint survived:\n%s", joined)
	}
	for _, want := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + LocalCollectorGRPCEndpoint,
		"OTEL_EXPORTER_OTLP_PROTOCOL=grpc",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=grpc",
		"OTEL_SERVICE_NAME=kopia",
	} {
		if !containsExact(env, want) {
			t.Errorf("missing %q for kopia in %v", want, env)
		}
	}
	if strings.Contains(joined, "http/protobuf") {
		t.Fatalf("kopia must not use http/protobuf (needs grpc):\n%s", joined)
	}
	if !strings.Contains(joined, "127.0.0.1:") {
		t.Fatalf("kopia endpoint not loopback:\n%s", joined)
	}
	if strings.Contains(joined, "0.0.0.0") {
		t.Fatalf("kopia endpoint must not bind 0.0.0.0:\n%s", joined)
	}
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
