package broker

import (
	"os"
	"strings"
	"testing"
)

// operatorHomeEnv carries the pre-fence HOME so a live test can run a real
// subscription CLI seat; see useOperatorHome.
const operatorHomeEnv = "YOKE_TEST_OPERATOR_HOME"

// useOperatorHome points HOME (and USERPROFILE) back at the operator's real
// home for one live test, keeping BASHY_HOME on the test's private fence so
// nothing the door journals reaches the real ~/.bashy. It skips when the
// test main recorded no real home.
func useOperatorHome(t *testing.T) {
	t.Helper()
	home := strings.TrimSpace(os.Getenv(operatorHomeEnv))
	if home == "" {
		t.Skip("no operator home recorded; live CLI seats need the real login")
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// TestMain fences every pkg/broker test into a private home. Door error
// responses are journaled through quotaobs; without this fence a test that
// forgets a per-test BASHY_HOME would append its fixtures to the operator's
// real ~/.bashy/quota/observations.jsonl.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "yoke-broker-test-*")
	if err != nil {
		panic(err)
	}
	// The operator's real home stays reachable for the opt-in live tests:
	// a subscription CLI finds its login (and its pinned managed binary)
	// under the real home, never under the fence.
	if err := os.Setenv(operatorHomeEnv, os.Getenv("HOME")); err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	if err := os.Setenv("USERPROFILE", home); err != nil {
		panic(err)
	}
	// A developer shell may export BASHY_HOME at the real home; it takes
	// precedence over HOME in every quota path helper, so drop it and let
	// per-test t.Setenv installs win.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "BASHY_HOME" || key == "BASHY_LLM_BUDGET_STATE" {
			if err := os.Unsetenv(key); err != nil {
				panic(err)
			}
		}
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
