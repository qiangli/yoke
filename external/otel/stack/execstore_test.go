package stack

import (
	"slices"
	"strings"
	"testing"
)

func TestVictoriaStoreArgsDoNotReportRemotely(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"logs", victoriaLogsArgs(9428, "/data/logs", "")},
		{"metrics", victoriaMetricsArgs(8428, "/data/metrics", "")},
		{"traces", victoriaTracesArgs(10428, "/data/traces", "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, arg := range tt.args {
				if strings.HasPrefix(arg, "-pushmetrics.url") {
					t.Fatalf("remote metrics reporting enabled: %v", tt.args)
				}
			}
		})
	}
	if !slices.Contains(tests[1].args, "-selfScrapeInterval=0") {
		t.Fatalf("VictoriaMetrics self-scrape not explicitly disabled: %v", tests[1].args)
	}
}

func TestVictoriaStoreEnvPinsLocalCollector(t *testing.T) {
	if !slices.Contains(localOTLPEnv("victoria-logs"), "OTEL_EXPORTER_OTLP_ENDPOINT="+localCollectorEndpoint) {
		t.Fatalf("local collector missing: %v", localOTLPEnv("victoria-logs"))
	}
}
