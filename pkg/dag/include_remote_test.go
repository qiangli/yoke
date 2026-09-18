package dag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRemoteInclude(t *testing.T) {
	got, err := parseRemoteInclude("gh:qiangli/bashy@v0.19.0/ci.dag.md")
	if err != nil {
		t.Fatalf("parseRemoteInclude: %v", err)
	}
	want := remoteSpec{Owner: "qiangli", Repo: "bashy", Ref: "v0.19.0", Path: "ci.dag.md"}
	if got != want {
		t.Errorf("parsed = %+v, want %+v", got, want)
	}
	if u := got.rawURL(); u != "https://raw.githubusercontent.com/qiangli/bashy/v0.19.0/ci.dag.md" {
		t.Errorf("rawURL = %q", u)
	}
}

func TestParseRemoteIncludeNestedPath(t *testing.T) {
	got, err := parseRemoteInclude("gh:owner/repo@abc1234/ci/shared.dag.md")
	if err != nil {
		t.Fatalf("parseRemoteInclude: %v", err)
	}
	if got.Path != "ci/shared.dag.md" || got.Ref != "abc1234" {
		t.Errorf("parsed = %+v", got)
	}
}

// The pin is the whole point: without it a shared graph can change under every
// dependent repo with no commit in that repo. These must fail closed.
func TestParseRemoteIncludeRejectsUnpinned(t *testing.T) {
	for _, bad := range []string{
		"gh:qiangli/bashy/ci.dag.md",    // no @ref
		"gh:qiangli@v1/ci.dag.md",       // no repo
		"gh:qiangli/bashy@v1",           // no path
		"gh:qiangli/bashy@/ci.dag.md",   // empty ref
		"https://example.com/ci.dag.md", // unpinnable
		"http://example.com/ci.dag.md",  // unpinnable
	} {
		if _, err := parseRemoteInclude(bad); err == nil {
			t.Errorf("parseRemoteInclude(%q) should fail closed", bad)
		}
	}
}

func TestRemoteIncludeErrorsAreActionable(t *testing.T) {
	_, err := parseRemoteInclude("gh:qiangli/bashy/ci.dag.md")
	if err == nil || !strings.Contains(err.Error(), "gh:owner/repo@ref/path") {
		t.Errorf("error should show the expected form, got %v", err)
	}
	_, err = parseRemoteInclude("https://example.com/x.md")
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Errorf("bare-URL error should explain pinning, got %v", err)
	}
}

// The cache key must include the ref — two pins of the same file are two
// different inputs, and collapsing them would defeat pinning.
func TestRemoteIncludeCacheKeyIncludesRef(t *testing.T) {
	t.Setenv("DAG_CACHE_DIR", t.TempDir())
	a, _ := parseRemoteInclude("gh:o/r@v1/ci.dag.md")
	b, _ := parseRemoteInclude("gh:o/r@v2/ci.dag.md")
	pa, err := a.cachePath()
	if err != nil {
		t.Fatal(err)
	}
	pb, err := b.cachePath()
	if err != nil {
		t.Fatal(err)
	}
	if pa == pb {
		t.Errorf("different refs share a cache path: %q", pa)
	}
	if a.key() == b.key() {
		t.Errorf("different refs share a dedupe key: %q", a.key())
	}
}

// withFetcher swaps the injectable fetcher and counts calls.
func withFetcher(t *testing.T, body string) *int {
	t.Helper()
	calls := 0
	prev := remoteFetcher
	remoteFetcher = func(string) ([]byte, error) {
		calls++
		return []byte(body), nil
	}
	t.Cleanup(func() { remoteFetcher = prev })
	return &calls
}

