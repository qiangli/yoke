package skills

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDetectAgent(t *testing.T) {
	// The marker table lives in the fleet registry now; this test only needs a
	// clean environment before exercising the delegation.
	for _, env := range []string{
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CODEX_THREAD_ID",
		"GEMINI_CLI", "CURSOR_AGENT", "CURSOR_TRACE_ID", "GOOSE_TERMINAL",
		"OPENCODE_CLIENT", "CLINE_ACTIVE",
	} {
		t.Setenv(env, "")
	}
	for _, env := range []string{"AGENT", "AI_AGENT"} {
		t.Setenv(env, "")
	}
	if name, ok := DetectAgent(); ok {
		t.Fatalf("clean env detected %q", name)
	}
	t.Setenv("CLAUDECODE", "1")
	if name, ok := DetectAgent(); !ok || name != "claude" {
		t.Fatalf("CLAUDECODE → %q %v", name, ok)
	}
	t.Setenv("CLAUDECODE", "")
	t.Setenv("AGENT", "goose")
	if name, ok := DetectAgent(); !ok || name != "goose" {
		t.Fatalf("AGENT=goose → %q %v", name, ok)
	}
}

func exportFixture(t *testing.T) (*config, Skill, Source) {
	t.Helper()
	embedded := fstest.MapFS{
		"guide/SKILL.md":           {Data: []byte("---\nname: guide\ndescription: the guide\n---\nGUIDE BODY\n")},
		"guide/reference.md":       {Data: []byte("REFERENCE\n")},
		"guide/references/deep.md": {Data: []byte("DEEP\n")},
	}
	cfg := newTestConfig(t.TempDir())
	cfg.sources = []Source{EmbedSource(embedded, RingEmbedded)}
	sk, src, ok := cfg.catalog().Get("guide")
	if !ok {
		t.Fatal("guide not found")
	}
	return cfg, sk, src
}

func TestExportToAndOwnership(t *testing.T) {
	_, sk, src := exportFixture(t)
	root := t.TempDir()

	dst, err := ExportTo(sk, src, root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"SKILL.md", "reference.md", "references/deep.md", exportMarker} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Fatalf("missing %s", f)
		}
	}
	// Refresh of an owned export succeeds without force.
	if _, err := ExportTo(sk, src, root, false); err != nil {
		t.Fatalf("owned refresh: %v", err)
	}
	// A folder we did NOT write is never clobbered without --force.
	foreign := filepath.Join(t.TempDir(), sk.Name)
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "SKILL.md"), []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportTo(sk, src, filepath.Dir(foreign), false); err == nil {
		t.Fatal("clobbered a foreign skill folder")
	}
	if _, err := ExportTo(sk, src, filepath.Dir(foreign), true); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

func TestExportRoots(t *testing.T) {
	home := t.TempDir()
	// No vendor dirs: only the neutral root.
	roots := userExportRoots(home)
	if len(roots) != 1 || !strings.HasSuffix(roots[0], filepath.Join(".agents", "skills")) {
		t.Fatalf("roots = %v", roots)
	}
	// Detected vendor roots join in.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots = userExportRoots(home)
	if len(roots) != 2 || !strings.Contains(roots[1], ".claude") {
		t.Fatalf("roots = %v", roots)
	}

	repo := t.TempDir()
	rr := repoExportRoots(repo)
	if len(rr) != 1 {
		t.Fatalf("repo roots = %v", rr)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rr = repoExportRoots(repo); len(rr) != 2 {
		t.Fatalf("repo roots = %v", rr)
	}
}

func TestCLIExport(t *testing.T) {
	embedded := fstest.MapFS{
		"guide/SKILL.md": {Data: []byte("---\nname: guide\ndescription: the guide\n---\nBODY\n")},
	}
	f := &cobraRunner{t: t, opts: []Option{
		WithSource(EmbedSource(embedded, RingEmbedded)),
		WithConfigDir(t.TempDir()),
	}}
	out := t.TempDir()
	stdout, _, err := f.run("export", "guide", "--to", out)
	if err != nil || !strings.Contains(stdout, "exported: ") {
		t.Fatalf("export: %v\n%s", err, stdout)
	}
	if _, err := os.Stat(filepath.Join(out, "guide", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	// No target → usage error.
	if _, _, err := f.run("export", "guide"); err == nil {
		t.Fatal("export with no target succeeded")
	}
}

func TestProvision(t *testing.T) {
	embedded := fstest.MapFS{
		"guide/SKILL.md": {Data: []byte("---\nname: guide\ndescription: the guide\n---\nBODY\n")},
	}
	ws := t.TempDir()
	var log strings.Builder
	Provision(ws, []string{"guide", "missing"}, &log,
		WithSource(EmbedSource(embedded, RingEmbedded)), WithConfigDir(t.TempDir()))
	for _, p := range []string{
		filepath.Join(ws, ".agents", "skills", "guide", "SKILL.md"),
		filepath.Join(ws, ".claude", "skills", "guide", "SKILL.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s", p)
		}
	}
	if !strings.Contains(log.String(), "missing") || !strings.Contains(log.String(), "provisioned") {
		t.Fatalf("log: %s", log.String())
	}
	// Idempotent re-provision.
	Provision(ws, []string{"guide"}, io.Discard,
		WithSource(EmbedSource(embedded, RingEmbedded)), WithConfigDir(t.TempDir()))
}
