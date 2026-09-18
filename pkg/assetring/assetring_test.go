package assetring

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestRingString(t *testing.T) {
	for _, tt := range []struct {
		r    Ring
		want string
	}{
		{RingEmbedded, "embedded"},
		{RingShared, "shared"},
		{RingLocal, "local"},
		{RingCloud, "cloud"},
	} {
		if got := tt.r.String(); got != tt.want {
			t.Errorf("Ring(%d).String() = %q, want %q", tt.r, got, tt.want)
		}
	}
}

// RingCloud was appended, not inserted: no persisted Ring value shifted.
func TestRingValuesAreStable(t *testing.T) {
	if RingEmbedded != 0 || RingShared != 1 || RingLocal != 2 || RingCloud != 3 {
		t.Fatalf("ring values moved: %d %d %d %d", RingEmbedded, RingShared, RingLocal, RingCloud)
	}
}

func TestFolderFS(t *testing.T) {
	fsys := fstest.MapFS{
		"alpha/SKILL.md":     {Data: []byte("alpha body")},
		"alpha/reference.md": {Data: []byte("ref")},
		"beta/SKILL.md":      {Data: []byte("beta body")},
		"nomarker/other.md":  {Data: []byte("ignored")},
		".hidden/SKILL.md":   {Data: []byte("ignored")},
	}
	s := FolderFS(fsys, RingEmbedded, "SKILL.md")

	names, err := s.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("Names() = %v, want [alpha beta] (marker-less and dot dirs skipped)", names)
	}
	if b, ok := s.Body("alpha"); !ok || string(b) != "alpha body" {
		t.Fatalf("Body(alpha) = %q, %v", b, ok)
	}
	if b, ok := s.File("alpha", "reference.md"); !ok || string(b) != "ref" {
		t.Fatalf("File(alpha, reference.md) = %q, %v", b, ok)
	}
	if _, ok := s.File("alpha", "../beta/SKILL.md"); ok {
		t.Fatal("traversal escaped the entry")
	}
	files, err := s.Files("alpha")
	if err != nil || len(files) != 2 {
		t.Fatalf("Files(alpha) = %v, %v", files, err)
	}
}

func TestFileFS(t *testing.T) {
	fsys := fstest.MapFS{
		"codex.yaml":   {Data: []byte("name: codex")},
		"claude.yaml":  {Data: []byte("name: claude")},
		"notes.md":     {Data: []byte("ignored")},
		".hidden.yaml": {Data: []byte("ignored")},
	}
	s := FileFS(fsys, RingEmbedded, ".yaml")

	names, err := s.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "claude" || names[1] != "codex" {
		t.Fatalf("Names() = %v, want [claude codex]", names)
	}
	if b, ok := s.Body("codex"); !ok || string(b) != "name: codex" {
		t.Fatalf("Body(codex) = %q, %v", b, ok)
	}
	// A single-file entry has no siblings.
	if _, ok := s.File("codex", "anything"); ok {
		t.Fatal("File() must be false for a file source")
	}
	if _, ok := s.Body("../codex"); ok {
		t.Fatal("traversal escaped the source")
	}
	files, _ := s.Files("codex")
	if len(files) != 1 || files[0] != "codex.yaml" {
		t.Fatalf("Files(codex) = %v", files)
	}
}

// A missing directory is an empty source, not an error — an unpaired
// host with no local store must still list its embedded baseline.
func TestMissingDirIsEmptyNotError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	for _, s := range []Source{
		FolderDir(missing, RingLocal, "SKILL.md"),
		FileDir(missing, RingLocal, ".yaml"),
	} {
		names, err := s.Names()
		if err != nil || len(names) != 0 {
			t.Fatalf("Names() = %v, %v; want empty, nil", names, err)
		}
		if _, ok := s.Body("x"); ok {
			t.Fatal("Body() found an entry in a missing dir")
		}
	}
}

func TestFileDirRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex.yaml"), []byte("name: codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := FileDir(dir, RingLocal, ".yaml")
	if got := s.(DirSourcer).Dir(); got != dir {
		t.Fatalf("Dir() = %q, want %q", got, dir)
	}
	b, ok := s.Body("codex")
	if !ok || string(b) != "name: codex\n" {
		t.Fatalf("Body(codex) = %q, %v — the file bytes ARE the entry body", b, ok)
	}
}

