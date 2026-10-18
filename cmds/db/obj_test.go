package dbcmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/yoke/mcp"
)

func TestObjCLI_Persistent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "obj_test.db")
	target := []string{"--engine", "sqlite", "--mode", "persistent", "--path", path}

	run := func(want int, stdin string, args ...string) map[string]any {
		t.Helper()
		code, doc := dbRun(t, root, stdin, append(append([]string{}, args...), target...)...)
		if code != want {
			t.Fatalf("db %v: exit %d (want %d): %v", args, code, want, doc)
		}
		return doc
	}

	// 1. Create target
	run(0, "", "create")

	// 2. Bucket create
	run(0, "", "obj", "bucket", "create", "--bucket", "b1")

	// Bucket drop without confirmation fails
	if doc := run(1, "", "obj", "bucket", "drop", "--bucket", "b1"); errCode(doc) != "confirmation_required" {
		t.Fatalf("expected confirmation_required for bucket drop without --yes: %v", doc)
	}

	// 3. Bucket list
	listDoc := run(0, "", "obj", "bucket", "list")
	buckets := listDoc["result"].(map[string]any)["buckets"].([]any)
	if len(buckets) != 1 || buckets[0].(map[string]any)["name"] != "b1" {
		t.Fatalf("unexpected buckets: %v", buckets)
	}

	// 5. Put object
	putDoc := run(0, "", "obj", "put", "--bucket", "b1", "--key", "doc1.txt", "--value", "text:hello world", "--content-type", "text/plain", "--metadata", `{"env":"test"}`)
	res := putDoc["result"].(map[string]any)["object"].(map[string]any)
	if res["revision"] != float64(1) || res["size"] != float64(11) {
		t.Fatalf("unexpected put result: %v", res)
	}
	sha := res["sha256"].(string)
	if sha == "" {
		t.Fatal("expected sha256 on put")
	}

	// 6. Stat object
	statDoc := run(0, "", "obj", "stat", "--bucket", "b1", "--key", "doc1.txt")
	sRes := statDoc["result"].(map[string]any)["object"].(map[string]any)
	if sRes["sha256"] != sha || sRes["content_type"] != "text/plain" {
		t.Fatalf("unexpected stat result: %v", sRes)
	}

	// 7. Get object
	getDoc := run(0, "", "obj", "get", "--bucket", "b1", "--key", "doc1.txt")
	gRes := getDoc["result"].(map[string]any)
	val := gRes["value"].(map[string]any)
	if val["text"] != "hello world" {
		t.Fatalf("unexpected get value: %v", val)
	}

	// 8. CAS / revision preconditions
	if doc := run(1, "", "obj", "put", "--bucket", "b1", "--key", "doc1.txt", "--value", "text:conflict", "--if-revision", "99"); errCode(doc) != "conflict" {
		t.Fatalf("expected conflict on stale revision: %v", doc)
	}
	putDoc2 := run(0, "", "obj", "put", "--bucket", "b1", "--key", "doc1.txt", "--value", "text:updated", "--if-revision", "1")
	if putDoc2["result"].(map[string]any)["object"].(map[string]any)["revision"] != float64(2) {
		t.Fatalf("expected revision 2 after update: %v", putDoc2)
	}

	// 9. If-absent precondition
	run(0, "", "obj", "put", "--bucket", "b1", "--key", "doc2.bin", "--value", "b64:AQID", "--if-absent")
	if doc := run(1, "", "obj", "put", "--bucket", "b1", "--key", "doc2.bin", "--value", "b64:AQID", "--if-absent"); errCode(doc) != "conflict" {
		t.Fatalf("expected conflict on if-absent: %v", doc)
	}

	// 10. List objects with pagination
	listObjDoc := run(0, "", "obj", "list", "--bucket", "b1", "--limit", "1")
	lRes := listObjDoc["result"].(map[string]any)
	objs := lRes["objects"].([]any)
	if len(objs) != 1 {
		t.Fatalf("expected 1 object with limit 1: %v", objs)
	}
	if listObjDoc["page"] == nil {
		t.Fatal("expected page cursor envelope for truncated list")
	}

	// 11. Foreign flag refusal
	if doc := run(2, "", "obj", "get", "--bucket", "b1", "--key", "doc1.txt", "--sql", "select 1"); errCode(doc) != "invalid_input" {
		t.Fatalf("expected invalid_input for foreign flag: %v", doc)
	}

	// 12. Duplicated flag refusal
	if doc := run(2, `{"bucket":"b1"}`, "obj", "get", "--input", "-", "--bucket", "b1", "--key", "doc1.txt"); errCode(doc) != "invalid_input" {
		t.Fatalf("expected invalid_input for duplicated input flag: %v", doc)
	}

	// 13. Logical export and import
	dump := filepath.Join(root, "export-dump")
	run(0, "", "obj", "export", "--bucket", "b1", "--out", dump)

	path2 := filepath.Join(root, "obj_test2.db")
	target2 := []string{"--engine", "sqlite", "--mode", "persistent", "--path", path2}
	run2 := func(want int, stdin string, args ...string) map[string]any {
		t.Helper()
		code, doc := dbRun(t, root, stdin, append(append([]string{}, args...), target2...)...)
		if code != want {
			t.Fatalf("db target2 %v: exit %d (want %d): %v", args, code, want, doc)
		}
		return doc
	}
	run2(0, "", "create")
	run2(0, "", "obj", "import", "--in", dump, "--replace", "--yes")

	getImported := run2(0, "", "obj", "get", "--bucket", "b1", "--key", "doc1.txt")
	if getImported["result"].(map[string]any)["value"].(map[string]any)["text"] != "updated" {
		t.Fatalf("imported object mismatch: %v", getImported)
	}

	// 14. Delete object and drop bucket
	run(0, "", "obj", "rm", "--bucket", "b1", "--key", "doc1.txt")
	run(0, "", "obj", "rm", "--bucket", "b1", "--key", "doc2.bin")
	run(0, "", "obj", "bucket", "drop", "--bucket", "b1", "--yes")

	// Bucket list is empty
	bListAfter := run(0, "", "obj", "bucket", "list")
	if len(bListAfter["result"].(map[string]any)["buckets"].([]any)) != 0 {
		t.Fatalf("expected empty bucket list: %v", bListAfter)
	}
}

