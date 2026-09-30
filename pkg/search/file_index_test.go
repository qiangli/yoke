package search

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileIndexRefreshAndFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	root := t.TempDir()
	write := func(name string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("content is not indexed"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/Widget.go")
	write("src/widget_test.go")
	write("other.txt")
	if hits, err := Local("widget", LocalOptions{Dir: root, Domain: "files"}); err != nil || len(hits) != 2 {
		t.Fatalf("scan fallback = %+v, %v", hits, err)
	}
	info, err := BuildFileIndex(root)
	if err != nil || info.Files != 3 || info.Bytes == 0 || info.UpdatedAt.IsZero() {
		t.Fatalf("index info = %+v, %v", info, err)
	}
	hits, err := Local("widget", LocalOptions{Dir: root, Domain: "files", MaxResults: 1})
	if err != nil || len(hits) != 1 || hits[0].Path != filepath.Join("src", "Widget.go") {
		t.Fatalf("bounded index query = %+v, %v", hits, err)
	}
	for _, query := range []string{"id", "Widget.*"} {
		if hits, err := Local(query, LocalOptions{Dir: root, Domain: "files"}); err != nil || len(hits) == 0 {
			t.Fatalf("index query %q = %+v, %v", query, hits, err)
		}
	}
	if err := os.Remove(filepath.Join(root, "src", "Widget.go")); err != nil {
		t.Fatal(err)
	}
	write("new-widget.md")
	if hits, err := Local("Widget.go", LocalOptions{Dir: root, Domain: "files"}); err != nil || len(hits) != 1 {
		t.Fatalf("index should retain snapshot until refresh: %+v, %v", hits, err)
	}
	info, err = BuildFileIndex(root)
	if err != nil || info.Files != 3 {
		t.Fatalf("refresh = %+v, %v", info, err)
	}
	if hits, err := Local("Widget.go", LocalOptions{Dir: root, Domain: "files"}); err != nil || len(hits) != 0 {
		t.Fatalf("removed file survived refresh: %+v, %v", hits, err)
	}
	if hits, err := Local("new-widget", LocalOptions{Dir: root, Domain: "files"}); err != nil || len(hits) != 1 {
		t.Fatalf("new file missing after refresh: %+v, %v", hits, err)
	}
}

func TestFileIndexRootIsolationAndCLI(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	rootA, rootB := t.TempDir(), t.TempDir()
	for _, root := range []string{rootA, rootB} {
		if err := os.WriteFile(filepath.Join(root, "needle.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := NewSearchCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--files", "--dir", rootA, "--index"})
	if err := cmd.Execute(); err != nil || !strings.Contains(output.String(), "1 files") {
		t.Fatalf("index command = %q, %v", output.String(), err)
	}
	if _, err := FileIndexInfo(rootB); !os.IsNotExist(err) {
		t.Fatalf("other root unexpectedly indexed: %v", err)
	}
	cmd = NewSearchCmd()
	output.Reset()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--files", "--dir", rootA, "--index-status", "--json"})
	if err := cmd.Execute(); err != nil || !strings.Contains(output.String(), `"files":1`) {
		t.Fatalf("status command = %q, %v", output.String(), err)
	}
	cmd = NewSearchCmd()
	output.Reset()
	cmd.SetOut(&output)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--files", "--dir", rootA, "--max", "1", "needle"})
	if err := cmd.Execute(); err != nil || strings.TrimSpace(output.String()) != "needle.txt" {
		t.Fatalf("bounded query command = %q, %v", output.String(), err)
	}
	cmd = NewSearchCmd()
	cmd.SetArgs([]string{"--index"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("index without --files must fail")
	}
}
