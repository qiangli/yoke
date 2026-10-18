package dataclients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/external/gws"
)

const canary = "CANARY-s3cr3t-Zx9qLm42"

// TestHelperProcess is the stand-in client: it replays a recorded output
// fixture (never a live service) so the real exec/cancel/bound paths run.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("DC_HELPER") != "1" {
		return
	}
	in, _ := io.ReadAll(os.Stdin)
	if p := os.Getenv("DC_STDIN_OUT"); p != "" {
		_ = os.WriteFile(p, in, 0o600)
		_ = os.WriteFile(p+".env", []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
	sub := strings.NewReplacer("<US>", "\x1f", "<RS>", "\x1e", "<NULL>", os.Getenv("DC_NULL"))
	switch os.Getenv("DC_MODE") {
	case "sleep":
		time.Sleep(time.Minute)
	case "flood":
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(0)
			}
		}
	case "fail":
		b, _ := os.ReadFile(os.Getenv("DC_FIXTURE"))
		fmt.Fprint(os.Stderr, strings.ReplaceAll(string(b), "CANARY-PLACEHOLDER", os.Getenv("DC_CANARY")))
		os.Exit(2)
	default:
		b, _ := os.ReadFile(os.Getenv("DC_FIXTURE"))
		fmt.Fprint(os.Stdout, sub.Replace(string(b)))
	}
	os.Exit(0)
}

func endpoint(client string) Endpoint {
	return Endpoint{Client: client, Host: "db.example.test", Port: 5432, Database: "app", User: "app", TLS: TLSVerify, CAFile: "/etc/ca.pem", Password: canary}
}

func helperInv(t *testing.T, client string, op Op, fixture, mode string) Invocation {
	t.Helper()
	inv, err := Build(endpoint(client), op, os.Args[0])
	if err != nil {
		t.Fatalf("Build(%s): %v", client, err)
	}
	inv.Args = []string{"-test.run=^TestHelperProcess$"}
	dir := t.TempDir()
	inv.Env = append(inv.Env, "DC_HELPER=1", "DC_MODE="+mode, "DC_FIXTURE="+filepath.Join("testdata", fixture),
		"DC_STDIN_OUT="+filepath.Join(dir, "stdin"), "DC_NULL="+inv.NullToken, "DC_CANARY="+canary)
	return inv
}

func stdinSeen(t *testing.T, inv Invocation) string {
	t.Helper()
	for _, kv := range inv.Env {
		if p, ok := strings.CutPrefix(kv, "DC_STDIN_OUT="); ok {
			b, _ := os.ReadFile(p)
			return string(b)
		}
	}
	return ""
}

func TestAdmissionRecords(t *testing.T) {
	want := map[string]string{"psql": "PostgreSQL", "mongosh": "Apache-2.0", "valkey-cli": "BSD-3-Clause", "snow": "Apache-2.0"}
	if got := strings.Join(Names(), ","); got != "mongosh,psql,snow,valkey-cli" {
		t.Fatalf("admitted clients = %s", got)
	}
	for n, lic := range want {
		c, _ := Lookup(n)
		if c.License != lic {
			t.Errorf("%s licence = %q, want %q", n, c.License, lic)
		}
		if c.RemoteLicense == "" || c.RemoteLicense == c.License || c.RemoteEngine == "" || c.Fidelity == "" {
			t.Errorf("%s: remote engine/licence must be recorded separately from the client licence", n)
		}
	}
	if c, _ := Lookup("mongosh"); !strings.Contains(c.RemoteLicense, "SSPL") {
		t.Error("mongosh remote licence must name SSPL for the MongoDB server")
	}
	if err := HostOnlyError("psql"); !strings.Contains(err.Error(), "no sha256-pinned managed release") {
		t.Errorf("host-only diagnostic = %v", err)
	}
}

