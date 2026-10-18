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

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/policy/advice"
)

// tsRunRaw runs the registered `db` command and returns the exit code with
// the raw stdout, so tests can assert exact tagged values (a JSON decode
// would round int64 extremes through float64).
func tsRunRaw(t *testing.T, dir, stdin string, args ...string) (int, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: dir, Env: os.Environ(), In: strings.NewReader(stdin), Out: &out, Err: &stderr}
	code := tool.Lookup("db").Run(rc, args)
	if code != 0 {
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err == nil {
			t.Fatalf("db %v: exit %d: %v", args, code, doc)
		}
		t.Fatalf("db %v: exit %d: stdout %q stderr %q", args, code, out.String(), stderr.String())
	}
	return code, out.String()
}

const tsPointsExtremes = `[{"ts":{"t":"int","v":"1577836800000000000"},"value":{"t":"int","v":"9223372036854775807"}},` +
	`{"ts":{"t":"int","v":"1577836860000000000"},"value":{"t":"int","v":"-9223372036854775808"}},` +
	`{"ts":{"t":"int","v":"1577836920000000000"},"value":{"t":"f64","v":1.5}}]`

// tsVerbsCLI drives the registered `db ts` path on one target: typed
// ingestion with int64 extremes, exact tagged round trip, bounded range,
// bucketed aggregation with the declared approximation warning,
// export/import, explicit retention and loud refusal of foreign,
// duplicated and misplaced flags.
func tsVerbsCLI(t *testing.T, root string, target, other []string) {
	run := func(want int, stdin string, args ...string) map[string]any {
		t.Helper()
		code, doc := dbRun(t, root, stdin, append(args, target...)...)
		if code != want {
			t.Fatalf("db %v: exit %d (want %d): %v", args, code, want, doc)
		}
		return doc
	}
	run(0, "", "create")
	if doc := run(0, "", "ts", "ingest", "--series", "cpu", "--labels", `{"host":"a"}`, "--points", tsPointsExtremes); doc["result"].(map[string]any)["ingested"] != float64(3) {
		t.Fatalf("ingest: %v", doc)
	}
	if doc := run(1, "", "ts", "ingest", "--series", "cpu", "--labels", `{"host":"a"}`, "--points", tsPointsExtremes); errCode(doc) != "conflict" {
		t.Fatalf("duplicate stamp: %v", doc)
	}
	// The tagged extremes survive the CLI round trip with tag and bits.
	_, stdout := tsRunRaw(t, root, "", append([]string{"ts", "range", "--series", "cpu", "--labels", `{"host":"a"}`}, target...)...)
	for _, want := range []string{`"t":"int"`, `"v":"9223372036854775807"`, `"v":"-9223372036854775808"`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("range lost tagged int64 (%s): %s", want, stdout)
		}
	}
	// Bounds travel in --input; start is inclusive.
	bounded := run(0, `{"start":{"t":"int","v":"1577836860000000000"}}`, "ts", "range", "--series", "cpu", "--labels", `{"host":"a"}`, "--input", "-")
	if n := len(bounded["result"].(map[string]any)["points"].([]any)); n != 2 {
		t.Fatalf("bounded range points %d (want 2): %v", n, bounded)
	}
	agg := run(0, "", "ts", "agg", "--series", "cpu", "--labels", `{"host":"a"}`, "--bucket", "60s", "--funcs", "count,min,max")
	if len(agg["result"].(map[string]any)["buckets"].([]any)) == 0 {
		t.Fatalf("agg buckets: %v", agg)
	}
	warnings, _ := json.Marshal(agg["warnings"])
	if !strings.Contains(string(warnings), "approximate") {
		t.Fatalf("agg over int64 extremes declares nothing: %v", agg)
	}
	// Foreign, duplicated and misplaced flags fail loudly.
	if doc := run(2, "", "ts", "range", "--series", "cpu", "--bucket", "x"); errCode(doc) != "invalid_input" {
		t.Fatalf("foreign flag: %v", doc)
	}
	if doc := run(2, `{"series":"cpu"}`, "ts", "range", "--input", "-", "--series", "cpu"); errCode(doc) != "invalid_input" {
		t.Fatalf("duplicated value: %v", doc)
	}
	if doc := run(2, "", "ts", "ingest", "--series", "cpu", "--labels", `{"host":"a"}`, "--points", tsPointsExtremes, "--out", "x"); errCode(doc) != "invalid_input" {
		t.Fatalf("misplaced --out: %v", doc)
	}
	if doc := run(2, "", "ts", "frobnicate"); errCode(doc) != "invalid_input" {
		t.Fatalf("unknown verb: %v", doc)
	}
	dump := filepath.Join(root, "tsdump")
	run(0, "", "ts", "export", "--out", dump)
	if code, doc := dbRun(t, root, "", append([]string{"create"}, other...)...); code != 0 {
		t.Fatalf("create other: %v", doc)
	}
	if code, doc := dbRun(t, root, "", append([]string{"ts", "import", "--in", dump, "--replace", "--yes"}, other...)...); code != 0 {
		t.Fatalf("import: %v", doc)
	}
	if code, stdout := tsRunRaw(t, root, "", append([]string{"ts", "range", "--series", "cpu", "--labels", `{"host":"a"}`}, other...)...); code != 0 || !strings.Contains(stdout, `"v":"9223372036854775807"`) {
		t.Fatalf("imported range: exit %d: %s", code, stdout)
	}
	// Retention is exclusive: unconfirmed it refuses, confirmed it deletes
	// every point older than now minus the window.
	if doc := run(1, "", "ts", "retain", "--series", "cpu", "--retention", "1d"); errCode(doc) != "confirmation_required" {
		t.Fatalf("unconfirmed retain: %v", doc)
	}
	if doc := run(0, "", "ts", "retain", "--series", "cpu", "--retention", "1d", "--yes"); doc["result"].(map[string]any)["deleted"] != float64(3) {
		t.Fatalf("retain: %v", doc)
	}
	if doc := run(0, "", "ts", "range", "--series", "cpu", "--labels", `{"host":"a"}`); len(doc["result"].(map[string]any)["points"].([]any)) != 0 {
		t.Fatalf("retained range: %v", doc)
	}
}

