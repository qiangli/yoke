package search

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestParseTavily(t *testing.T) {
	raw := []byte(`{"results":[{"title":"RFT survey","url":"https://x.test/rft","content":"rejection sampling fine-tuning..."}]}`)
	got, err := parseTavily(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != "https://x.test/rft" || got[0].Snippet == "" || got[0].Backend != "tavily" {
		t.Fatalf("bad tavily parse: %+v", got)
	}
}

func TestParseBrave(t *testing.T) {
	raw := []byte(`{"web":{"results":[{"title":"A2A","url":"https://x.test/a2a","description":"agent to agent"}]}}`)
	got, err := parseBrave(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != "https://x.test/a2a" || got[0].Snippet != "agent to agent" || got[0].Backend != "brave" {
		t.Fatalf("bad brave parse: %+v", got)
	}
}

func TestParseSerper(t *testing.T) {
	raw := []byte(`{"organic":[{"title":"MCP","link":"https://x.test/mcp","snippet":"model context protocol"}]}`)
	got, err := parseSerper(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != "https://x.test/mcp" || got[0].Backend != "serper" {
		t.Fatalf("bad serper parse: %+v", got)
	}
}

func clearKeys(t *testing.T) {
	for _, k := range []string{"TAVILY_API_KEY", "BRAVE_API_KEY", "BRAVE_SEARCH_API_KEY", "SERPER_API_KEY", "BASHY_SEARCH_BACKEND"} {
		t.Setenv(k, "")
	}
}

func TestWebNoBackendConfigured(t *testing.T) {
	clearKeys(t)
	_, _, err := Web(context.Background(), "anything", Options{NoVault: true})
	if !errors.Is(err, ErrNoBackend) {
		t.Fatalf("want ErrNoBackend, got %v", err)
	}
}

func TestWebForcedUnknownBackend(t *testing.T) {
	clearKeys(t)
	_, _, err := Web(context.Background(), "q", Options{Backend: "bing", NoVault: true})
	if err == nil {
		t.Fatal("forcing an unknown backend must error")
	}
}

func TestWebForcedBackendMissingKey(t *testing.T) {
	clearKeys(t)
	_, _, err := Web(context.Background(), "q", Options{Backend: "tavily", NoVault: true})
	if err == nil {
		t.Fatal("forcing a backend whose key is unset must error clearly")
	}
}

func TestWebEmptyQuery(t *testing.T) {
	if _, _, err := Web(context.Background(), "   ", Options{}); err == nil {
		t.Fatal("empty query must error")
	}
}

func TestLocalContentScan(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/a.go", []byte("package a\n// TODO fix the widget\nfunc F(){}\n"), 0o644)
	os.WriteFile(dir+"/b.txt", []byte("nothing here\n"), 0o644)
	res, err := Local("todo", LocalOptions{Dir: dir, Domain: "content"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Kind != "content" || res[0].Line != 2 {
		t.Fatalf("content scan: want 1 hit at a.go:2, got %+v", res)
	}
}

func TestLocalFilesScan(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/widget_test.go", []byte("x"), 0o644)
	os.WriteFile(dir+"/other.go", []byte("y"), 0o644)
	res, err := Local("widget", LocalOptions{Dir: dir, Domain: "files"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Kind != "file" {
		t.Fatalf("files scan: want 1 file hit, got %+v", res)
	}
}

func TestLocalSkipsBinaryAndNoise(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/.git", 0o755)
	os.WriteFile(dir+"/.git/todo.txt", []byte("todo in git"), 0o644)  // must be skipped
	os.WriteFile(dir+"/bin.dat", []byte("todo\x00\x01binary"), 0o644) // binary, skipped
	res, _ := Local("todo", LocalOptions{Dir: dir, Domain: "content"})
	if len(res) != 0 {
		t.Fatalf(".git + binary must be skipped, got %+v", res)
	}
}
