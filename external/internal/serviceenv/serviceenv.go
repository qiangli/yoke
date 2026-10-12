// Package serviceenv defines the privacy boundary for managed services.
package serviceenv

import "strings"

const (
	// LocalCollectorEndpoint is the stable OTLP/HTTP proxy exposed by
	// external/otel/stack (stack.DefaultProxyPort).
	LocalCollectorEndpoint = "http://127.0.0.1:31415"
	LocalCollectorHostPort = "127.0.0.1:31415"
)

var otlpOverrides = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT=" + LocalCollectorEndpoint,
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + LocalCollectorEndpoint + "/v1/traces",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=" + LocalCollectorEndpoint + "/v1/metrics",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=" + LocalCollectorEndpoint + "/v1/logs",
	"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
	"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf",
	"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=http/protobuf",
	"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL=http/protobuf",
}

// LocalOTLP returns a complete child environment with every general and
// signal-specific OTLP endpoint pinned to bashy's local collector. Removing the
// inherited values first is important: signal-specific variables override the
// general endpoint and could otherwise retain a vendor destination.
//
// Standard configuration: https://opentelemetry.io/docs/specs/otel/protocol/exporter/
func LocalOTLP(base []string, serviceName string) []string {
	out := make([]string, 0, len(base)+len(otlpOverrides)+1)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if isManagedOTLPKey(key) || key == "OTEL_SERVICE_NAME" {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, otlpOverrides...)
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
