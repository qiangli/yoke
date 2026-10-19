package agentlaunch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexSocketDiagnostic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "app-server-control")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "app-server-control.sock")
	target := filepath.Join(t.TempDir(), "runtime.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := codexSocketDiagnostic(); err == nil || !strings.Contains(err.Error(), "missing runtime socket") {
		t.Fatalf("dangling socket diagnostic: %v", err)
	}
	if _, err := Resolve("codex", Options{}); err == nil || !strings.Contains(err.Error(), "missing runtime socket") {
		t.Fatalf("launch must explain dangling socket: %v", err)
	}
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := codexSocketDiagnostic(); err != nil {
		t.Fatalf("live target must be preserved: %v", err)
	}
}
