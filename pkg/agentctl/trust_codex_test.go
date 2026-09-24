package agentctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func codexHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	return filepath.Join(dir, "config.toml")
}

// codex keys trust on the repo root, not the launch directory; a preseed under
// any other key leaves the dialog up.
func TestCodexTrustPreseedAppendsTheRepoRootOnce(t *testing.T) {
	cfg := codexHome(t)
	before := "model = \"gpt-x\"\n\n[tui]\nscreen_reader_detection_done = true"
	if err := os.WriteFile(cfg, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	sub := filepath.Join(repo, "pkg", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: ../.git/modules/x\n"), 0o600); err != nil {
		t.Fatal(err) // a submodule's .git is a file
	}
	for i := 0; i < 2; i++ {
		if err := ApplyTrustPreseed(sub, "codex.toml"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(cfg)
	want := before + "\n\n[projects." + tomlBasicString(repo) + "]\ntrust_level = \"trusted\"\n"
	if string(got) != want {
		t.Fatalf("config.toml =\n%s\nwant\n%s", got, want)
	}
}

// The operator's own answer stands — including "no".
func TestCodexTrustPreseedLeavesAnExistingTableAlone(t *testing.T) {
	cfg := codexHome(t)
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	before := "[projects." + tomlBasicString(ws) + "]\ntrust_level = \"untrusted\"\n"
	if err := os.WriteFile(cfg, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyTrustPreseed(ws, "codex.toml"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(cfg); string(got) != before {
		t.Fatalf("preseed rewrote an existing project table:\n%s", got)
	}
}

func TestCodexTrustPreseedCreatesAMissingConfig(t *testing.T) {
	cfg := codexHome(t)
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	if err := ApplyTrustPreseed(ws, "codex.toml"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfg)
	if !strings.HasPrefix(string(got), "[projects.") || !strings.Contains(string(got), `trust_level = "trusted"`) {
		t.Fatalf("config.toml = %q", got)
	}
}

func TestTOMLBasicStringEscapes(t *testing.T) {
	if got, want := tomlBasicString(`C:\work\"x"`), `"C:\\work\\\"x\""`; got != want {
		t.Fatalf("tomlBasicString = %s, want %s", got, want)
	}
}
