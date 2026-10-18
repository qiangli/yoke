package dbcmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/yoke/mcp"
)

func TestFTSRegisteredCLIAndTypedMCP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	target := []string{"--engine", "bleve", "--mode", "persistent", "--path", filepath.Join(root, "idx")}
	if code, doc := dbRun(t, root, "", append([]string{"create"}, target...)...); code != 0 {
		t.Fatalf("create: %v", doc)
	}
	put := `{"id":"a","doc":{"title":"hello","n":9007199254740993}}`
	if code, doc := dbRun(t, root, put, append([]string{"fts", "put", "--input", "-"}, target...)...); code != 0 {
		t.Fatalf("put: %v", doc)
	}
	_, cli := dbRun(t, root, `{"query":"hello"}`, append([]string{"fts", "search", "--input", "-"}, target...)...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	srv, err := mcp.BuildServer("yoke", "test", mcp.Options{Policy: &mcp.Policy{Allow: map[string]bool{"destroy": true}, Audit: func(mcp.Record) {}}, Registered: func() []mcp.RegisteredCommand { return []mcp.RegisteredCommand{MCPCommand()} }})
	if err != nil {
		t.Fatal(err)
	}
	server, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "db-fts-test", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	args := map[string]any{"verb": "fts", "subject": "search", "engine": "bleve", "mode": "persistent", "path": filepath.Join(root, "idx"), "input": "-", "stdin": `{"query":"hello"}`}
	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "db_lifecycle", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out mcp.RunToolOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("MCP exit %d: %s %s", out.ExitCode, out.Stdout, out.Stderr)
	}
	var viaMCP map[string]any
	if err := json.Unmarshal([]byte(out.Stdout), &viaMCP); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(cli)
	b, _ = json.Marshal(viaMCP)
	if string(a) != string(b) {
		t.Fatalf("CLI/MCP mismatch: %s != %s", a, b)
	}
	if _, err := os.Stat(filepath.Join(root, "idx")); err != nil {
		t.Fatal(err)
	}
}
