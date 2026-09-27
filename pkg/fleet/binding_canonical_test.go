package fleet

// Sprint: #290 (operator 2026-09-27: pooled vendors run through <tool>:<model> bindings)

import (
	"os"
	"path/filepath"
	"testing"
)

// A tool:model binding shared by seat clones still names the canonical agent;
// without one it stays ambiguous (never a guess).
func TestBindingSharedByClonesNamesTheCanonicalAgent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, tool, model string) {
		if err := os.WriteFile(filepath.Join(root, "agents", name+".yaml"),
			[]byte("name: "+name+"\ntool: "+tool+"\nmodel: "+model+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("codex-gpt-5.5", "codex", "gpt-5.5")
	write("sprint209-manager", "codex", "gpt-5.5")
	write("seat-a", "claude", "sonnet5")
	write("seat-b", "claude", "sonnet5")
	c := New(WithRoot(root))

	a, ok := c.Agent("codex:gpt-5.5")
	if !ok || a.Name != "codex-gpt-5.5" {
		t.Fatalf("Agent(codex:gpt-5.5) = %q, %v; want the canonical codex-gpt-5.5", a.Name, ok)
	}
	if a, ok := c.Agent("sprint209-manager"); !ok || a.Name != "sprint209-manager" {
		t.Errorf("a clone must stay reachable by its own name: %q, %v", a.Name, ok)
	}
	if a, ok := c.Agent("claude:sonnet5"); ok && (a.Name == "seat-a" || a.Name == "seat-b") {
		t.Errorf("Agent(claude:sonnet5) guessed a clone %q", a.Name)
	}
}
