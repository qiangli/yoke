package toolcmd

import (
	"os"
	"strings"
	"testing"
)

// TestMain fences every pkg/toolcmd test into a private home. Job state and
// peer registries live under $BASHY_HOME (else ~/.bashy); without this fence
// a test that forgets a per-test home would touch the operator's real
// ~/.bashy/toolcmd and quota siblings.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "yoke-toolcmd-test-*")
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
	// precedence over HOME in every state path helper, so drop it and let
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
