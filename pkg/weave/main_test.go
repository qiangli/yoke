package weave

import (
	"os"
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

	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
