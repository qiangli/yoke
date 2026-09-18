package telemetry

import "testing"

// BASHY_TELEMETRY_NOTICE and BASHY_TELEMETRY_DEBUG are the two switches this
// run introduces, and the run's own automationMarker establishes the package's
// rule for boolean env vars: "0", "false", "no" and "off" mean UNSET (the
// CI=false fix from the previous round). shouldReportTelemetryStatus does not
// apply that rule to the switches it added: any non-empty value, including
// "0" and "false", forces the notice on. `BASHY_TELEMETRY_DEBUG=false bashy -c …`
// — the natural way to turn debug OFF in a CI matrix or a systemd unit — turns
// it ON, and prints the notice into exactly the noninteractive stderr this run
// exists to keep it out of.
func TestNoticeOptInHonoursFalseValues(t *testing.T) {
	for _, name := range []string{"CI", "CODEX_CI", "WEAVE_AGENT", "BASHY_AGENT_ID", "BASHY_PRINCIPAL", "TERM"} {
		t.Setenv(name, "")
	}
	t.Setenv("BASHY_TELEMETRY_QUIET", "")
	for _, tc := range []struct{ notice, debug string }{
		{"0", ""}, {"false", ""}, {"off", ""}, {"no", ""},
		{"", "0"}, {"", "false"}, {"", "off"}, {"", "no"},
	} {
		t.Setenv("BASHY_TELEMETRY_NOTICE", tc.notice)
		t.Setenv("BASHY_TELEMETRY_DEBUG", tc.debug)
		if shouldReportTelemetryStatus(false) {
			t.Errorf("BASHY_TELEMETRY_NOTICE=%q BASHY_TELEMETRY_DEBUG=%q on a noninteractive stderr forced the notice ON; %q is the package's own spelling of unset (see automationMarker)", tc.notice, tc.debug, tc.notice+tc.debug)
		}
	}
}
