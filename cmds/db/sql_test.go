package dbcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"
)

// TestMain lets this test binary act as the db CLI (`<binary> db ...`):
// `db session start` re-execs it as the owner and `db hold -- CMD` children
// run it as independent client processes.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "db" {
		dir, _ := os.Getwd()
		rc := &tool.RunContext{Ctx: context.Background(), Dir: dir, Env: os.Environ(), In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
		os.Exit(tool.Lookup("db").Run(rc, os.Args[2:]))
	}
	os.Exit(m.Run())
}

func dbRun(t *testing.T, dir, stdin string, args ...string) (int, map[string]any) {
	t.Helper()
	var out, stderr bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: dir, Env: os.Environ(), In: strings.NewReader(stdin), Out: &out, Err: &stderr}
	code := tool.Lookup("db").Run(rc, args)
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("db %v: output %q stderr %q: %v", args, out.String(), stderr.String(), err)
	}
	return code, doc
}

func rowsOf(doc map[string]any) string {
	b, _ := json.Marshal(doc["result"].(map[string]any)["rows"])
	return string(b)
}

func TestSQLVerbsBothModes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(root, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	code, doc := dbRun(t, root, "", "session", "start", "--idle-ttl", "2m")
	if code != 0 {
		t.Fatalf("session start: %v", doc)
	}
	session := doc["result"].(map[string]any)["id"].(string)
	defer dbRun(t, root, "", "session", "stop", "--session", session, "--yes")
	for _, target := range [][]string{
		{"--engine", "sqlite", "--mode", "persistent", "--path", "cli.db"},
		{"--engine", "sqlite", "--mode", "memory", "--session", session, "--name", "cli"},
	} {
		t.Run(target[3], func(t *testing.T) {
			run := func(args ...string) (int, map[string]any) { return dbRun(t, root, "", append(args, target...)...) }
			if code, doc := run("sql", "query", "--sql", "select 1"); code != 1 || doc["error"].(map[string]any)["code"] != "not_found" {
				t.Fatalf("query before create: %d %v", code, doc)
			}
			if code, doc := run("create"); code != 0 {
				t.Fatalf("create: %v", doc)
			}
			if code, doc := run("sql", "schema", "apply", "--sql", "create table t(id integer primary key, v)", "--explain"); code != 0 || doc["result"].(map[string]any)["declaration"].(map[string]any)["hold"] != "exclusive" {
				t.Fatalf("explain: %v", doc)
			}
			if code, doc := run("sql", "schema", "apply", "--sql", "create table t(id integer primary key, v)"); code != 1 || doc["error"].(map[string]any)["code"] != "confirmation_required" {
				t.Fatalf("unconfirmed: %d %v", code, doc)
			}
			if code, doc := run("sql", "schema-apply", "--yes", "--sql", "create table t(id integer primary key, v)"); code != 0 || doc["mutation_class"] != "destructive" {
				t.Fatalf("apply: %v", doc)
			}
			if code, doc := run("sql", "exec", "--sql", "insert into t(v) values (?), (?)", "--param", "int:9007199254740993", "--param", "bytes:AP8="); code != 0 || doc["mutation_class"] != "write" {
				t.Fatalf("exec: %v", doc)
			}
			if code, doc := run("sql", "exec", "--sql", "insert into t(v) values (?)", "--param", "int:1.5", "--dry-run"); code != 2 || doc["error"].(map[string]any)["code"] != "invalid_input" {
				t.Fatalf("dry-run bad param: %d %v", code, doc)
			}
			if code, doc := run("sql", "exec", "--sql", "insert into t(v) values (7)", "--dry-run"); code != 0 || doc["preview_level"] != "local-preview" {
				t.Fatalf("dry-run: %d %v", code, doc)
			}
			if code, doc := run("sql", "query", "--sql", "insert into t(v) values (1)"); code != 1 || doc["error"].(map[string]any)["code"] != "read_only" {
				t.Fatalf("read-only query: %d %v", code, doc)
			}
			code, doc := dbRun(t, root, `{"sql":"select v from t where id > ? order by id","params":[{"t":"int","v":"0"}],"limit":1}`, append([]string{"sql", "query", "--input", "-"}, target...)...)
			if code != 0 || rowsOf(doc) != `[[{"t":"int","v":"9007199254740993"}]]` {
				t.Fatalf("typed --input query: %v", doc)
			}
			cursor, _ := json.Marshal(doc["page"].(map[string]any)["cursor"])
			code, doc = dbRun(t, root, `{"sql":"select v from t where id > ? order by id","params":[{"t":"int","v":"0"}],"limit":1}`, append([]string{"sql", "query", "--input", "-", "--since", string(cursor)}, target...)...)
			if code != 0 || rowsOf(doc) != `[[{"b64":"AP8=","t":"bytes"}]]` {
				t.Fatalf("resumed page: %v", doc)
			}
			if code, doc := run("sql", "schema", "show"); code != 0 || !strings.Contains(rowsOrObjects(doc), `"name":"t"`) {
				t.Fatalf("schema show: %v", doc)
			}
			out := filepath.Join(root, "export-"+target[3])
			if code, doc := run("export", "--out", out); code != 0 || doc["op"] != "sql.export" {
				t.Fatalf("export: %v", doc)
			}
			if code, doc := run("import", "--in", out); code != 0 || doc["result"].(map[string]any)["on_conflict"] != "error" {
				// Merge of identical rows conflicts by default.
				if doc["error"].(map[string]any)["code"] != "conflict" {
					t.Fatalf("merge import: %d %v", code, doc)
				}
			}
			if code, doc := run("import", "--in", out, "--on-conflict", "skip"); code != 0 {
				t.Fatalf("skip import: %v", doc)
			}
			if code, doc := run("import", "--in", out, "--replace", "--yes"); code != 0 || doc["mutation_class"] != "destructive" {
				t.Fatalf("replace import: %v", doc)
			}
			if code, doc := run("sql", "query", "--sql", "select count(*) from t"); code != 0 || rowsOf(doc) != `[[{"t":"int","v":"2"}]]` {
				t.Fatalf("count after import: %v", doc)
			}
		})
	}
	// Direct persistent path cannot promise request-id dedup.
	if code, doc := dbRun(t, root, "", "sql", "exec", "--engine", "sqlite", "--mode", "persistent", "--path", "cli.db", "--sql", "insert into t(v) values (1)", "--request-id", "r"); code != 2 || doc["error"].(map[string]any)["code"] != "unsupported" {
		t.Fatalf("direct request-id: %d %v", code, doc)
	}
}

