package search

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedIndexCLIAndFallback(t *testing.T) {
	t.Setenv("BASHY_HOME", filepath.Join(t.TempDir(), "home"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(t.TempDir(), "coord"))
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "note.md"), []byte("cliindexmarker"), 0600); e != nil {
		t.Fatal(e)
	}
	run := func(args ...string) map[string]any {
		t.Helper()
		c := NewSearchCmd()
		var b bytes.Buffer
		c.SetOut(&b)
		c.SetErr(&b)
		c.SetArgs(args)
		if e := c.Execute(); e != nil {
			t.Fatalf("%v: %v %s", args, e, b.String())
		}
		var out map[string]any
		if e := json.Unmarshal(b.Bytes(), &out); e != nil {
			t.Fatalf("%s: %v", b.String(), e)
		}
		return out
	}
	run("index", "init", "--json")
	run("index", "add", root, "--json")
	out := run("--indexed", "cliindexmarker", "--json")
	r := out["result"].(map[string]any)
	if r["mode"] != "lexical" || r["fallback"] != true || len(r["hits"].([]any)) != 1 {
		t.Fatal(out)
	}
	run("index", "config", "--promotion-days", "2", "--json")
	out = run("index", "status", "--json")
	if out["limits"].(map[string]any)["promotion_days"] != float64(2) {
		t.Fatal(out)
	}
	run("index", "pause", "--json")
	run("index", "forget", root, "--json")
	out = run("--indexed", "cliindexmarker", "--json")
	if out["result"].(map[string]any)["hits"] != nil && len(out["result"].(map[string]any)["hits"].([]any)) != 0 {
		t.Fatal("forgotten source served", out)
	}
}