func TestResolveEndpoint(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	base := map[string]string{"PSQL_HOST": "db.example.test", "PSQL_USER": "app", "PSQL_PASSWORD": canary}
	cases := []struct {
		name string
		m    map[string]string
		code gws.Code
	}{
		{"no host: ambient PGHOST is not a fallback", map[string]string{"PGHOST": "x", "PGPASSWORD": canary}, gws.CodeMissingCredentials},
		{"no credential", map[string]string{"PSQL_HOST": "db.example.test"}, gws.CodeMissingCredentials},
		{"plaintext off-loopback", merge(base, "PSQL_TLS", "plaintext-loopback"), gws.CodeInvalidInput},
		{"unknown tls", merge(base, "PSQL_TLS", "none"), gws.CodeInvalidInput},
		{"bad port", merge(base, "PSQL_PORT", "99999"), gws.CodeInvalidInput},
		{"both credential forms", merge(base, "PSQL_PASSWORD_FILE", "/x"), gws.CodeInvalidInput},
	}
	for _, c := range cases {
		if _, err := ResolveEndpoint("psql", env(c.m)); err == nil || err.Code != c.code {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
	ep, err := ResolveEndpoint("psql", env(base))
	if err != nil || ep.TLS != TLSVerify || ep.Port != 5432 {
		t.Fatalf("default must be TLS verify on 5432: %+v %v", ep, err)
	}
	if lb, err := ResolveEndpoint("psql", env(merge(merge(base, "PSQL_HOST", "127.0.0.1"), "PSQL_TLS", "plaintext-loopback"))); err != nil || lb.TLS != TLSPlaintextLoopback {
		t.Errorf("loopback plaintext should be allowed: %v", err)
	}
	if _, err := ResolveEndpoint("snow", env(map[string]string{"SNOW_HOST": "acct", "SNOW_PASSWORD": canary, "SNOW_TLS": "plaintext-loopback"})); err == nil {
		t.Error("snow plaintext must be refused")
	}
	pf := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(pf, []byte(canary+"\n"), 0o600)
	if f, err := ResolveEndpoint("psql", env(map[string]string{"PSQL_HOST": "h", "PSQL_PASSWORD_FILE": pf})); err != nil || f.Password != canary {
		t.Errorf("password file: %v", err)
	}
	if _, err := ResolveEndpoint("psql", env(map[string]string{"PSQL_HOST": "h", "PSQL_PASSWORD_FILE": "/nonexistent/" + canary})); err == nil || strings.Contains(err.Message, canary) {
		t.Errorf("unreadable password file must fail without echoing the path-borne canary: %v", err)
	}
	if s := ep.RedactedScope(); strings.Contains(s, canary) || !strings.Contains(s, "credential=<set>") {
		t.Errorf("scope leaks or omits: %s", s)
	}
}

func merge(m map[string]string, k, v string) map[string]string {
	out := map[string]string{k: v}
	for a, b := range m {
		if _, ok := out[a]; !ok {
			out[a] = b
		}
	}
	return out
}

func TestBuildSecretsNeverInArgvOrStdin(t *testing.T) {
	stmts := map[string]string{"psql": "select 1", "mongosh": "db.t.find({})", "valkey-cli": "GET k", "snow": "select 1"}
	for _, n := range Names() {
		inv, err := Build(endpoint(n), Op{Statement: stmts[n]}, "/bin/"+n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if strings.Contains(strings.Join(inv.Args, " "), canary) || strings.Contains(inv.Stdin, canary) {
			t.Errorf("%s: credential reached argv/stdin", n)
		}
		if !strings.Contains(strings.Join(inv.Env, "\n"), canary) {
			t.Errorf("%s: credential must travel by env", n)
		}
		if inv.Op.MaxRows != DefaultMaxRows || inv.Op.MaxBytes != DefaultMaxBytes || inv.Op.Timeout != DefaultTimeout {
			t.Errorf("%s: finite defaults not applied: %+v", n, inv.Op)
		}
	}
}

func TestTLSArgs(t *testing.T) {
	inv, _ := Build(endpoint("valkey-cli"), Op{Statement: "GET k"}, "valkey-cli")
	if a := strings.Join(inv.Args, " "); !strings.Contains(a, "--tls") || !strings.Contains(a, "--cacert /etc/ca.pem") {
		t.Errorf("valkey TLS args: %s", a)
	}
	inv, _ = Build(endpoint("psql"), Op{Statement: "select 1"}, "psql")
	if e := strings.Join(inv.Env, "\n"); !strings.Contains(e, "PGSSLMODE=verify-full") || !strings.Contains(e, "PGSSLROOTCERT=/etc/ca.pem") {
		t.Errorf("psql TLS env: %s", e)
	}
	inv, _ = Build(endpoint("mongosh"), Op{Statement: "db.t.find()"}, "mongosh")
	if e := strings.Join(inv.Env, "\n"); !strings.Contains(e, "tls=true") || !strings.Contains(e, "tlsCAFile=") {
		t.Errorf("mongosh TLS uri: %s", e)
	}
}

func TestTransactionRollbackByDefault(t *testing.T) {
	cases := []struct {
		client, stmt, rollback, commit string
	}{
		{"psql", "update t set a=1", "ROLLBACK;", "COMMIT;"},
		{"snow", "update t set a=1", "ROLLBACK;", "COMMIT;"},
		{"valkey-cli", "SET k v", "DISCARD", "EXEC"},
		{"mongosh", "db.t.updateOne({}, {$set:{a:1}})", "abortTransaction();\n} catch", "commitTransaction()"},
	}
	for _, c := range cases {
		inv, err := Build(endpoint(c.client), Op{Statement: c.stmt, Write: true}, c.client)
		if err != nil || !strings.Contains(inv.Stdin, c.rollback) || strings.Contains(inv.Stdin, c.commit) {
			t.Errorf("%s: write without commit must roll back: err=%v\n%s", c.client, err, inv.Stdin)
		}
		inv, err = Build(endpoint(c.client), Op{Statement: c.stmt, Write: true, Commit: true}, c.client)
		if err != nil || !strings.Contains(inv.Stdin, c.commit) {
			t.Errorf("%s: explicit commit must commit: err=%v", c.client, err)
		}
	}
	inv, _ := Build(endpoint("psql"), Op{Statement: "select 1"}, "psql")
	if !strings.HasPrefix(inv.Stdin, "BEGIN READ ONLY;") || !strings.Contains(strings.Join(inv.Env, "\n"), "default_transaction_read_only=on") {
		t.Errorf("psql read must be a read-only transaction: %q", inv.Stdin)
	}
}

func TestBuildRefusals(t *testing.T) {
	cases := []struct {
		name   string
		client string
		op     Op
		code   gws.Code
	}{
		{"empty", "psql", Op{}, gws.CodeInvalidInput},
		{"commit without write", "psql", Op{Statement: "select 1", Commit: true}, gws.CodeInvalidInput},
		{"rows over cap", "psql", Op{Statement: "select 1", MaxRows: CapMaxRows + 1}, gws.CodeInvalidInput},
		{"timeout over cap", "snow", Op{Statement: "select 1", Timeout: CapTimeout + 1}, gws.CodeInvalidInput},
		{"oversize statement", "psql", Op{Statement: strings.Repeat("x", MaxStatement+1)}, gws.CodeOutputLimit},
		{"psql shell escape", "psql", Op{Statement: "select 1\n\\! rm -rf /"}, gws.CodeUnsupported},
		{"valkey subscribe", "valkey-cli", Op{Statement: "SUBSCRIBE ch", Write: true}, gws.CodeUnsupported},
		{"valkey flush", "valkey-cli", Op{Statement: "FLUSHALL", Write: true}, gws.CodeUnsupported},
		{"valkey write as read", "valkey-cli", Op{Statement: "SET k v"}, gws.CodeUnsupported},
		{"valkey manual multi", "valkey-cli", Op{Statement: "MULTI", Write: true}, gws.CodeUnsupported},
		{"unknown client", "mysql", Op{Statement: "select 1"}, gws.CodeUnsupported},
	}
	for _, c := range cases {
		if _, err := Build(endpoint(c.client), c.op, "x"); err == nil || err.Code != c.code {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
}

func TestRunRecordedFixtures(t *testing.T) {
	inv := helperInv(t, "psql", Op{Statement: "select * from t"}, "psql_select.out", "")
	res, err := Run(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Columns, ",") != "id,name,note,amount" || res.RecordCount != 2 || res.Partial || res.Mutation != "none" {
		t.Fatalf("psql result: %+v", res)
	}
	row := res.Records[0].([]any)
	if row[2] != nil || row[3] != "0012.50" {
		t.Errorf("NULL must be nil and text kept verbatim (no numeric coercion): %#v", row)
	}
	if r2 := res.Records[1].([]any); r2[2] != "line1\nline2" || r2[3] != "9007199254740993" {
		t.Errorf("embedded newline / big int mangled: %#v", r2)
	}
	if in := stdinSeen(t, inv); !strings.Contains(in, "BEGIN READ ONLY;") || !strings.HasSuffix(strings.TrimSpace(in), "ROLLBACK;") {
		t.Errorf("statement did not arrive on stdin in a rolled-back tx: %q", in)
	}

	for _, c := range []struct{ client, fixture, stmt, wantNum string }{
		{"mongosh", "mongosh_docs.out", "db.t.find({})", "9007199254740993"},
		{"valkey-cli", "valkey_values.out", "GET k", "9007199254740993"},
		{"snow", "snow_rows.out", "select * from t", ""},
	} {
		res, err := Run(context.Background(), helperInv(t, c.client, Op{Statement: c.stmt}, c.fixture, ""))
		if err != nil || res.RecordCount == 0 || res.Partial {
			t.Fatalf("%s: %v %+v", c.client, err, res)
		}
		b, _ := json.Marshal(res.Records)
		if c.wantNum != "" && !strings.Contains(string(b), c.wantNum) {
			t.Errorf("%s: 64-bit integer lost precision: %s", c.client, b)
		}
		if c.client == "mongosh" && (!strings.Contains(string(b), `"$numberDecimal":"12.50"`) || !strings.Contains(string(b), `"missing":null`)) {
			t.Errorf("mongosh EJSON types/null not preserved: %s", b)
		}
		if c.client == "snow" && !strings.Contains(string(b), `"12345678901234567890.123456789"`) {
			t.Errorf("snow decimal precision lost: %s", b)
		}
	}
}

func TestEnvScrubbedOfAmbientClientVars(t *testing.T) {
	out := childEnv("psql", []string{"PGHOST=evil", "PGPASSWORD=ambient", "PATH=/bin"}, []string{"PGPASSWORD=" + canary})
	j := strings.Join(out, "\n")
	if strings.Contains(j, "evil") || strings.Contains(j, "ambient") || !strings.Contains(j, "PATH=/bin") || !strings.Contains(j, "PGPASSWORD="+canary) {
		t.Errorf("ambient scrub wrong:\n%s", j)
	}
}

func TestBoundedReads(t *testing.T) {
	res, err := Run(context.Background(), helperInv(t, "mongosh", Op{Statement: "db.t.find({})", MaxRows: 2}, "mongosh_docs.out", ""))
	if err != nil || !res.Partial || res.Reason != "row-limit" || res.RecordCount != 2 {
		t.Fatalf("row limit must be explicit partial: %v %+v", err, res)
	}
	res, err = Run(context.Background(), helperInv(t, "mongosh", Op{Statement: "db.t.find({})", MaxBytes: 250}, "mongosh_docs.out", ""))
	if err != nil || !res.Partial || res.Reason != "byte-limit" || res.RecordCount != 1 {
		t.Fatalf("byte limit must drop the cut-off tail and say partial: %v %+v", err, res)
	}
	start := time.Now()
	inv := helperInv(t, "valkey-cli", Op{Statement: "GET k", MaxBytes: 1 << 20}, "valkey_values.out", "flood")
	res, err = Run(context.Background(), inv)
	if err != nil || !res.Partial || res.Reason != "byte-limit" || res.BytesRead != 1<<20 {
		t.Fatalf("unbounded producer must be stopped at the bound: %v %+v", err, res)
	}
	if time.Since(start) > 20*time.Second {
		t.Error("flooding child was not killed promptly")
	}
	res, err = Run(context.Background(), helperInv(t, "psql", Op{Statement: "select 1", MaxBytes: 40}, "psql_select.out", ""))
	if err != nil || !res.Partial || res.Reason != "byte-limit" {
		t.Fatalf("psql byte limit: %v %+v", err, res)
	}
}

func TestCancellationAndTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	_, err := Run(ctx, helperInv(t, "psql", Op{Statement: "update t set a=1", Write: true}, "psql_select.out", "sleep"))
	if err == nil || err.Code != gws.CodeCancelled || err.Exit() != 130 || !strings.Contains(err.Message, "nothing committed") {
		t.Fatalf("rolled-back write cancel: %v", err)
	}
	_, err = Run(context.Background(), helperInv(t, "snow", Op{Statement: "update t set a=1", Write: true, Commit: true, Timeout: time.Second}, "snow_rows.out", "sleep"))
	if err == nil || err.Code != gws.CodeTimeout && err.Code != gws.CodePartialMutation {
		t.Fatalf("committing write timeout: %v", err)
	}
	if !strings.Contains(err.Message, "UNKNOWN") {
		t.Errorf("a committing write that is interrupted must report unknown outcome: %v", err)
	}
	_, err = Run(context.Background(), helperInv(t, "psql", Op{Statement: "select pg_sleep(99)", Timeout: time.Second}, "psql_select.out", "sleep"))
	if err == nil || err.Code != gws.CodeTimeout || err.Exit() != 124 || !err.Retryable {
		t.Fatalf("read timeout: %v", err)
	}
}

func TestRedactionCanary(t *testing.T) {
	inv := helperInv(t, "psql", Op{Statement: "select 1"}, "psql_auth_fail.err", "fail")
	_, err := Run(context.Background(), inv)
	if err == nil || err.Code != gws.CodeMissingCredentials {
		t.Fatalf("auth failure must classify: %v", err)
	}
	var buf bytes.Buffer
	err.EmitError(&buf, "dataclients query")
	for name, s := range map[string]string{"message": err.Message, "envelope": buf.String(), "explain": fmt.Sprint(inv.Explain(endpoint("psql"))), "scope": endpoint("psql").RedactedScope()} {
		if strings.Contains(s, canary) {
			t.Errorf("canary leaked into %s: %s", name, s)
		}
	}
	if !strings.Contains(err.Message, gws.Redacted) {
		t.Errorf("provider error body should carry the redaction marker: %s", err.Message)
	}
	// statement-borne secrets are masked in the preview too
	pre, _ := Build(endpoint("psql"), Op{Statement: "alter role x password '" + canary + "'", Write: true}, "psql")
	if strings.Contains(fmt.Sprint(pre.Explain(endpoint("psql"))), canary) {
		t.Error("explain leaked a declared secret from the statement")
	}
}

func TestExecuteParityAndExplain(t *testing.T) {
	env := map[string]string{"PSQL_HOST": "db.example.test", "PSQL_USER": "app", "PSQL_PASSWORD": canary}
	getenv := func(k string) string { return env[k] }
	if err := Schema().Validate(); err != nil {
		t.Fatalf("schema invalid: %v", err)
	}
	cli := Values{"client": "psql", "statement": "update t set a=1", "write": "true", "explain": "true", "max-rows": "5"}
	mcp := Values{"explain": "true", "max-rows": "5", "write": "true", "statement": "update t set a=1", "client": "psql"}
	var a, b bytes.Buffer
	ea := Execute(context.Background(), &a, getenv, nil, cli)
	eb := Execute(context.Background(), &b, getenv, nil, mcp)
	if ea != 0 || ea != eb || a.String() != b.String() {
		t.Fatalf("CLI and MCP must yield identical results:\n%s\n%s", a.String(), b.String())
	}
	if !strings.Contains(a.String(), `"schema_version"`) || strings.Contains(a.String(), canary) || !strings.Contains(a.String(), "rolled back") {
		t.Errorf("explain envelope: %s", a.String())
	}
	for _, bad := range []Values{{"client": "psql", "statement": "x", "bogus": "1"}, {"client": "psql", "statement": "x", "max-rows": "-1"}, {"client": "mysql", "statement": "x"}} {
		var x, y bytes.Buffer
		cx := Execute(context.Background(), &x, getenv, nil, bad)
		cy := Execute(context.Background(), &y, getenv, nil, bad)
		if cx == 0 || cx != cy || x.String() != y.String() || !strings.Contains(x.String(), `"schema_version"`) {
			t.Errorf("rejection must be typed and identical: %v %d", bad, cx)
		}
	}
	var m bytes.Buffer
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	if c := Execute(context.Background(), &m, getenv, missing, Values{"client": "psql", "statement": "select 1"}); c != 2 || !strings.Contains(m.String(), "no sha256-pinned managed release") {
		t.Errorf("absent host client must be an actionable refusal (exit 2): %d %s", c, m.String())
	}
	var nores bytes.Buffer
	if c := Execute(context.Background(), &nores, func(string) string { return "" }, nil, Values{"client": "psql", "statement": "select 1"}); c == 0 {
		t.Error("no explicit endpoint must fail")
	}
}

func TestEmitEnvelope(t *testing.T) {
	res, err := Parse("snow", strings.NewReader(`[{"A":1}]`), Op{}, "")
	if err != nil {
		t.Fatal(err)
	}
	res.Mutation, res.Scope = "none", "x"
	var buf bytes.Buffer
	if Emit(&buf, "dataclients query", res) != 0 || !strings.Contains(buf.String(), `"schema_version"`) {
		t.Errorf("envelope: %s", buf.String())
	}
}

func TestParseRejectsGarbageJSON(t *testing.T) {
	if _, err := Parse("mongosh", strings.NewReader("not json"), Op{}, ""); err == nil || err.Code != gws.CodeProviderFailure {
		t.Errorf("non-JSON output must be refused, not guessed: %v", err)
	}
}
