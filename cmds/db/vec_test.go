package dbcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"
	"github.com/qiangli/yoke/pkg/policy/advice"
)

func vecHits(t *testing.T, doc map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(doc["result"].(map[string]any)["hits"])
	return string(b)
}

// vecVerbsCLI drives the registered `db vec` path on one target: explicit
// collection, supplied vectors (zero network), revision guard, metadata
// filter, refusal of foreign or duplicated flags, export and import.
func vecVerbsCLI(t *testing.T, root string, target, other []string) {
	run := func(want int, target []string, stdin string, args ...string) map[string]any {
		t.Helper()
		code, doc := dbRun(t, root, stdin, append(append([]string{}, args...), target...)...)
		if code != want {
			t.Fatalf("db %v: exit %d (want %d): %v", args, code, want, doc)
		}
		return doc
	}
	run(0, target, "", "create")
	if doc := run(1, target, "", "vec", "put", "--collection", "c", "--id", "a", "--vector", "[1,0,0]"); errCode(doc) != "not_found" {
		t.Fatalf("implicit collection: %v", doc)
	}
	if doc := run(1, target, "", "vec", "collection", "add", "--collection", "c", "--dimension", "3", "--metric", "l2", "--model", "m@1"); errCode(doc) != "confirmation_required" {
		t.Fatalf("unconfirmed schema change: %v", doc)
	}
	if doc := run(0, target, "", "vec", "collection-add", "--yes", "--collection", "c", "--dimension", "3", "--metric", "l2", "--model", "m@1"); doc["mutation_class"] != "destructive" {
		t.Fatalf("collection add: %v", doc)
	}
	run(0, target, "", "vec", "put", "--collection", "c", "--id", "a", "--vector", "[1,0,0]", "--metadata", `{"k":"x","n":1}`)
	run(0, target, `{"collection":"c","id":"b","vector":{"t":"vec","dtype":"f32","b64":"AAAAAAAAgD8AAAAA"},"metadata":{"k":"y","n":2}}`, "vec", "put", "--input", "-")
	if doc := run(1, target, "", "vec", "put", "--collection", "c", "--id", "a", "--vector", "[0,0,1]", "--if-revision", "7"); errCode(doc) != "conflict" {
		t.Fatalf("stale revision: %v", doc)
	}
	run(0, target, "", "vec", "put", "--collection", "c", "--id", "a", "--vector", "[1,0,0.5]", "--metadata", `{"k":"x","n":1}`, "--if-revision", "1")
	if doc := run(0, target, "", "vec", "get", "--collection", "c", "--id", "a"); doc["result"].(map[string]any)["revision"] != float64(2) {
		t.Fatalf("get: %v", doc)
	}
	search := run(0, target, "", "vec", "search", "--collection", "c", "--vector", "[0,1,0]", "--filter", `{"field":"k","op":"eq","value":"y"}`)
	if got := vecHits(t, search); got != `[{"distance":0,"id":"b","metadata":{"k":"y","n":2},"revision":1}]` {
		t.Fatalf("filtered search: %s", got)
	}
	// Foreign, duplicated and misplaced flags fail loudly.
	if doc := run(2, target, "", "vec", "get", "--collection", "c", "--id", "a", "--bucket", "b"); errCode(doc) != "invalid_input" {
		t.Fatalf("foreign flag: %v", doc)
	}
	if doc := run(2, target, `{"collection":"c"}`, "vec", "get", "--input", "-", "--collection", "c", "--id", "a"); errCode(doc) != "invalid_input" {
		t.Fatalf("duplicated value: %v", doc)
	}
	if doc := run(2, target, "", "vec", "put", "--collection", "c", "--id", "z", "--vector", "[1,1,1]", "--out", "x"); errCode(doc) != "invalid_input" {
		t.Fatalf("misplaced --out: %v", doc)
	}
	if doc := run(2, target, "", "vec", "search", "--collection", "c", "--text", "hello"); errCode(doc) != "invalid_input" {
		t.Fatalf("text without endpoint: %v", doc)
	}
	dump := filepath.Join(root, "dump-"+target[3])
	run(0, target, "", "vec", "export", "--out", dump)
	run(0, other, "", "create")
	run(0, other, "", "vec", "import", "--in", dump, "--replace", "--yes")
	if got := vecHits(t, run(0, other, "", "vec", "search", "--collection", "c", "--vector", "[0,1,0]", "--filter", `{"field":"k","op":"eq","value":"y"}`)); got != vecHits(t, search) {
		t.Fatalf("imported search %s != %s", got, vecHits(t, search))
	}
	run(0, target, "", "vec", "rm", "--collection", "c", "--id", "a", "--if-revision", "2")
	run(0, target, "", "vec", "collection", "rm", "--collection", "c", "--yes")
}

