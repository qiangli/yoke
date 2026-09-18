package foreman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/recall"
)

func isolateForemanKBStores(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BASHY_KB_DIR", filepath.Join(root, "host-kb"))
	t.Setenv("BASHY_HOME", filepath.Join(root, "bashy-home"))
	t.Setenv("BASHY_SKILLS_DIR", filepath.Join(root, "skills"))
	t.Setenv("YCODE_DATA_DIR", filepath.Join(root, "ycode"))
	return root
}

func TestComposeKBNoteUsesContextBudget(t *testing.T) {
	root := isolateForemanKBStores(t)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, kb.RepoSub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	if err := kb.Open(os.Getenv("BASHY_KB_DIR")).Write(&kb.Page{
		Slug: "bounded-widget", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "bounded widget", Description: strings.Repeat("widget guidance ", 200),
	}, "add"); err != nil {
		t.Fatal(err)
	}
	got := composeKBNote("bounded widget")
	if got == "" || !strings.Contains(got, "kb:bounded-widget") {
		t.Fatalf("foreman preamble did not use assembled context: %q", got)
	}
	if len(got) > recall.PreambleBudget {
		t.Fatalf("foreman preamble is %d bytes, budget %d", len(got), recall.PreambleBudget)
	}
}