func TestRemoteIncludeMergesTargets(t *testing.T) {
	t.Setenv("DAG_CACHE_DIR", t.TempDir())
	calls := withFetcher(t, "## Tasks\n\n### compile\n"+block("bash", "echo compile"))

	dir := t.TempDir()
	main := "---\ninclude: gh:qiangli/bashy@v0.19.0/ci.dag.md\n---\n\n## Tasks\n\n" +
		"### build\nRequires: compile\n" + block("bash", "echo build")
	if err := os.WriteFile(filepath.Join(dir, "DAG.md"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := ParseFile(filepath.Join(dir, "DAG.md"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, ok := doc.Lookup("compile"); !ok {
		t.Fatalf("remote target not merged; order=%v", doc.Order)
	}
	if _, err := BuildGraph(doc); err != nil {
		t.Errorf("BuildGraph across remote include: %v", err)
	}
	if *calls != 1 {
		t.Errorf("expected 1 fetch, got %d", *calls)
	}
}

// Offline-first: a pinned ref is immutable by convention, so a second parse
// must be served from cache with no network call. This is what lets a QA host
// or CI runner parse the graph offline.
func TestRemoteIncludeServedFromCacheOffline(t *testing.T) {
	t.Setenv("DAG_CACHE_DIR", t.TempDir())
	calls := withFetcher(t, "## Tasks\n\n### compile\n"+block("bash", "echo compile"))

	dir := t.TempDir()
	main := "---\ninclude: gh:qiangli/bashy@v0.19.0/ci.dag.md\n---\n\n## Tasks\n\n" +
		"### build\n" + block("bash", "echo build")
	path := filepath.Join(dir, "DAG.md")
	if err := os.WriteFile(path, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(path); err != nil {
		t.Fatalf("first parse: %v", err)
	}
	// Make any further network use a hard failure, then re-parse.
	remoteFetcher = func(string) ([]byte, error) {
		t.Error("second parse hit the network; cache was not used")
		return nil, os.ErrNotExist
	}
	doc, err := ParseFile(path)
	if err != nil {
		t.Fatalf("second parse (offline): %v", err)
	}
	if _, ok := doc.Lookup("compile"); !ok {
		t.Error("cached include did not merge on the offline parse")
	}
	if *calls != 1 {
		t.Errorf("expected exactly 1 fetch across both parses, got %d", *calls)
	}
}

// A repo must be able to specialize a shared target: the local definition wins.
func TestRemoteIncludeLocalTargetWins(t *testing.T) {
	t.Setenv("DAG_CACHE_DIR", t.TempDir())
	withFetcher(t, "## Tasks\n\n### build\n"+block("bash", "echo SHARED"))

	dir := t.TempDir()
	main := "---\ninclude: gh:qiangli/bashy@v1/ci.dag.md\n---\n\n## Tasks\n\n" +
		"### build\n" + block("bash", "echo LOCAL")
	if err := os.WriteFile(filepath.Join(dir, "DAG.md"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := ParseFile(filepath.Join(dir, "DAG.md"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	tk, ok := doc.Lookup("build")
	if !ok {
		t.Fatal("build target missing")
	}
	if !strings.Contains(tk.Body, "LOCAL") {
		t.Errorf("local target should override the included one; body=%q", tk.Body)
	}
}

// Two files pinning the same remote include must fetch and merge it once.
func TestRemoteIncludeDedupesAcrossFiles(t *testing.T) {
	t.Setenv("DAG_CACHE_DIR", t.TempDir())
	calls := withFetcher(t, "## Tasks\n\n### compile\n"+block("bash", "echo compile"))

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.md"),
		[]byte("---\ninclude: gh:o/r@v1/ci.dag.md\n---\n\n## Tasks\n\n### lib\n"+
			block("bash", "echo lib")), 0o644); err != nil {
		t.Fatal(err)
	}
	main := "---\ninclude:\n  - lib.md\n  - gh:o/r@v1/ci.dag.md\n---\n\n## Tasks\n\n### build\n" +
		block("bash", "echo build")
	if err := os.WriteFile(filepath.Join(dir, "DAG.md"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := ParseFile(filepath.Join(dir, "DAG.md"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	for _, want := range []string{"build", "lib", "compile"} {
		if _, ok := doc.Lookup(want); !ok {
			t.Errorf("target %q missing; order=%v", want, doc.Order)
		}
	}
	if *calls != 1 {
		t.Errorf("diamond include should fetch once, got %d fetches", *calls)
	}
}

// An absolute include path must be used as-is: filepath.Join would splice it
// onto the including file's directory and lose the root.
func TestIncludeAbsolutePath(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared.md")
	if err := os.WriteFile(shared,
		[]byte("## Tasks\n\n### compile\n"+block("bash", "echo compile")), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	main := "---\ninclude: " + shared + "\n---\n\n## Tasks\n\n### build\n" + block("bash", "echo build")
	if err := os.WriteFile(filepath.Join(dir, "DAG.md"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := ParseFile(filepath.Join(dir, "DAG.md"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, ok := doc.Lookup("compile"); !ok {
		t.Errorf("absolute include not merged; order=%v", doc.Order)
	}
}
