package selfbin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	bashy := filepath.Join(dir, "bashy")
	if err := os.WriteFile(bashy, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "webconsole.test")
	if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	realBashy, _ := filepath.EvalSymlinks(bashy)
	exe := func(p string) func() (string, error) { return func() (string, error) { return p, nil } }

	if got := resolve("/opt/custom", exe(bashy)); got != "/opt/custom" {
		t.Errorf("configured must win, got %q", got)
	}
	if got := resolve("", exe(bashy)); got != realBashy {
		t.Errorf("running bashy: got %q want %q", got, realBashy)
	}
	if got := resolve("", exe(other)); got != "bashy" {
		t.Errorf("non-bashy exe: got %q", got)
	}
	if got := resolve("", func() (string, error) { return "", errors.New("x") }); got != "bashy" {
		t.Errorf("error: got %q", got)
	}
}