// The merge order IS the precedence order: the last source wins, and the
// win is reported as a shadow. A local entry must beat a cloud overlay,
// which beats the compiled-in baseline.
func TestCatalogLastSourceWinsAndReportsShadow(t *testing.T) {
	embedded := FileFS(fstest.MapFS{
		"codex.yaml":  {Data: []byte("embedded")},
		"claude.yaml": {Data: []byte("embedded")},
	}, RingEmbedded, ".yaml")
	cloud := FileFS(fstest.MapFS{"codex.yaml": {Data: []byte("cloud")}}, RingCloud, ".yaml")
	local := FileFS(fstest.MapFS{"codex.yaml": {Data: []byte("local")}}, RingLocal, ".yaml")

	type entry struct {
		body string
		ring Ring
	}
	c := &Catalog[entry]{
		Sources: []Source{embedded, cloud, local},
		Parse:   func(_ string, b []byte, s Source) entry { return entry{string(b), s.Ring()} },
	}

	got, src, ok := c.Get("codex")
	if !ok || got.body != "local" || src.Ring() != RingLocal {
		t.Fatalf("Get(codex) = %+v (ring %v); local must shadow cloud and embedded", got, src.Ring())
	}

	rows, err := c.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "claude" || rows[1].Name != "codex" {
		t.Fatalf("Rows() = %v, want name-sorted [claude codex]", rows)
	}
	if rows[1].Entry.body != "local" || !rows[1].Shadows {
		t.Fatalf("codex row = %+v, want local body with Shadows=true", rows[1])
	}
	if rows[0].Shadows {
		t.Fatal("claude exists in one ring only; it shadows nothing")
	}
}

// A noun with no compiled-in baseline (hosts, people) yields an fs.FS whose
// root does not exist. That is an empty ring, not a broken one — treating it
// as an error made every entry of that kind silently unresolvable.
func TestMissingEmbeddedSubdirIsEmptyNotError(t *testing.T) {
	root := fstest.MapFS{"baseline/tools/codex.yaml": {Data: []byte("name: codex")}}
	missing, err := fs.Sub(root, "baseline/people")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Source{
		FileFS(missing, RingEmbedded, ".yaml"),
		FolderFS(missing, RingEmbedded, "SKILL.md"),
	} {
		names, err := s.Names()
		if err != nil || len(names) != 0 {
			t.Fatalf("Names() = %v, %v; want empty, nil", names, err)
		}
	}

	// And a catalog over such a source still lists the rings that do exist.
	local := FileFS(fstest.MapFS{"alice.yaml": {Data: []byte("handle: alice")}}, RingLocal, ".yaml")
	c := &Catalog[string]{
		Sources: []Source{FileFS(missing, RingEmbedded, ".yaml"), local},
		Parse:   func(n string, _ []byte, _ Source) string { return n },
	}
	rows, err := c.Rows()
	if err != nil {
		t.Fatalf("Rows() over a missing embedded ring failed: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "alice" {
		t.Fatalf("Rows() = %v, want the one local entry", rows)
	}
}

func TestCatalogGetMissing(t *testing.T) {
	c := &Catalog[string]{
		Sources: []Source{FileFS(fstest.MapFS{}, RingEmbedded, ".yaml")},
		Parse:   func(_ string, b []byte, _ Source) string { return string(b) },
	}
	if _, _, ok := c.Get("nope"); ok {
		t.Fatal("Get() found a missing entry")
	}
}

func TestExitCode(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d, want 0", got)
	}
	for _, msg := range []string{"unknown command \"x\"", "unknown flag: --x", "accepts 1 arg(s)", "invalid argument"} {
		if got := ExitCode(errString(msg)); got != 2 {
			t.Errorf("ExitCode(%q) = %d, want 2 (usage)", msg, got)
		}
	}
	if got := ExitCode(errString("disk on fire")); got != 1 {
		t.Errorf("ExitCode(runtime error) = %d, want 1", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
