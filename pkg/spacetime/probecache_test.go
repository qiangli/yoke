package spacetime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileCacheTTL(t *testing.T) {
	dir := t.TempDir()
	fc := NewFileCache(dir, time.Hour)
	now := time.Now()
	fc.SetNow(func() time.Time { return now })

	fc.Put("tool.zzz", "h1", "1.2.3")
	if v, ok := fc.Get("tool.zzz", "h1"); !ok || v != "1.2.3" {
		t.Fatalf("Get = %q, %v", v, ok)
	}
	// Expired.
	fc.SetNow(func() time.Time { return now.Add(2 * time.Hour) })
	if _, ok := fc.Get("tool.zzz", "h1"); ok {
		t.Fatal("expired entry served")
	}
}

func TestFileCachePathHashInvalidation(t *testing.T) {
	fc := NewFileCache(t.TempDir(), time.Hour)
	fc.Put("tool.zzz", "hash-a", "1.0")
	if _, ok := fc.Get("tool.zzz", "hash-b"); ok {
		t.Fatal("entry served across PATH-hash change")
	}
	// Writing under the new hash drops the old world entirely.
	fc.Put("tool.yyy", "hash-b", "2.0")
	if _, ok := fc.Get("tool.zzz", "hash-b"); ok {
		t.Fatal("old-hash entry survived rewrite")
	}
}

func TestFileCacheCorruptSelfHeal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probecache.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	fc := NewFileCache(dir, time.Hour)
	if _, ok := fc.Get("tool.zzz", "h"); ok {
		t.Fatal("corrupt cache returned a value")
	}
	fc.Put("tool.zzz", "h", "1.0")
	if v, ok := fc.Get("tool.zzz", "h"); !ok || v != "1.0" {
		t.Fatalf("cache did not self-heal: %q, %v", v, ok)
	}
}
