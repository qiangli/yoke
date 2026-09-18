package telemetry

import (
	"os"
	"testing"
)

func TestShouldReportTelemetryStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		quiet    string
		notice   string
		debug    string
		want     bool
	}{
		{"interactive", true, "", "", "", true},
		{"noninteractive default", false, "", "", "", false},
		{"notice opt-in", false, "", "1", "", true},
		{"debug opt-in", false, "", "", "1", true},
		{"quiet wins over terminal", true, "1", "", "", false},
		{"quiet wins over notice", false, "1", "1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"CI", "CODEX_CI", "WEAVE_AGENT", "BASHY_AGENT_ID", "BASHY_PRINCIPAL", "TERM"} {
				t.Setenv(name, "")
			}
			t.Setenv("BASHY_TELEMETRY_QUIET", tc.quiet)
			t.Setenv("BASHY_TELEMETRY_NOTICE", tc.notice)
			t.Setenv("BASHY_TELEMETRY_DEBUG", tc.debug)
			if got := shouldReportTelemetryStatus(tc.terminal); got != tc.want {
				t.Errorf("shouldReportTelemetryStatus(%v) = %v, want %v", tc.terminal, got, tc.want)
			}
		})
	}
}

func TestIsTerminalRejectsNullDevice(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("null device reported as an interactive terminal")
	}
}