func TestVecVerbsPersistentCLI(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	vecVerbsCLI(t, root,
		[]string{"--engine", "sqlite", "--mode", "persistent", "--path", "a.db"},
		[]string{"--engine", "sqlite", "--mode", "persistent", "--path", "b.db"})
}

// TestVecVerbsMemorySessionCLI runs the same verbs against memory targets in
// an owner session, which re-execs this binary as an independent process.
func TestVecVerbsMemorySessionCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns an independent owner process")
	}
	root := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(root, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	code, doc := dbRun(t, root, "", "session", "start", "--idle-ttl", "2m")
	if code != 0 {
		t.Fatalf("session start: %v", doc)
	}
	session := doc["result"].(map[string]any)["id"].(string)
	defer dbRun(t, root, "", "session", "stop", "--session", session, "--yes")
	vecVerbsCLI(t, root,
		[]string{"--engine", "sqlite", "--mode", "memory", "--session", session, "--name", "a"},
		[]string{"--engine", "sqlite", "--mode", "memory", "--session", session, "--name", "b"})
}

// TestVecRegisteredCLIAndTypedMCP proves CLI/MCP parity for supplied vectors
// and for text embedded through an explicit endpoint, and that a supplied
// vector never reaches the endpoint.
func TestVecRegisteredCLIAndTypedMCP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	var calls atomic.Int32
	var model atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		model.Store(body["model"])
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"index":0,"embedding":[0,1,0]}]}`)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/v1"
	path := filepath.Join(root, "v.db")
	target := []string{"--engine", "sqlite", "--mode", "persistent", "--path", path}
	for _, args := range [][]string{
		{"create"},
		{"vec", "collection", "add", "--yes", "--collection", "c", "--dimension", "3", "--metric", "cosine", "--model", "emb@2"},
		{"vec", "put", "--collection", "c", "--id", "a", "--vector", "[1,0,0]", "--metadata", `{"k":1}`},
	} {
		if code, doc := dbRun(t, root, "", append(args, target...)...); code != 0 {
			t.Fatalf("%v: %v", args, doc)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("supplied vectors reached the endpoint %d times", calls.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	s, err := mcp.BuildServer("yoke", "test", mcp.Options{Policy: &mcp.Policy{Allow: map[string]bool{"destroy": true}, Audit: func(mcp.Record) {}}, Registered: func() []mcp.RegisteredCommand { return []mcp.RegisteredCommand{MCPCommand()} }})
	if err != nil {
		t.Fatal(err)
	}
	server, err := s.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "db-vec-test", Version: "test"}, nil).Connect(ctx, clientT, nil)
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
	// Text put through MCP embeds with the persisted model at the endpoint.
	viaMCP(map[string]any{"verb": "vec", "subject": "put", "collection": "c", "id": "t", "text": "hello", "endpoint": endpoint, "metadata": `{"k":2}`})
	if calls.Load() != 1 || model.Load() != "emb@2" {
		t.Fatalf("embedding calls %d model %v", calls.Load(), model.Load())
	}
	for _, q := range []struct{ cli []string }{
		{[]string{"--vector", "[0,1,0]"}},
		{[]string{"--text", "hello", "--endpoint", endpoint}},
	} {
		_, cli := dbRun(t, root, "", append(append([]string{"vec", "search", "--collection", "c"}, q.cli...), target...)...)
		args := map[string]any{"verb": "vec", "subject": "search", "collection": "c"}
		for i := 0; i < len(q.cli); i += 2 {
			args[q.cli[i][2:]] = q.cli[i+1]
		}
		mcpDoc := viaMCP(args)
		a, _ := json.Marshal(cli)
		b, _ := json.Marshal(mcpDoc)
		if string(a) != string(b) {
			t.Fatalf("CLI/MCP mismatch: %s != %s", a, b)
		}
		if hits := vecHits(t, cli); hits[:23] != `[{"distance":0,"id":"t"` {
			t.Fatalf("nearest is not the embedded item: %s", hits)
		}
	}
}

// TestVecTextNetEffect: the CLI declares net.egress only for text with an
// explicit endpoint, a dry run never calls the endpoint, and an effect cap
// without net refuses text before any request while vectors still run.
func TestVecTextNetEffect(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"index":0,"embedding":[0,1,0]}]}`)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/v1"
	target := []string{"--engine", "sqlite", "--mode", "persistent", "--path", filepath.Join(root, "v.db")}
	for _, args := range [][]string{
		{"create"},
		{"vec", "collection", "add", "--yes", "--collection", "c", "--dimension", "3", "--metric", "l2", "--model", "m"},
	} {
		if code, doc := dbRun(t, root, "", append(args, target...)...); code != 0 {
			t.Fatalf("%v: %v", args, doc)
		}
	}
	hasNet := func(doc map[string]any) bool {
		b, _ := json.Marshal(doc["effects"])
		return strings.Contains(string(b), "net.egress")
	}
	text := []string{"vec", "put", "--collection", "c", "--id", "t", "--text", "hi", "--endpoint", endpoint}
	vector := []string{"vec", "put", "--collection", "c", "--id", "v", "--vector", "[1,0,0]"}
	for _, preview := range []string{"--dry-run", "--explain"} {
		if code, doc := dbRun(t, root, "", append(append(text, preview), target...)...); code != 0 || !hasNet(doc) {
			t.Fatalf("%s text: exit %d %v", preview, code, doc)
		}
		if code, doc := dbRun(t, root, "", append(append(vector, preview), target...)...); code != 0 || hasNet(doc) {
			t.Fatalf("%s vector: exit %d %v", preview, code, doc)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("preview reached the endpoint %d times", calls.Load())
	}
	noNet, err := advice.ParseCap("read,write,destroy")
	if err != nil {
		t.Fatal(err)
	}
	capped := func(args ...string) (int, map[string]any) {
		var out, stderr bytes.Buffer
		rc := &tool.RunContext{Ctx: advice.WithCap(context.Background(), noNet), Dir: root, Env: os.Environ(), In: strings.NewReader(""), Out: &out, Err: &stderr}
		code := tool.Lookup("db").Run(rc, append(args, target...))
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("db %v: %q %q", args, out.String(), stderr.String())
		}
		return code, doc
	}
	if code, doc := capped(text...); code == 0 || errCode(doc) != "unsupported" || calls.Load() != 0 {
		t.Fatalf("text under a no-net cap: exit %d %v (calls %d)", code, doc, calls.Load())
	}
	if code, doc := capped(vector...); code != 0 || hasNet(doc) {
		t.Fatalf("vector under a no-net cap: exit %d %v", code, doc)
	}
	if code, doc := dbRun(t, root, "", append(text, target...)...); code != 0 || !hasNet(doc) || calls.Load() != 1 {
		t.Fatalf("uncapped text: exit %d %v (calls %d)", code, doc, calls.Load())
	}
}
