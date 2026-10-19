package python

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envGet(env []string, k string) string {
	v := ""
	for _, e := range env {
		if x, ok := strings.CutPrefix(e, k+"="); ok {
			v = x
		}
	}
	return v
}

func TestFrontDoorEnv(t *testing.T) {
	t.Setenv("BASHY_PYTHON_VERSION", "")
	root := t.TempDir()
	child := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	env := frontDoorEnv(nil, child)
	if got := envGet(env, "UV_PYTHON"); got != DefaultPython {
		t.Errorf("default UV_PYTHON = %q, want %q", got, DefaultPython)
	}
	if got := envGet(env, "UV_PYTHON_PREFERENCE"); got != "only-managed" {
		t.Errorf("preference = %q", got)
	}

	t.Setenv("BASHY_PYTHON_VERSION", "3.12")
	if got := envGet(frontDoorEnv(nil, child), "UV_PYTHON"); got != "3.12" {
		t.Errorf("BASHY_PYTHON_VERSION: UV_PYTHON = %q", got)
	}
	t.Setenv("BASHY_PYTHON_VERSION", "")

	if err := os.WriteFile(filepath.Join(root, ".python-version"), []byte("3.14\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := envGet(frontDoorEnv(nil, child), "UV_PYTHON"); got != "" {
		t.Errorf(".python-version in parent: UV_PYTHON = %q, want unset", got)
	}

	env = frontDoorEnv([]string{"UV_PYTHON_PREFERENCE=system"}, child)
	if got := envGet(env, "UV_PYTHON_PREFERENCE"); got != "system" {
		t.Errorf("caller preference overridden: %q", got)
	}
}
