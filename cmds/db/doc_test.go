package dbcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/yoke/mcp"
)

func errCode(doc map[string]any) any {
	if e, ok := doc["error"].(map[string]any); ok {
		return e["code"]
	}
	return nil
}

func TestDocVerbParsing(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		op   string
		rest int
	}{
		{[]string{"get", "T"}, "doc.get", 1},
		{[]string{"collection", "create", "T"}, "doc.collection.create", 1},
		{[]string{"collection-drop", "T"}, "doc.collection.drop", 1},
		{[]string{"index", "add", "T"}, "doc.index.add", 1},
		{[]string{"index-list"}, "doc.index.list", 0},
		{[]string{"export", "T"}, "doc.export", 1},
	} {
		op, rest, err := docVerb(tc.in)
		if err != nil || op != tc.op || len(rest) != tc.rest {
			t.Fatalf("%v: %s %v %v", tc.in, op, rest, err)
		}
	}
	for _, bad := range [][]string{nil, {"query"}, {"collection"}, {"index", "rebuild"}, {"collection-x"}} {
		if _, _, err := docVerb(bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}

// TestDocCLIAndTypedMCPParity drives the registered db command and the typed
// db_lifecycle MCP tool in process against one persistent target.
func TestDocCLIAndTypedMCPParity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	path := filepath.Join(root, "doc.db")
	target := []string{"--engine", "sqlite", "--mode", "persistent", "--path", path}
	cli := func(stdin string, args ...string) (int, map[string]any) {
		return dbRun(t, root, stdin, append(args, target...)...)
	}
	if code, doc := cli("", "create"); code != 0 {
		t.Fatalf("create: %v", doc)
	}
	if code, doc := cli("", "doc", "put", "--collection", "c", "--doc", `{"a":1}`); code != 1 || errCode(doc) != "not_found" {
		t.Fatalf("implicit collection: %d %v", code, doc)
	}
	if code, doc := cli("", "doc", "collection", "create", "--collection", "c", "--explain"); code != 0 || doc["result"].(map[string]any)["declaration"].(map[string]any)["hold"] != "exclusive" {
		t.Fatalf("explain: %v", doc)
	}
	if code, doc := cli("", "doc", "collection", "create", "--collection", "c"); code != 1 || errCode(doc) != "confirmation_required" {
		t.Fatalf("unconfirmed: %d %v", code, doc)
	}
	if code, doc := cli("", "doc", "collection", "create", "--collection", "c", "--yes"); code != 0 || doc["op"] != "doc.collection.create" {
		t.Fatalf("collection create: %v", doc)
	}
	if code, doc := cli("", "doc", "put", "--collection", "c", "--id", "x", "--doc", `{"a":`); code != 2 || errCode(doc) != "invalid_input" {
		t.Fatalf("malformed --doc: %d %v", code, doc)
	}
	if code, doc := cli("", "doc", "find", "--collection", "c", "--filter", `{"field":"a') or 1=1 --","op":"eq","value":1}`, "--dry-run"); code != 2 || errCode(doc) != "invalid_input" {
		t.Fatalf("dry-run bad filter: %d %v", code, doc)
	}
	if code, doc := cli("", "doc", "put", "--collection", "c", "--id", "x", "--doc", `{"big":12345678901234567890,"d":1.50}`); code != 0 || doc["mutation_class"] != "write" {
		t.Fatalf("put: %v", doc)
	}
	code, doc := cli(`{"collection":"c","ops":[{"op":"patch","id":"x","patch":{"n":{"m":[true,null]}},"if_revision":1},{"op":"put","id":"y","doc":{"s":"t"}}]}`, "doc", "batch", "--input", "-")
	if code != 0 || doc["result"].(map[string]any)["committed"] != true {
		t.Fatalf("batch: %v", doc)
	}
	if code, doc := cli("", "doc", "patch", "--collection", "c", "--id", "x", "--patch", `{"z":1}`, "--if-revision", "1"); code != 1 || errCode(doc) != "conflict" {
		t.Fatalf("stale patch: %d %v", code, doc)
	}
	if code, doc := cli(`{"collection":"c"}`, "doc", "get", "--input", "-", "--collection", "c", "--id", "x"); code != 2 || errCode(doc) != "invalid_input" {
		t.Fatalf("value twice: %d %v", code, doc)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "db-doc-test", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(kv ...any) (int, map[string]any, string) {
		t.Helper()
		args := map[string]any{"verb": "doc", "engine": "sqlite", "mode": "persistent", "path": path}
		for i := 0; i < len(kv); i += 2 {
			args[kv[i].(string)] = kv[i+1]
		}
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
		return out.ExitCode, doc, out.Stdout
	}
	if code, doc, _ := call("subject", "index-add", "collection", "c", "index", "by_s", "field", "s", "unique", true, "yes", true); code != 0 {
		t.Fatalf("mcp index add: %v", doc)
	}
	if code, doc, _ := call("subject", "put", "collection", "c", "id", "z", "doc", `{"s":"t"}`); code != 1 || errCode(doc) != "conflict" {
		t.Fatalf("mcp unique: %d %v", code, doc)
	}
	// The exact stored document text reaches the MCP client.
	if code, doc, stdout := call("subject", "get", "collection", "c", "id", "x"); code != 0 || !strings.Contains(stdout, `"doc":{"big":12345678901234567890,"d":1.50,"n":{"m":[true,null]}}`) {
		t.Fatalf("mcp get: %v %s", doc, stdout)
	}
	// CLI and MCP return the same envelope for the same request.
	for _, req := range [][]string{
		{"subject", "find", "collection", "c", "filter", `{"field":"s","op":"eq","param":"s"}`, "params", `{"s":"t"}`, "limit", "1"},
		{"subject", "collection-list"},
		{"subject", "index-list", "collection", "c"},
	} {
		kv := make([]any, len(req))
		args := []string{"doc", req[1]}
		for i, v := range req {
			kv[i] = v
			if i >= 2 && i%2 == 0 {
				args = append(args, "--"+v, req[i+1])
			}
		}
		if req[1] == "find" {
			kv[len(kv)-1] = 1
		}
		_, viaCLI := cli("", args...)
		_, viaMCP, _ := call(kv...)
		a, _ := json.Marshal(viaCLI)
		b, _ := json.Marshal(viaMCP)
		if string(a) != string(b) || viaCLI["ok"] != true {
			t.Fatalf("CLI/MCP envelopes differ for %v\ncli %s\nmcp %s", req, a, b)
		}
	}
	out := filepath.Join(root, "exp")
	if code, doc, _ := call("subject", "export", "out", out); code != 0 || doc["op"] != "doc.export" {
		t.Fatalf("mcp export: %v", doc)
	}
	if code, doc := cli("", "doc", "import", "--in", out, "--on-conflict", "skip"); code != 0 || doc["result"].(map[string]any)["records_applied"] != float64(0) {
		t.Fatalf("cli import: %v", doc)
	}
	if code, doc, _ := call("subject", "collection-drop", "collection", "c"); code != 1 || errCode(doc) != "confirmation_required" {
		t.Fatalf("mcp unconfirmed drop: %v", doc)
	}
	if code, doc, _ := call("subject", "collection-drop", "collection", "c", "yes", true); code != 0 {
		t.Fatalf("mcp drop: %v", doc)
	}
}

// --- independent-process tests (manager remote gate; not run on Dragon) ---

// cliProc runs `db ARGS` as a separate OS process (this test binary re-exec'd
// through TestMain), so every client below is genuinely independent.
func cliProc(t *testing.T, dir string, args ...string) (int, map[string]any) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"db"}, args...)...)
	cmd.Dir = dir
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	dec := json.NewDecoder(&out)
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("db %v: stdout %q stderr %q", args, out.String(), stderr.String())
	}
	return code, doc
}

