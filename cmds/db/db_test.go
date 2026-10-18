package dbcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"
	"github.com/qiangli/yoke/pkg/ref"
)

func TestRegisteredLifecycleCommand(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "one.db")
	registered := tool.Lookup("db")
	if registered == nil {
		t.Fatal("db tool not registered")
	}
	run := func(args ...string) (int, map[string]any) {
		t.Helper()
		var out, stderr bytes.Buffer
		rc := &tool.RunContext{Ctx: context.Background(), Dir: root, Env: os.Environ()}
		rc.Out = &out
		rc.Err = &stderr
		code := registered.Run(rc, args)
		t.Logf("db %v => exit=%d stdout=%s stderr=%s", args, code, out.String(), stderr.String())
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("%v: output %q stderr %q: %v", args, out.String(), stderr.String(), err)
		}
		return code, doc
	}
	flags := []string{"--engine", "sqlite", "--mode", "persistent", "--path", path}
	if code, doc := run(append([]string{"show"}, flags...)...); code != 1 || doc["schema_version"] != "bashy-db-error-v1" {
		t.Fatalf("missing show: code=%d doc=%v", code, doc)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("show created path: %v", err)
	}
	if code, doc := run("create", "--engine", "sqlite", "--mode", "persistent", "--path", path, "--explain"); code != 0 || doc["preview_level"] != "explain-only" {
		t.Fatalf("explain: code=%d doc=%v", code, doc)
	}
	if code, doc := run("create", "--engine", "sqlite", "--mode", "persistent", "--path", path, "--dry-run"); code != 0 || doc["preview_level"] != "local-preview" {
		t.Fatalf("dry-run: code=%d doc=%v", code, doc)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("preview created path: %v", err)
	}
	if code, doc := run(append([]string{"create"}, flags...)...); code != 0 || doc["schema_version"] != "bashy-db-result-v1" {
		t.Fatalf("create: code=%d doc=%v", code, doc)
	}
	if code, doc := run("session", "start", "--dry-run"); code != 0 || doc["preview_level"] != "local-preview" {
		t.Fatalf("session dry-run: code=%d doc=%v", code, doc)
	}
	if code, doc := run(append([]string{"check"}, flags...)...); code != 0 || doc["ok"] != true {
		t.Fatalf("check: code=%d doc=%v", code, doc)
	}
	locator := (ref.Locator{Scheme: "db", Target: "sqlite:file:" + path}).String()
	if code, doc := run("show", locator); code != 0 || doc["ok"] != true {
		t.Fatalf("locator show: code=%d doc=%v", code, doc)
	}
	backup := filepath.Join(root, "one.backup")
	if code, doc := run(append(append([]string{"backup"}, flags...), "--out", backup)...); code != 0 || doc["ok"] != true {
		t.Fatalf("backup: code=%d doc=%v", code, doc)
	}
	other := filepath.Join(root, "other.db")
	otherFlags := []string{"--engine", "sqlite", "--mode", "persistent", "--path", other}
	if code, _ := run(append(append([]string{"restore"}, otherFlags...), "--in", backup, "--replace", "--yes")...); code != 1 {
		t.Fatalf("missing restore target should fail: %d", code)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("restore created missing target: %v", err)
	}
	if code, _ := run(append([]string{"create"}, otherFlags...)...); code != 0 {
		t.Fatalf("create restore target: %d", code)
	}
	if code, doc := run(append(append([]string{"restore"}, otherFlags...), "--in", backup, "--replace", "--yes")...); code != 0 || doc["ok"] != true {
		t.Fatalf("restore: code=%d doc=%v", code, doc)
	}
	if code, _ := run(append([]string{"drop"}, flags...)...); code != 2 {
		t.Fatalf("unconfirmed drop code=%d", code)
	}
	if code, doc := run(append(append([]string{"drop"}, flags...), "--yes")...); code != 0 || doc["ok"] != true {
		t.Fatalf("drop: code=%d doc=%v", code, doc)
	}
}

func TestMCPRunToolLifecycleParity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	server, err := mcp.NewServer("yoke", "test").Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "db-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	path := filepath.Join(t.TempDir(), "mcp.db")
	for _, tt := range []struct {
		op   string
		exit int
		code string
	}{
		{"show", 1, "not_found"},
		{"create", 0, ""},
		{"check", 0, ""},
	} {
		res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: mcp.RunToolInput{Name: "db", Args: []string{tt.op, "--engine", "sqlite", "--mode", "persistent", "--path", path}}})
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out mcp.RunToolOutput
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out.Stdout), &doc); err != nil {
			t.Fatal(err)
		}
		if out.ExitCode != tt.exit {
			t.Fatalf("%s exit=%d doc=%v", tt.op, out.ExitCode, doc)
		}
		if tt.code != "" && doc["error"].(map[string]any)["code"] != tt.code {
			t.Fatalf("%s doc=%v", tt.op, doc)
		}
	}
}

func TestTypedMCPDbLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	srv, err := mcp.BuildServer("yoke", "test", mcp.Options{Registered: func() []mcp.RegisteredCommand { return []mcp.RegisteredCommand{MCPCommand()} }})
	if err != nil {
		t.Fatal(err)
	}
	server, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "db-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range listed.Tools {
		if item.Name == "db_lifecycle" {
			found = true
			schema, ok := item.InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("unexpected MCP schema %T", item.InputSchema)
			}
			if _, ok := schema["properties"].(map[string]any)["verb"]; !ok {
				t.Fatal("typed verb absent from MCP schema")
			}
		}
	}
	if !found {
		t.Fatal("typed db_lifecycle tool absent")
	}
	path := filepath.Join(t.TempDir(), "typed.db")
	locator := (ref.Locator{Scheme: "db", Target: "sqlite:file:" + path}).String()
	for _, tt := range []struct {
		verb string
		exit int
	}{{"show", 1}, {"create", 0}, {"check", 0}} {
		res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "db_lifecycle", Arguments: map[string]any{"verb": tt.verb, "subject": locator}})
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out mcp.RunToolOutput
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if out.ExitCode != tt.exit {
			t.Fatalf("%s exit=%d out=%s stderr=%s", tt.verb, out.ExitCode, out.Stdout, out.Stderr)
		}
	}
}
