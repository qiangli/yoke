package cligw

import (
	"os"
	"strings"
	"testing"
)

// TestMain fences every pkg/cligw test into a private home. Worker quota
// output is journaled through quotaobs; without this fence a test that
// forgets a per-test BASHY_HOME would append its fixtures to the operator's
// real ~/.bashy/quota/observations.jsonl.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "yoke-cligw-test-*")
	if err != nil {
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
