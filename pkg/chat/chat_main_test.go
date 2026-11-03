package chat

import (
	"os"
	"path/filepath"
	"testing"
)

// isolatedRealHome holds the HOME value before TestMain overrode it, so
// tests can assert the real shim dir was not poisoned.
var isolatedRealHome string
var isolatedRealShimDir string

// TestMain isolates shim creation from the real HOME.
//
// Without this, any test that reaches agentChildEnv → ensureShims →
// WriteShellShim would write ~/.bashy/shims/{sh,bash,zsh} pointing at
// os.Executable() — which under `go test` is a *.test binary under the Go
// build cache. After that binary is deleted the host's sh is broken.
func TestMain(m *testing.M) {
	// Capture the real home before we clobber it.
	isolatedRealHome = os.Getenv("HOME")
	if isolatedRealHome == "" {
		if h, err := os.UserHomeDir(); err == nil {
			isolatedRealHome = h
		}
	}
	if isolatedRealHome != "" {
		isolatedRealShimDir = filepath.Join(isolatedRealHome, ".bashy", "shims")
	}
	home, err := os.MkdirTemp("/tmp", "yoke-chat-home-*")
	if err != nil {
		panic(err)
	}
	shims, err := os.MkdirTemp("/tmp", "yoke-chat-shims-*")
	if err != nil {
		panic(err)
	}
	// Ensure both HOME and the explicit override point at temp locations.
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	if err := os.Setenv("USERPROFILE", home); err != nil {
		panic(err)
	}
	if err := os.Setenv("BASHY_SHIM_DIR", filepath.Join(shims, "shims")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	_ = os.RemoveAll(shims)
	os.Exit(code)
}
