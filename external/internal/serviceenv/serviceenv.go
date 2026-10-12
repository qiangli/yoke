// Package serviceenv defines the privacy boundary for managed services.
package serviceenv

import "strings"

const (
	// LocalCollectorEndpoint is the stable OTLP/HTTP proxy exposed by
	// external/otel/stack (stack.DefaultProxyPort).
	LocalCollectorEndpoint = "http://127.0.0.1:31415"
	LocalCollectorHostPort = "127.0.0.1:31415"
	// LocalCollectorGRPCEndpoint is the loopback-only OTLP/gRPC receiver
	// (stack.DefaultGRPCPort) that forwards to the same Victoria stores.
	// Kopia v0.23.1's --otlp-trace uses otlptracegrpc (Go gRPC exporter),
	// which does not speak OTLP/HTTP; it must target this gRPC address.
	LocalCollectorGRPCEndpoint = "http://127.0.0.1:31416"
	LocalCollectorGRPCHostPort = "127.0.0.1:31416"
	LocalCollectorGRPCPort     = 31416
)

var httpOTLPOverrides = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT=" + LocalCollectorEndpoint,
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + LocalCollectorEndpoint + "/v1/traces",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=" + LocalCollectorEndpoint + "/v1/metrics",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=" + LocalCollectorEndpoint + "/v1/logs",
	"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
	"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf",
	"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=http/protobuf",
	"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL=http/protobuf",
}

var grpcOTLPOverrides = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT=" + LocalCollectorGRPCEndpoint,
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + LocalCollectorGRPCEndpoint,
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=" + LocalCollectorGRPCEndpoint,
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=" + LocalCollectorGRPCEndpoint,
	"OTEL_EXPORTER_OTLP_PROTOCOL=grpc",
	"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=grpc",
	"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=grpc",
	"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL=grpc",
}

// LocalOTLP returns a complete child environment with every general and
// signal-specific OTLP endpoint pinned to bashy's local collector. Removing the
// inherited values first is important: signal-specific variables override the
// general endpoint and could otherwise retain a vendor destination.
//
// Kopia v0.23.1's --otlp-trace uses otlptracegrpc (gRPC only, see
// cli/observability_flags.go: otlptracegrpc.NewUnstarted), which does not honour
// OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf. Kopia must target the
// loopback gRPC receiver (127.0.0.1:31416); all other services use OTLP/HTTP
// on 127.0.0.1:31415.
//
// Standard configuration: https://opentelemetry.io/docs/specs/otel/protocol/exporter/
func LocalOTLP(base []string, serviceName string) []string {
	overrides := httpOTLPOverrides
	if serviceName == "kopia" {
		overrides = grpcOTLPOverrides
	}
	out := make([]string, 0, len(base)+len(overrides)+1)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if isManagedOTLPKey(key) || key == "OTEL_SERVICE_NAME" {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, overrides...)
	if serviceName != "" {
		out = append(out, "OTEL_SERVICE_NAME="+serviceName)
	}
	return out
}

func isManagedOTLPKey(key string) bool {
	switch key {
	case "OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL":
		return true
	default:
		return false
	}
}
