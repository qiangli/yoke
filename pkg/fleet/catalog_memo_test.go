package fleet

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

// memoLoads reports how many times the catalog rebuilt a noun from disk.
func memoLoads(c *Catalog) int {
	c.memo.mu.Lock()
	defer c.memo.mu.Unlock()
	return c.memo.loads
}

func writeMemoRecord(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Repeated lookups parse the rings once; a direct write to the local store
// (not through the catalog) is noticed on the next lookup, and a lookup after
// that is a hit again.
func TestCatalogMemoizesUntilTheRingChanges(t *testing.T) {
	root := t.TempDir()
	base := fstest.MapFS{
		baselineRoot + "/tools/codex.yaml": {Data: []byte("name: codex\nkind: cli\ncli:\n  binary: codex\n")},
	}
	c := New(WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay())

	for i := 0; i < 50; i++ {
		if _, ok := c.Tool("codex"); !ok {
			t.Fatalf("lookup %d: codex not found", i)
		}
	}
	if got := memoLoads(c); got != 1 {
		t.Fatalf("50 lookups rebuilt the tools ring %d times, want 1", got)
	}

	// A new record written straight to disk, as tests and operators do.
	writeMemoRecord(t, filepath.Join(root, "tools"), "mytool", "name: mytool\nkind: cli\ncli:\n  binary: mytool\n")
	if _, ok := c.Tool("mytool"); !ok {
		t.Fatal("a record written after the first lookup was not seen")
	}
	if got := memoLoads(c); got != 2 {
		t.Fatalf("after one write the tools ring was rebuilt %d times in all, want 2", got)
	}

	// An in-place rewrite of an existing record (same name, new content).
	// Wait past the filesystem's timestamp resolution so the change is not
	// hidden behind an equal mtime on a coarse filesystem.
	time.Sleep(20 * time.Millisecond)
	writeMemoRecord(t, filepath.Join(root, "tools"), "mytool", "name: mytool\nkind: cli\ncli:\n  binary: mytool-v2\n")
	tool, ok := c.Tool("mytool")
	if !ok {
		t.Fatal("rewritten record vanished")
	}
	if tool.CLI.Binary != "mytool-v2" {
		t.Fatalf("in-place rewrite not seen: binary=%q", tool.CLI.Binary)
	}
	if got := memoLoads(c); got != 3 {
		t.Fatalf("after a rewrite the tools ring was rebuilt %d times in all, want 3", got)
	}

	// Quiet again: hits only.
	for i := 0; i < 20; i++ {
		c.Tool("mytool")
	}
	if got := memoLoads(c); got != 3 {
		t.Fatalf("quiet lookups rebuilt the ring: %d loads, want 3", got)
	}
}

// Results are copies: a caller that appends to the returned slice cannot
// leak its extra entry into the next caller's view.
func TestCatalogMemoReturnsCopies(t *testing.T) {
	root := t.TempDir()
	base := fstest.MapFS{
		baselineRoot + "/tools/codex.yaml": {Data: []byte("name: codex\nkind: cli\ncli:\n  binary: codex\n")},
	}
	c := New(WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay())
	first, _ := c.Tools(true)
	first = append(first, Tool{Name: "ghost"})
	first[0].Name = "mutated"
	second, _ := c.Tools(true)
	if len(second) != 1 || second[0].Name != "codex" {
		t.Fatalf("second call saw the first caller's edits: %+v", second)
	}
}