// TestIndependentDocClientsBothModes: independent processes share one
// logical document database directly (persistent) and through an explicit
// owner session (memory). Contested if-revision increments lose nothing, a
// same-ID if-absent insert has one winner, a destructive drop under another
// client's hold is refused, and the memory database ends with its owner.
func TestIndependentDocClientsBothModes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(root, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	_, doc := cliProc(t, root, "session", "start", "--idle-ttl", "5m")
	session := doc["result"].(map[string]any)["id"].(string)
	stopped := false
	defer func() {
		if !stopped {
			cliProc(t, root, "session", "stop", "--session", session, "--yes")
		}
	}()
	for _, target := range [][]string{
		{"--engine", "sqlite", "--mode", "persistent", "--path", filepath.Join(root, "shared.db")},
		{"--engine", "sqlite", "--mode", "memory", "--session", session, "--name", "shared"},
	} {
		t.Run(target[3], func(t *testing.T) {
			run := func(args ...string) (int, map[string]any) { return cliProc(t, root, append(args, target...)...) }
			if code, doc := run("create"); code != 0 {
				t.Fatalf("create: %v", doc)
			}
			if code, doc := run("doc", "collection", "create", "--collection", "c", "--yes"); code != 0 {
				t.Fatalf("collection create: %v", doc)
			}
			run("doc", "put", "--collection", "c", "--id", "ctr", "--doc", `{"n":0}`)
			const clients, each = 3, 4
			var wg sync.WaitGroup
			var mu sync.Mutex
			var failures []string
			winners := 0
			for c := 0; c < clients; c++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if code, _ := run("doc", "put", "--collection", "c", "--id", "once", "--if-absent", "--doc", fmt.Sprintf(`{"by":%d}`, c)); code == 0 {
						mu.Lock()
						winners++
						mu.Unlock()
					}
					for done, tries := 0, 0; done < each; tries++ {
						if tries > 200 {
							mu.Lock()
							failures = append(failures, "too many retries")
							mu.Unlock()
							return
						}
						code, got := run("doc", "get", "--collection", "c", "--id", "ctr")
						if code != 0 {
							continue
						}
						d := got["result"].(map[string]any)
						n, _ := d["doc"].(map[string]any)["n"].(json.Number).Int64()
						code, got = run("doc", "put", "--collection", "c", "--id", "ctr", "--if-revision", d["revision"].(json.Number).String(), "--doc", fmt.Sprintf(`{"n":%d}`, n+1))
						switch {
						case code == 0:
							done++
						case errCode(got) == "conflict" || errCode(got) == "busy":
						default:
							mu.Lock()
							failures = append(failures, fmt.Sprint(got))
							mu.Unlock()
							return
						}
					}
				}()
			}
			wg.Wait()
			if len(failures) > 0 || winners != 1 {
				t.Fatalf("failures %v winners %d", failures, winners)
			}
			_, got := run("doc", "get", "--collection", "c", "--id", "ctr")
			if d := got["result"].(map[string]any); d["doc"].(map[string]any)["n"].(json.Number).String() != fmt.Sprint(clients*each) {
				t.Fatalf("lost updates: %v", d)
			}
			// A destructive drop from another agent's process is refused while
			// a hold is active, then succeeds after release. The holder has
			// its own episode: a same-episode caller without the epoch is
			// fenced (stale_epoch), not refused as another holder.
			hold := exec.Command(os.Args[0], append(append([]string{"db", "hold"}, target...), "--", "sleep", "3")...)
			hold.Dir = root
			hold.Env = append(os.Environ(), "BASHY_EPISODE=doc-test-holder")
			if err := hold.Start(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Second)
			code, got := run("doc", "collection", "drop", "--collection", "c", "--yes", "--wait", "200ms")
			if code == 0 || (errCode(got) != "claim_held" && errCode(got) != "busy") {
				t.Fatalf("drop under hold: %d %v", code, got)
			}
			if err := hold.Wait(); err != nil {
				t.Fatalf("hold child: %v", err)
			}
			if code, got := run("doc", "get", "--collection", "c", "--id", "ctr"); code != 0 || got["result"].(map[string]any)["doc"].(map[string]any)["n"].(json.Number).String() != fmt.Sprint(clients*each) {
				t.Fatalf("denied drop changed data: %d %v", code, got)
			}
			if code, got := run("doc", "collection", "drop", "--collection", "c", "--yes"); code != 0 {
				t.Fatalf("drop after release: %v", got)
			}
		})
	}
	cliProc(t, root, "session", "stop", "--session", session, "--yes")
	stopped = true
	if code, doc := cliProc(t, root, "doc", "collection", "list", "--engine", "sqlite", "--mode", "memory", "--session", session, "--name", "shared"); code == 0 {
		t.Fatalf("memory database outlived its owner: %v", doc)
	}
}