func TestTSVerbsPersistentCLI(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	tsVerbsCLI(t, root,
		[]string{"--engine", "sqlite", "--mode", "persistent", "--path", filepath.Join(root, "a.db")},
		[]string{"--engine", "sqlite", "--mode", "persistent", "--path", filepath.Join(root, "b.db")})
}

// TestTSVerbsMemorySessionCLI runs the typed verbs against memory targets
// in an owner session, which re-execs this binary as an independent
// process.
func TestTSVerbsMemorySessionCLI(t *testing.T) {
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
	tsVerbsCLI(t, root,
		[]string{"--engine", "sqlite", "--mode", "memory", "--session", session, "--name", "a"},
		[]string{"--engine", "sqlite", "--mode", "memory", "--session", session, "--name", "b"})
}

// TestTSRegisteredCLIAndMCP proves CLI/MCP parity for typed ingestion and
// range on a persistent target through the same declared flags and runner.
func TestTSRegisteredCLIAndMCP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	path := filepath.Join(root, "t.db")
	target := []string{"--engine", "sqlite", "--mode", "persistent", "--path", path}
	series := []string{"ts", "range", "--series", "cpu", "--labels", `{"host":"a"}`}
	for _, args := range [][]string{
		{"create"},
		{"ts", "ingest", "--series", "cpu", "--labels", `{"host":"a"}`, "--points", tsPointsExtremes},
	} {
		if code, doc := dbRun(t, root, "", append(args, target...)...); code != 0 {
			t.Fatalf("%v: %v", args, doc)
		}
	}
	_, cli := dbRun(t, root, "", append(series, target...)...)
	schema := Schema()
	found := false
	for _, f := range schema.Flags {
		if f.Name == "series" {
			found = true
		}
	}
	if !found {
		t.Fatal("MCP schema lacks ts series input")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := []string{"--engine", "sqlite", "--mode", "persistent", "--path", path, "--series", "cpu", "--labels", `{"host":"a"}`, "ts", "range"}
	stdout, stderr, code := MCPCommand().Run(ctx, argv, "", root)
	if code != 0 {
		t.Fatalf("MCP ts range: exit %d: %s %s", code, stdout, stderr)
	}
	var mcpDoc map[string]any
	if err := json.Unmarshal([]byte(stdout), &mcpDoc); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(cli)
	b, _ := json.Marshal(mcpDoc)
	if string(a) != string(b) {
		t.Fatalf("CLI/MCP mismatch: %s != %s", a, b)
	}
	if !strings.Contains(string(a), `"v":"9223372036854775807"`) {
		t.Fatalf("parity envelope lost the tagged extreme: %s", a)
	}
}

