package agentctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// claude looks the project up by its symlink-resolved cwd (/tmp is
// /private/tmp on macOS); a preseed under the unresolved path alone leaves the
// trust dialog up with "No, exit" selected.
func TestClaudeTrustPreseedMarksTheResolvedPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink:", err)
	}
	if err := ApplyTrustPreseed(link, ".claude.json"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(real)
	for _, key := range []string{link, resolved} {
		if doc.Projects[key]["hasTrustDialogAccepted"] != true {
			t.Errorf("projects[%q] not trusted: %s", key, b)
		}
	}
}