func TestObjCLI_Memory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(root, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))

	code, doc := dbRun(t, root, "", "session", "start", "--idle-ttl", "2m")
	if code != 0 {
		t.Fatalf("session start: %v", doc)
	}
	session := doc["result"].(map[string]any)["id"].(string)
	defer dbRun(t, root, "", "session", "stop", "--session", session, "--yes")

	target := []string{"--engine", "sqlite", "--mode", "memory", "--session", session, "--name", "memobj"}

	run := func(want int, stdin string, args ...string) map[string]any {
		t.Helper()
		code, doc := dbRun(t, root, stdin, append(append([]string{}, args...), target...)...)
		if code != want {
			t.Fatalf("db mem %v: exit %d (want %d): %v", args, code, want, doc)
		}
		return doc
	}

	run(0, "", "create")
	run(0, "", "obj", "bucket", "create", "--bucket", "mbkt", "--yes")
	run(0, "", "obj", "put", "--bucket", "mbkt", "--key", "k1", "--value", "text:memory object")

	gDoc := run(0, "", "obj", "get", "--bucket", "mbkt", "--key", "k1")
	val := gDoc["result"].(map[string]any)["value"].(map[string]any)
	if val["text"] != "memory object" {
		t.Fatalf("memory get value: %v", val)
	}
}

func TestObjMCP(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp_obj.db")
	dbRun(t, root, "", "create", "--engine", "sqlite", "--mode", "persistent", "--path", path)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverT, clientT := mcpsdk.NewInMemoryTransports()
	s, err := mcp.BuildServer("yoke", "test", mcp.Options{
		Policy: &mcp.Policy{Allow: map[string]bool{"destroy": true}, Audit: func(mcp.Record) {}},
		Registered: func() []mcp.RegisteredCommand {
			return []mcp.RegisteredCommand{MCPCommand()}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := s.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "db-obj-test", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	viaMCP := func(args map[string]any) map[string]any {
		t.Helper()
		args["engine"], args["mode"], args["path"] = "sqlite", "persistent", path
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
			t.Fatalf("MCP %v exit %d: %s %s", args, out.ExitCode, out.Stdout, out.Stderr)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out.Stdout), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}

	// Bucket create via MCP
	doc := viaMCP(map[string]any{
		"verb": "obj", "subject": "bucket-create", "bucket": "mcp-bkt", "yes": true,
	})
	if doc["op"] != "obj.bucket.create" {
		t.Fatalf("unexpected MCP op: %v", doc)
	}

	// Put via MCP
	docPut := viaMCP(map[string]any{
		"verb": "obj", "subject": "put", "bucket": "mcp-bkt", "key": "msg.txt", "value": "text:mcp payload",
	})
	if docPut["op"] != "obj.put" {
		t.Fatalf("unexpected MCP put op: %v", docPut)
	}

	// Get via MCP
	docGet := viaMCP(map[string]any{
		"verb": "obj", "subject": "get", "bucket": "mcp-bkt", "key": "msg.txt",
	})
	val := docGet["result"].(map[string]any)["value"].(map[string]any)
	if val["text"] != "mcp payload" {
		t.Fatalf("unexpected MCP get value: %v", val)
	}
}
