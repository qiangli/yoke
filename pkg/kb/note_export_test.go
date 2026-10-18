package kb

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findPage locates a written page under dir, wherever the ring placed it.
func findPage(t *testing.T, dir, slug string) *Page {
	t.Helper()
	var found *Page
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == slug+".md" {
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if found, rerr = ParsePage(slug, b); rerr != nil {
				t.Fatal(rerr)
			}
		}
		return nil
	})
	if found == nil {
		t.Fatalf("page %s not written under %s", slug, dir)
	}
	return found
}

// TestNoteAddExportAndLineage pins --export (agent ring only) and the parent
// edge a child episode stamps on its notes and journal events.
func TestNoteAddExportAndLineage(t *testing.T) {
	t.Setenv("BASHY_EPISODE", "ep-child")
	t.Setenv("BASHY_PARENT_EPISODE", "ep-parent")
	dir := t.TempDir()
	mustRun(t, dir, "--ring", "agent", "note", "add", "--candidate", "--export", "--title", "shared note", "--body", "b")
	p := findPage(t, dir, "shared-note")
	if !p.Export || p.Source == nil || p.Source.Episode != "ep-child" || p.Source.ParentEpisode != "ep-parent" {
		t.Fatalf("export/lineage not recorded: export=%v source=%+v", p.Export, p.Source)
	}
	mustRun(t, dir, "--ring", "agent", "note", "add", "--candidate", "--title", "private note", "--body", "b")
	if q := findPage(t, dir, "private-note"); q.Export {
		t.Fatal("note without --export was exported")
	}
	mustRun(t, dir, "--ring", "agent", "observe", "--episode", "ep-child", "--kind", "note", "--ref", "r")
	journal, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(journal)), "\n") {
		if !strings.Contains(line, `"parent_episode":"ep-parent"`) {
			t.Errorf("journal line lacks parent_episode: %s", line)
		}
	}
	// A resolved shareable ring refuses --export (no --dir: that bypasses rings).
	t.Setenv("BASHY_KB_DIR", t.TempDir())
	host := NewKBCmd()
	host.SetOut(io.Discard)
	host.SetErr(io.Discard)
	host.SetArgs([]string{"--ring", "host", "note", "add", "--candidate", "--export", "--title", "x", "--body", "b"})
	if err := host.Execute(); err == nil || !strings.Contains(err.Error(), "--export") {
		t.Fatalf("--export on the host ring: err=%v, want refusal", err)
	}
}

// TestNoteAddRootEpisodeOmitsLineage pins omitempty: a root episode writes no
// parent edge and no export key.
func TestNoteAddRootEpisodeOmitsLineage(t *testing.T) {
	t.Setenv("BASHY_EPISODE", "ep-root")
	t.Setenv("BASHY_PARENT_EPISODE", "")
	dir := t.TempDir()
	mustRun(t, dir, "--ring", "agent", "note", "add", "--candidate", "--title", "root note", "--body", "b")
	b, err := os.ReadFile(filepath.Join(dir, "pages", "root-note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "parent_episode") || strings.Contains(s, "export") {
		t.Fatalf("root note carries lineage keys:\n%s", s)
	}
}
