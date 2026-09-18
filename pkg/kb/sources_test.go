package kb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixtureStores lays out fake private-memory stores under home and a
// fake repo (with a graph contrib log) and returns (home, repoCwd).
func writeFixtureStores(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BASHY_KB_DIR", filepath.Join(home, "kb"))
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_SKILLS_DIR", filepath.Join(home, "skills"))
	t.Setenv("YCODE_DATA_DIR", filepath.Join(home, "ycode"))
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	claude := filepath.Join(home, ".claude", "projects", "proj-1", "memory")
	write(filepath.Join(claude, "fact-a.md"), "---\nname: a\n---\nA.\n")
	write(filepath.Join(claude, "fact-b.md"), "---\nname: b\n---\nB.\n")
	write(filepath.Join(claude, "MEMORY.md"), "- index line\n") // index, not an entry
	write(filepath.Join(home, ".agents", "ycode", "memory", "note.md"), "---\nname: n\n---\nN.\n")
	write(filepath.Join(home, ".bashy", "weave", "repo-abc123", "memory.jsonl"),
		`{"issue_id":1,"summary":"one"}`+"\n"+`{"issue_id":2,"summary":"two"}`+"\n")

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(repo, RepoSub, RelationFile),
		`{"op":"note","target":"x","text":"t1"}`+"\n"+
			`{"op":"note","target":"y","text":"t2"}`+"\n"+
			`{"op":"note","target":"z","text":"t3"}`+"\n")
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, sub
}

func TestDetectSources(t *testing.T) {
	home, cwd := writeFixtureStores(t)
	got := map[string]SourceInfo{}
	for _, s := range DetectSources(home, cwd) {
		if s.Present {
			got[s.Name+"|"+s.Path] = s
		}
	}
	wantEntries := map[string]int{"claude-memory": 2, "memex": 1, "weave-memory": 2, "repo-graph": 3}
	found := map[string]int{}
	for _, s := range got {
		found[s.Name] += s.Entries
		if s.Format == "" || s.Newest == "" {
			t.Errorf("%s: missing format/newest: %+v", s.Name, s)
		}
	}
	for name, want := range wantEntries {
		if found[name] != want {
			t.Errorf("%s: entries = %d, want %d", name, found[name], want)
		}
	}
}

func TestDetectSourcesAbsent(t *testing.T) {
	// Empty home, cwd not in a repo: every known store reported absent, no error.
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("BASHY_KB_DIR", filepath.Join(home, "kb"))
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_SKILLS_DIR", filepath.Join(home, "skills"))
	t.Setenv("YCODE_DATA_DIR", filepath.Join(home, "ycode"))
	for _, s := range DetectSources(home, cwd) {
		if s.Present {
			t.Errorf("unexpected present store in empty fixture: %+v", s)
		}
	}
}

func TestTransferredCounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_KB_DIR", filepath.Join(home, "kb"))
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_SKILLS_DIR", filepath.Join(home, "skills"))
	t.Setenv("YCODE_DATA_DIR", filepath.Join(home, "ycode"))
	pages := []*Page{
		{Slug: "a", Status: StatusCandidate, Tags: []string{"xfer:claude-memory", "outpost"}},
		{Slug: "b", Status: StatusValidated, Tags: []string{"XFER:Claude-Memory"}}, // case-folded
		{Slug: "c", Status: StatusCandidate, Tags: []string{"xfer:recall"}},
		{Slug: "d", Status: StatusSuperseded, Tags: []string{"xfer:recall"}}, // superseded excluded
		{Slug: "e", Status: StatusCandidate, Tags: []string{"xfer:"}},        // empty suffix ignored
	}
	got := TransferredCounts(pages)
	if got["claude-memory"] != 2 || got["recall"] != 1 || len(got) != 2 {
		t.Fatalf("TransferredCounts = %v", got)
	}
}

func TestSourcesCmd(t *testing.T) {
	home, cwd := writeFixtureStores(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows os.UserHomeDir
	t.Chdir(cwd)

	dir := t.TempDir()
	mustRun(t, dir, "add",
		"--type", "fact", "--title", "outpost orientation",
		"--description", "WHEN touching outpost",
		"--tags", "xfer:claude-memory")

	out := mustRun(t, dir, "sources")
	for _, want := range []string{"claude-memory", "memex", "weave-memory", "repo-graph", "already transferred", "claude-memory=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("sources output missing %q:\n%s", want, out)
		}
	}

	jsonOut := mustRun(t, dir, "sources", "--json")
	var payload struct {
		Sources     []SourceInfo   `json:"sources"`
		Transferred map[string]int `json:"transferred"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil {
		t.Fatalf("sources --json: %v\n%s", err, jsonOut)
	}
	if len(payload.Sources) == 0 || payload.Transferred["claude-memory"] != 1 {
		t.Fatalf("sources --json payload: %+v", payload)
	}
}

// snapshotDir maps every file under root to its content — the byte-identical
// pin for the read-only rule.
func snapshotDir(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		snap[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestSourcesAndTransferAreReadOnly pins the hard rule: kb reads foreign
// stores and its own store, and neither verb writes ANYTHING — not the kb
// store, not the source stores.
func TestSourcesAndTransferAreReadOnly(t *testing.T) {
	home, cwd := writeFixtureStores(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(cwd)

	dir := t.TempDir()
	mustRun(t, dir, "add",
		"--type", "gotcha", "--title", "upgrade no-ops on same commit",
		"--description", "WHEN deploying an outpost binary iteratively use force",
		"--tags", "xfer:claude-memory")

	before := snapshotDir(t, dir)
	beforeHome := snapshotDir(t, home)

	mustRun(t, dir, "sources")
	mustRun(t, dir, "sources", "--json")
	mustRun(t, dir, "transfer", "deploy outpost binary")
	mustRun(t, dir, "transfer")

	compare := func(label string, before, after map[string]string) {
		t.Helper()
		if len(before) != len(after) {
			t.Fatalf("%s: file count changed: %d -> %d", label, len(before), len(after))
		}
		for path, content := range before {
			if after[path] != content {
				t.Errorf("%s: %s changed", label, path)
			}
		}
	}
	compare("kb store", before, snapshotDir(t, dir))
	compare("source stores", beforeHome, snapshotDir(t, home))
}