func rowsOrObjects(doc map[string]any) string {
	b, _ := json.Marshal(doc["result"])
	return string(b)
}

func TestTypedMCPSQLQuery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	path := filepath.Join(root, "mcp.db")
	target := []string{"--engine", "sqlite", "--mode", "persistent", "--path", path}
	dbRun(t, root, "", append([]string{"create"}, target...)...)
	dbRun(t, root, "", append([]string{"sql", "schema", "apply", "--yes", "--sql", "create table m(v)"}, target...)...)
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
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "db-sql-test", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(args map[string]any) (int, map[string]any) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "db_lifecycle", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(res.StructuredContent)
		var out mcp.RunToolOutput
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out.Stdout), &doc); err != nil {
			t.Fatalf("stdout %q stderr %q", out.Stdout, out.Stderr)
		}
		return out.ExitCode, doc
	}
	base := map[string]any{"verb": "sql", "engine": "sqlite", "mode": "persistent", "path": path}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	if code, doc := call(with("subject", "exec", "sql", "insert into m values (?)", "params", `[{"t":"int","v":"-9223372036854775808"}]`)); code != 0 {
		t.Fatalf("mcp exec: %v", doc)
	}
	if code, doc := call(with("subject", "query", "sql", "select v from m")); code != 0 || rowsOf(doc) != `[[{"t":"int","v":"-9223372036854775808"}]]` {
		t.Fatalf("mcp query: %v", doc)
	}
	if code, doc := call(with("subject", "query", "sql", "pragma query_only=0; delete from m")); code != 1 || doc["error"].(map[string]any)["code"] != "read_only" {
		t.Fatalf("mcp read-only: %d %v", code, doc)
	}
	if code, doc := call(with("subject", "schema-show")); code != 0 || doc["op"] != "sql.schema.show" {
		t.Fatalf("mcp schema show: %v", doc)
	}
	// CLI and MCP return the same envelope for the same request.
	_, cli := dbRun(t, root, "", append([]string{"sql", "query", "--sql", "select v from m"}, target...)...)
	_, viaMCP := call(with("subject", "query", "sql", "select v from m"))
	a, _ := json.Marshal(cli)
	b, _ := json.Marshal(viaMCP)
	if string(a) != string(b) {
		t.Fatalf("CLI/MCP envelopes differ\ncli %s\nmcp %s", a, b)
	}
}

func TestMemoryHoldChildPresentsEpoch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(root, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	_, doc := dbRun(t, root, "", "session", "start", "--idle-ttl", "2m")
	session := doc["result"].(map[string]any)["id"].(string)
	defer dbRun(t, root, "", "session", "stop", "--session", session, "--yes")
	target := []string{"--engine", "sqlite", "--mode", "memory", "--session", session, "--name", "h"}
	dbRun(t, root, "", append([]string{"create"}, target...)...)
	dbRun(t, root, "", append([]string{"sql", "schema", "apply", "--yes", "--sql", "create table t(v)"}, target...)...)
	child := append(append([]string{"hold"}, target...), "--", os.Args[0], "db", "sql", "exec", "--sql", "insert into t values (1)")
	child = append(child, target...)
	var out, stderr bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: root, Env: os.Environ(), Out: &out, Err: &stderr}
	if code := tool.Lookup("db").Run(rc, child); code != 0 {
		t.Fatalf("held child exit %d: %s %s", code, out.String(), stderr.String())
	}
	if _, doc := dbRun(t, root, "", append([]string{"sql", "query", "--sql", "select count(*) from t"}, target...)...); rowsOf(doc) != `[[{"t":"int","v":"1"}]]` {
		t.Fatalf("held write: %v", doc)
	}
}
