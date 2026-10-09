package weave

import (
	"os"
	"strings"
	"testing"
)

// TestMain fences every pkg/weave test into a private home. Individual tests
// may create repositories and invoke real command handlers; none of those
// handlers may register a temporary repo in the operator's ~/.bashy/weave.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "coreutils-weave-test-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	if err := os.Setenv("USERPROFILE", home); err != nil {
		panic(err)
	}
	// Store overrides take precedence over HOME. Inheriting the conductor's
	// sprint store can expose its linked live queues even with a private HOME.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "BASHY_") && strings.HasSuffix(key, "_DIR") {
			if err := os.Unsetenv(key); err != nil {
				panic(err)
			}
		}
	}
	// Let the default follow HOME when a fixture installs its own private
	// home; pinning a suite-wide BASHY_HOME would share sprint state across tests.
	if err := os.Unsetenv("BASHY_HOME"); err != nil {
		panic(err)
	}

	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
