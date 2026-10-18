package dbcmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestGraphRegisteredCLIAndMCP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	target := []string{"--engine", "bonsai", "--mode", "persistent", "--path", filepath.Join(root, "graph")}
	run := func(want int, input string, args ...string) map[string]any {
		t.Helper()
		code, doc := dbRun(t, root, input, append(args, target...)...)
		if code != want {
			t.Fatalf("db %v: exit %d want %d: %v", args, code, want, doc)
		}
		return doc
	}
	run(0, "", "create")
	node := `{"id":"alone","type":"person","props":"AAE="}`
	run(0, "", "graph", "node", "put", "--node", node, "--if-absent")
	got := run(0, "", "graph", "node-get", "--id", "alone")
	if got["result"].(map[string]any)["id"] != "alone" {
		t.Fatalf("node get: %v", got)
	}
	if doc := run(2, "", "graph", "node-get", "--id", "alone", "--bucket", "x"); errCode(doc) != "invalid_input" {
		t.Fatalf("foreign flag: %v", doc)
	}
	if doc := run(2, `{"id":"alone"}`, "graph", "node-get", "--input", "-", "--id", "alone"); errCode(doc) != "invalid_input" {
		t.Fatalf("duplicate input: %v", doc)
	}
	export := filepath.Join(root, "export")
	run(0, "", "export", "--out", export)
	other := []string{"--engine", "bonsai", "--mode", "persistent", "--path", filepath.Join(root, "other")}
	if code, doc := dbRun(t, root, "", append([]string{"create"}, other...)...); code != 0 {
		t.Fatalf("create other: %v", doc)
	}
	if code, doc := dbRun(t, root, "", append([]string{"graph", "import", "--in", export, "--replace", "--yes"}, other...)...); code != 0 {
		t.Fatalf("import other: %v", doc)
	}
	if code, doc := dbRun(t, root, "", append([]string{"graph", "node-get", "--id", "alone"}, other...)...); code != 0 || doc["result"].(map[string]any)["id"] != "alone" {
		t.Fatalf("imported isolated node: %v", doc)
	}
	// The typed direct MCP registration uses the same declared flags and runner.
	schema := Schema()
	found := false
	for _, f := range schema.Flags {
		if f.Name == "node" {
			found = true
		}
	}
	if !found {
		t.Fatal("MCP schema lacks graph node input")
	}
	argv := []string{"--engine", "bonsai", "--mode", "persistent", "--path", other[5], "--id", "alone", "graph", "node-get"}
	stdout, stderr, code := MCPCommand().Run(context.Background(), argv, "", root)
	if code != 0 {
		t.Fatalf("MCP graph get: exit %d %s %s", code, stdout, stderr)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err != nil || m["op"] != "graph.node.get" {
		t.Fatalf("MCP graph output: %s %v", stdout, err)
	}
}
