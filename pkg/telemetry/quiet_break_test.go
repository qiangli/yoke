package telemetry

import "testing"

// shouldReportTelemetryStatus reads three boolean switches. This run gave two
// of them (BASHY_TELEMETRY_NOTICE, BASHY_TELEMETRY_DEBUG) and every automation
// marker the package's own spelling of "unset" via automationMarker: "0",
// "false", "no", "off". The third, BASHY_TELEMETRY_QUIET, still means "set to
// anything at all", so inside ONE function `false` is unset for two variables
// and set for the third. `BASHY_TELEMETRY_QUIET=false bashy` — the natural way
// to turn quiet OFF in a profile or CI matrix that defaults it on — silences the
// notice for the interactive user who asked to see it, and no documentation
// says the switches disagree.
func TestQuietHonoursFalseValues(t *testing.T) {
	for _, name := range []string{"CI", "CODEX_CI", "WEAVE_AGENT", "BASHY_AGENT_ID", "BASHY_PRINCIPAL", "TERM"} {
		t.Setenv(name, "")
	}
	t.Setenv("BASHY_TELEMETRY_NOTICE", "")
	t.Setenv("BASHY_TELEMETRY_DEBUG", "")
	for _, v := range []string{"0", "false", "no", "off"} {
		t.Setenv("BASHY_TELEMETRY_QUIET", v)
		if !shouldReportTelemetryStatus(true) {
			t.Errorf("BASHY_TELEMETRY_QUIET=%q silenced an interactive stderr; %q is the package's own spelling of unset (see automationMarker)", v, v)
		}
	}
}