// TestTSRemoteNetEffect: the CLI declares net.egress for remote ts ops
// through the shared input-aware seam, a dry run or explain performs zero
// HTTP, an effect cap without net refuses before any request, and local ts
// ops never declare net. Execution runs the bounded query and ingest
// against the fixture.
func TestTSRemoteNetEffect(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	var calls atomic.Int32
	var lastBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		lastBody.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/v1/import") {
			io.WriteString(w, `{}`)
			return
		}
		io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[`+
			`{"metric":{"__name__":"cpu","host":"a"},"values":[[1719792000,"1.5"],[1719792060,"2.5"]]}`+
			`]}}`)
	}))
	defer srv.Close()
	target := []string{"--engine", "sqlite", "--mode", "persistent", "--path", filepath.Join(root, "t.db")}
	if code, doc := dbRun(t, root, "", append([]string{"create"}, target...)...); code != 0 {
		t.Fatalf("create: %v", doc)
	}
	bounds := `{"start":{"t":"int","v":"1719792000000000000"},"end":{"t":"int","v":"1719792120000000000"}}`
	query := []string{"ts", "remote", "query", "--endpoint", srv.URL, "--expr", "up", "--step", "60s", "--input", "-"}
	ingestDoc := `{"series_list":[{"name":"cpu","labels":{"host":"a"},` +
		`"points":[{"ts":{"t":"int","v":"1719792000000000000"},"value":{"t":"f64","v":1.5}}]}]}`
	ingest := []string{"ts", "remote-ingest", "--endpoint", srv.URL, "--input", "-"}
	hasNet := func(doc map[string]any) bool {
		b, _ := json.Marshal(doc["effects"])
		return strings.Contains(string(b), "net.egress")
	}
	for _, preview := range []string{"--dry-run", "--explain"} {
		if code, doc := dbRun(t, root, bounds, append(append(append([]string{}, query...), preview), target...)...); code != 0 || !hasNet(doc) {
			t.Fatalf("%s remote query: exit %d %v", preview, code, doc)
		}
		if code, doc := dbRun(t, root, ingestDoc, append(append(append([]string{}, ingest...), preview), target...)...); code != 0 || !hasNet(doc) {
			t.Fatalf("%s remote ingest: exit %d %v", preview, code, doc)
		}
		if code, doc := dbRun(t, root, "", append([]string{"ts", "range", "--series", "cpu", preview}, target...)...); code != 0 || hasNet(doc) {
			t.Fatalf("%s local range declares net: exit %d %v", preview, code, doc)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("preview reached the endpoint %d times", calls.Load())
	}
	noNet, err := advice.ParseCap("read,write,destroy")
	if err != nil {
		t.Fatal(err)
	}
	capped := func(stdin string, args ...string) (int, map[string]any) {
		var out, stderr bytes.Buffer
		rc := &tool.RunContext{Ctx: advice.WithCap(context.Background(), noNet), Dir: root, Env: os.Environ(), In: strings.NewReader(stdin), Out: &out, Err: &stderr}
		code := tool.Lookup("db").Run(rc, append(args, target...))
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("db %v: %q %q", args, out.String(), stderr.String())
		}
		return code, doc
	}
	if code, doc := capped(bounds, query...); code == 0 || errCode(doc) != "unsupported" || calls.Load() != 0 {
		t.Fatalf("remote query under a no-net cap: exit %d %v (calls %d)", code, doc, calls.Load())
	}
	if code, doc := capped(bounds, "ts", "range", "--series", "cpu"); code != 0 || hasNet(doc) {
		t.Fatalf("local range under a no-net cap: exit %d %v", code, doc)
	}
	if code, doc := dbRun(t, root, bounds, append(query, target...)...); code != 0 || !hasNet(doc) || calls.Load() != 1 {
		t.Fatalf("uncapped remote query: exit %d %v (calls %d)", code, doc, calls.Load())
	} else if doc["result"].(map[string]any)["series"].([]any)[0].(map[string]any)["name"] != "cpu" {
		t.Fatalf("remote query series: %v", doc)
	}
	if code, doc := dbRun(t, root, ingestDoc, append(ingest, target...)...); code != 0 || calls.Load() != 2 {
		t.Fatalf("uncapped remote ingest: exit %d %v (calls %d)", code, doc, calls.Load())
	} else if doc["result"].(map[string]any)["points"] != float64(1) {
		t.Fatalf("remote ingest result: %v", doc)
	}
	if body, _ := lastBody.Load().(string); !strings.Contains(body, `cpu{host="a"}`) {
		t.Fatalf("remote ingest body is not Prometheus text: %q", body)
	}
}
