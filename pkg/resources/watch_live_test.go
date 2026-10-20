package resources

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestWatchLiveOnce is the S480 live gate: ONE real `resource watch --once`
// sample on this host (auto-registration plus a single evaluation pass).
// Skipped unless BASHY_WATCH_LIVE=1, so ordinary test runs never touch the
// real registry or state.
func TestWatchLiveOnce(t *testing.T) {
	if os.Getenv("BASHY_WATCH_LIVE") != "1" {
		t.Skip("set BASHY_WATCH_LIVE=1 to take the live sample")
	}
	cmd := NewCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"watch", "--once"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("watch --once: %v\n%s", err, out.String())
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Fatal("watch --once produced no output")
	}
	t.Logf("live sample:\n%s", out.String())
}
