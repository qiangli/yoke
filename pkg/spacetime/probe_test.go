package spacetime

import (
	"runtime"
	"testing"
	"time"
)

func TestContextKeyGolden(t *testing.T) {
	// Byte-compatibility contract with the dhnt skill-CNL runtime's
	// ContextKey: sorted "name=value" lines joined by \n, sha256, "c"+hex.
	// Golden value computed independently of this implementation.
	got := ContextKey(map[string]string{"os": "linux", "arch": "arm64"})
	want := "c46a96dcaa5616e5a8e6663d2922e57af08a5fdd861a7dc1dd7745003396d792a"
	if got != want {
		t.Fatalf("ContextKey = %s, want %s", got, want)
	}
	// Order-independence.
	if ContextKey(map[string]string{"arch": "arm64", "os": "linux"}) != want {
		t.Fatal("ContextKey is order-dependent")
	}
}

func TestCoreProbes(t *testing.T) {
	ps := DefaultProbes(NopCache())
	core := ps.Core()
	if core["os"] != runtime.GOOS || core["arch"] != runtime.GOARCH {
		t.Errorf("os/arch = %q/%q", core["os"], core["arch"])
	}
	// Booleans are always present with true/false values.
	for _, name := range []string{"container", "tty"} {
		if v := core[name]; v != "true" && v != "false" {
			t.Errorf("probe %s = %q, want true/false", name, v)
		}
	}
	// libc: linux-only — omitted elsewhere, never empty.
	if v, ok := core["libc"]; runtime.GOOS != "linux" && ok {
		t.Errorf("libc present on %s: %q", runtime.GOOS, v)
	}
	for name, v := range core {
		if v == "" {
			t.Errorf("probe %s has empty value (must be omitted instead)", name)
		}
	}
}

func TestSetStatic(t *testing.T) {
	ps := DefaultProbes(NopCache())
	ps.SetStatic("bashy", "0.9.1")
	if v, ok := ps.Value("bashy"); !ok || v != "0.9.1" {
		t.Fatalf("bashy = %q, %v", v, ok)
	}
	ps.SetStatic("bashy", "")
	if _, ok := ps.Value("bashy"); ok {
		t.Fatal("bashy probe not removed")
	}
}

func TestLazyProbeCaching(t *testing.T) {
	calls := 0
	ps := DefaultProbes(NopCache())
	ps.Register(countingResolver{&calls})
	if v, _ := ps.Value("fake.thing"); v != "present" {
		t.Fatalf("fake.thing = %q", v)
	}
	// NopCache: second call re-evaluates.
	ps.Value("fake.thing")
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (NopCache)", calls)
	}

	calls = 0
	dir := t.TempDir()
	fc := NewFileCache(dir, time.Hour)
	ps2 := DefaultProbes(fc)
	ps2.Register(countingResolver{&calls})
	ps2.Value("fake.thing")
	ps2.Value("fake.thing") // in-process second read hits the file cache
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (FileCache)", calls)
	}
	// A fresh ProbeSet sharing the cache dir also hits it.
	ps3 := DefaultProbes(NewFileCache(dir, time.Hour))
	ps3.Register(countingResolver{&calls})
	ps3.Value("fake.thing")
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (shared FileCache)", calls)
	}
}

type countingResolver struct{ n *int }

func (countingResolver) Namespace() string { return "fake" }
func (c countingResolver) Eval(string) (string, error) {
	*c.n++
	return "present", nil
}

func TestMeshReserved(t *testing.T) {
	ps := DefaultProbes(NopCache())
	if _, ok := ps.Value("mesh.paired"); ok {
		t.Fatal("mesh.* must be not-applicable in P0")
	}
}
