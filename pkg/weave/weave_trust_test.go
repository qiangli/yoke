package weave

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWeaveTrustClearPayload(t *testing.T) {
	got, ok := weaveTrustClearPayload("say:1")
	if !ok || got != "1" {
		t.Fatalf("weaveTrustClearPayload(say:1) = %q, %v; want 1, true", got, ok)
	}
	if got, ok := weaveTrustClearPayload(""); ok || got != "" {
		t.Fatalf("empty trust_clear = %q, %v; want no-op", got, ok)
	}
	if got, ok := weaveTrustClearPayload("press:1"); ok || got != "" {
		t.Fatalf("unsupported trust_clear = %q, %v; want no-op", got, ok)
	}
}

func TestWeaveTrustLaunchForDeclaredTools(t *testing.T) {
	claude := weaveTrustLaunchFor("claude")
	if claude.Preseed != ".claude.json" || claude.Clear != "say:1" {
		t.Fatalf("claude trust launch = %+v, want .claude.json and say:1", claude)
	}
	codex := weaveTrustLaunchFor("codex")
	if codex.Clear != "" {
		t.Fatalf("codex trust clear = %q, want empty no-op", codex.Clear)
	}
}

func TestWeavePreseedClaudeTrust(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := weaveApplyTrustPreseed(workspace, ".claude.json"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Projects map[string]map[string]bool `json:"projects"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(workspace)
	if !doc.Projects[abs]["hasTrustDialogAccepted"] || !doc.Projects[abs]["hasCompletedProjectOnboarding"] {
		t.Fatalf("claude preseed for %s = %#v", abs, doc.Projects[abs])
	}
}
