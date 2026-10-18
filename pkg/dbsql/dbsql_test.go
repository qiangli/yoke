package dbsql

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

type fixture struct {
	t      *testing.T
	svc    dblifecycle.Service
	target dbstore.Target
}

// newFixture creates an explicit target in the given mode. Memory targets
// live in an in-process MemSpace, the same space an owner session serves.
func newFixture(t *testing.T, mode dbstore.Mode) *fixture {
	t.Helper()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(t.TempDir(), "coord"))
	f := &fixture{t: t}
	if mode == dbstore.Memory {
		mem, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { mem.Close() })
		f.svc = dblifecycle.Service{Mem: mem}
		f.target = dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "t"}
	} else {
		f.target = dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "t.db")}
	}
	if _, err := f.svc.Create(context.Background(), f.target, time.Second); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) run(op string, in Input, yes bool) (dblifecycle.Outcome, error) {
	return f.runCtx(context.Background(), op, in, yes)
}

func (f *fixture) runCtx(ctx context.Context, op string, in Input, yes bool) (dblifecycle.Outcome, error) {
	b, err := json.Marshal(in)
	if err != nil {
		f.t.Fatal(err)
	}
	_, out, err := f.svc.RunCategory(ctx, f.target, op, b, yes, time.Second)
	return out, err
}

func (f *fixture) must(op string, in Input, yes bool) dblifecycle.Outcome {
	f.t.Helper()
	out, err := f.run(op, in, yes)
	if err != nil {
		f.t.Fatalf("%s %+v: %v", op, in, err)
	}
	return out
}

func (f *fixture) code(op string, in Input, yes bool) string {
	f.t.Helper()
	_, err := f.run(op, in, yes)
	return dbstore.Code(err)
}

// rows returns the query result rows re-marshalled for exact comparison.
func (f *fixture) rows(q string) string {
	f.t.Helper()
	out := f.must(OpQuery, Input{SQL: q}, false)
	b, _ := json.Marshal(out.Result.(map[string]any)["rows"])
	return string(b)
}

func modes(t *testing.T, fn func(t *testing.T, f *fixture)) {
	for _, m := range []dbstore.Mode{dbstore.Persistent, dbstore.Memory} {
		t.Run(string(m), func(t *testing.T) { fn(t, newFixture(t, m)) })
	}
}

func params(t *testing.T, ps ...string) json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	for _, p := range ps {
		_, v, err := ParseParam(p)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, v)
	}
	b, _ := json.Marshal(items)
	return b
}

func TestTypedBindingAndResults(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpSchemaApply, Input{SQL: "create table v(id integer primary key, big integer, f real, s text, b blob, n)"}, true)
		f.must(OpExec, Input{SQL: "insert into v(big, f, s, b, n) values (?, ?, ?, ?, ?)",
			Params: params(t, "int:9007199254740993", "f64:0.1", "text:héllo ' or 1=1 --", "bytes:AAEC/w==", "null")}, false)
		f.must(OpExec, Input{SQL: "insert into v(big, f, b) values (:big, :f, :b)",
			Params: json.RawMessage(`{":big":{"t":"int","v":"-9223372036854775808"},"f":{"t":"f64","v":"-Inf"},"b":{"t":"bytes","b64":""}}`)}, false)
		got := f.rows("select big, f, s, b, n from v order by id")
		want := `[[{"t":"int","v":"9007199254740993"},{"t":"f64","v":0.1},"héllo ' or 1=1 --",{"t":"bytes","b64":"AAEC/w=="},null],` +
			`[{"t":"int","v":"-9223372036854775808"},{"t":"f64","v":"-Inf"},null,{"t":"bytes","b64":""},null]]`
		if got != want {
			t.Fatalf("rows\n got %s\nwant %s", got, want)
		}
		// The quote in the bound text never reached the SQL: one row matches.
		out := f.must(OpQuery, Input{SQL: "select count(*) from v where s = ?", Params: params(t, "text:héllo ' or 1=1 --")}, false)
		if b, _ := json.Marshal(out.Result.(map[string]any)["rows"]); string(b) != `[[{"t":"int","v":"1"}]]` {
			t.Fatalf("bound match %s", b)
		}
		for _, bad := range []string{`[1]`, `[true]`, `[{"t":"int","v":1}]`, `[{"t":"int","v":"9223372036854775808"}]`, `[{"t":"zz"}]`, `[{"t":"int","v":"1","x":1}]`} {
			if c := f.code(OpQuery, Input{SQL: "select ?", Params: json.RawMessage(bad)}, false); c != "invalid_input" {
				t.Errorf("params %s => %s", bad, c)
			}
		}
		if c := f.code(OpQuery, Input{SQL: "select ?", Params: json.RawMessage(`[{"t":"dec","v":"1.0","scale":1}]`)}, false); c != "unsupported" {
			t.Errorf("dec param => %s", c)
		}
		if c := f.code(OpQuery, Input{SQL: "select * from missing"}, false); c != "backend_error" {
			t.Errorf("missing table => %s", c)
		} else if _, err := f.run(OpQuery, Input{SQL: "select * from missing"}, false); !strings.Contains(err.Error(), "no such table: missing") {
			t.Errorf("SQL error not actionable: %v", err)
		}
	})
}

func TestReadPathRefusesMutation(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpSchemaApply, Input{SQL: "create table r(a)"}, true)
		f.must(OpExec, Input{SQL: "insert into r values (1)"}, false)
		for _, q := range []string{
			"insert into r values (2)",
			"pragma query_only=0; insert into r values (2)",
			"begin; delete from r; commit",
			"pragma query_only=0; create temp table tt(x); insert into tt values (1)",
			"pragma user_version=7",
			"pragma query_only=0; pragma user_version=7",
			"create table r2(a)",
			"pragma query_only=0; drop table r",
		} {
			if c := f.code(OpQuery, Input{SQL: q}, false); c != "read_only" {
				t.Errorf("%q => %s, want read_only", q, c)
			}
		}
		vacuum := filepath.Join(t.TempDir(), "copy.db")
		for _, q := range []string{"vacuum into '" + vacuum + "'", "attach '" + vacuum + "' as x", "attach 'file:/x?vfs=memdb' as y"} {
			if c := f.code(OpQuery, Input{SQL: q}, false); c != "unsupported" {
				t.Errorf("%q => %s, want unsupported", q, c)
			}
		}
		if _, err := os.Stat(vacuum); !os.IsNotExist(err) {
			t.Fatalf("read path wrote a file: %v", err)
		}
		if got := f.rows("select a, (select user_version from pragma_user_version) from r"); got != `[[{"t":"int","v":"1"},{"t":"int","v":"0"}]]` {
			t.Fatalf("target changed through the read path: %s", got)
		}
	})
}

func TestHeldHandleReadStillRefusesMutation(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.must(OpSchemaApply, Input{SQL: "create table r(a); insert into r values (1)"}, true)
	// A hold's handle is the adapter's read-write exclusive open.
	h, err := (sqlitedb.Adapter{}).Open(context.Background(), f.target, dbstore.Options{Exclusive: true, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, q := range []string{"pragma query_only=0; insert into r values (2)", "begin; insert into r values (3)", "pragma query_only=0; begin; insert into r values (4); commit"} {
		_, err := runQuery(context.Background(), h, Input{SQL: q})
		if dbstore.Code(err) != "read_only" {
			t.Errorf("held %q => %v", q, err)
		}
	}
	out, err := runQuery(context.Background(), h, Input{SQL: "select count(*) from r"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out.Result.(map[string]any)["rows"]); string(b) != `[[{"t":"int","v":"1"}]]` {
		t.Fatalf("held handle changed: %s", b)
	}
	// The connection is clean for the holder's next write.
	if _, err := runExec(context.Background(), h, Input{SQL: "insert into r values (5)"}, false); err != nil {
		t.Fatalf("holder write after read: %v", err)
	}
}

// A read on a held (read-write) handle leaves nothing behind on its pooled
// connection: no temp objects, no changed session pragma, no released native
// lock, and a memory database keeps temp_store=MEMORY (no spill to files).
func TestHeldReadLeavesNoConnectionState(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpSchemaApply, Input{SQL: "create table r(a); insert into r values (1)"}, true)
		h, err := (sqlitedb.Adapter{}).Open(context.Background(), f.target, dbstore.Options{Exclusive: true, Wait: time.Second, Mem: f.svc.Mem})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		state := func() string {
			t.Helper()
			out, err := runQuery(context.Background(), h, Input{SQL: "select (select count(*) from temp.sqlite_schema), (select temp_store from pragma_temp_store), (select foreign_keys from pragma_foreign_keys), (select writable_schema from pragma_writable_schema)"})
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(out.Result.(map[string]any)["rows"])
			return string(b)
		}
		before := state()
		if f.target.Mode == dbstore.Memory && !strings.Contains(before, `{"t":"int","v":"2"}`) {
			t.Fatalf("memory database temp_store is not MEMORY: %s", before)
		}
		for _, q := range []string{
			"pragma query_only=0; create temp table tt(x); insert into tt values (1)",
			"pragma query_only=0; begin; create temp table tt(x); insert into tt values (1)",
			"pragma temp_store=1; pragma foreign_keys=1; pragma writable_schema=1; pragma query_only=0; insert into r values (2)",
		} {
			if _, err := runQuery(context.Background(), h, Input{SQL: q}); dbstore.Code(err) != "read_only" {
				t.Errorf("held %q => %v", q, err)
			}
			if got := state(); got != before {
				t.Fatalf("held %q left connection state %s (was %s)", q, got, before)
			}
		}
		// Pragma-only reads that succeed are restored as well.
		if _, err := runQuery(context.Background(), h, Input{SQL: "pragma temp_store=1; pragma foreign_keys=1"}); err != nil {
			t.Fatal(err)
		}
		if got := state(); got != before {
			t.Fatalf("pragma read left connection state %s (was %s)", got, before)
		}
		if f.target.Mode == dbstore.Persistent {
			// Dropping locking_mode through a read must not release the hold's native lock.
			if _, err := runQuery(context.Background(), h, Input{SQL: "pragma locking_mode=normal; select count(*) from r"}); err != nil {
				t.Fatal(err)
			}
			other, err := sql.Open("sqlite", "file:"+f.target.Path+"?mode=ro&_pragma=busy_timeout(0)")
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if _, err := other.Exec("select count(*) from r"); err == nil || !strings.Contains(err.Error(), "locked") {
				t.Fatalf("native lock released after a held read: %v", err)
			}
		}
		if _, err := runExec(context.Background(), h, Input{SQL: "insert into r values (3)"}, false); err != nil {
			t.Fatalf("holder write after reads: %v", err)
		}
		out, err := runQuery(context.Background(), h, Input{SQL: "select group_concat(a) from r"})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := json.Marshal(out.Result.(map[string]any)["rows"]); string(b) != `[["1,3"]]` {
			t.Fatalf("held rows: %s", b)
		}
	})
}

func TestExecIsAtomic(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpSchemaApply, Input{SQL: "create table a(id integer primary key, v text not null)"}, true)
		f.must(OpExec, Input{SQL: "insert into a values (1, 'one')"}, false)
		// A failing second statement rolls back the first.
		_, err := f.run(OpExec, Input{Statements: []Statement{
			{SQL: "insert into a values (2, 'two')"},
			{SQL: "insert into a values (?, 'dup')", Params: params(t, "int:1")},
		}}, false)
		if dbstore.Code(err) != "conflict" || !strings.Contains(err.Error(), "statement 2") || !strings.Contains(err.Error(), "rolled back") {
			t.Fatalf("batch conflict = %v", err)
		}
		// Script transaction control cannot split the operation.
		for _, q := range []string{
			"insert into a values (3, 'x'); commit; insert into a values (4, 'y')",
			"insert into a values (3, 'x'); rollback; insert into a values (4, 'y')",
			"insert into a values (3, 'x'); rollback",
			"begin; insert into a values (3, 'x')",
			"insert into a values (3, 'x'); create table b(x)",
			"insert into a values (3, 'x'); insert into a values (4, null)",
			// A rolled-back head plus a reopened transaction must not commit the tail alone.
			"insert into a values (3, 'x'); rollback; begin immediate; insert into a values (4, 'y')",
			"insert or rollback into a values (1, 'dup'); begin immediate; insert into a values (4, 'y')",
		} {
			if _, err := f.run(OpExec, Input{SQL: q}, false); err == nil {
				t.Errorf("%q applied", q)
			}
		}
		_, err = f.run(OpExec, Input{Statements: []Statement{
			{SQL: "insert into a values (3, 'x')"},
			{SQL: "rollback; begin immediate"},
			{SQL: "insert into a values (4, 'y')"},
		}}, false)
		if dbstore.Code(err) != "invalid_input" || !strings.Contains(err.Error(), "rolled back") {
			t.Fatalf("rollback/begin batch = %v", err)
		}
		// The caller-writable schema cookie cannot hide a schema change from exec.
		f.must(OpSchemaApply, Input{SQL: "create table scratch(x)"}, true)
		var cv [][]map[string]string
		if err := json.Unmarshal([]byte(f.rows("pragma schema_version")), &cv); err != nil {
			t.Fatal(err)
		}
		cookie := cv[0][0]["v"]
		for _, q := range []string{
			"create table b(x); pragma schema_version=" + cookie,
			"drop table scratch; pragma schema_version=" + cookie,
			"pragma writable_schema=1; update sqlite_schema set sql = replace(sql, '(x)', '(x, y)') where name = 'scratch'; pragma schema_version=" + cookie,
		} {
			if _, err := f.run(OpExec, Input{SQL: q}, false); dbstore.Code(err) != "invalid_input" || !strings.Contains(err.Error(), "schema apply") {
				t.Errorf("%q => %v", q, err)
			}
		}
		if got := f.rows("select group_concat(name || ':' || sql, ' | ') from sqlite_schema where name in ('b', 'scratch')"); got != `[["scratch:CREATE TABLE scratch(x)"]]` {
			t.Fatalf("schema changed through exec: %s", got)
		}
		if _, err := f.run(OpExec, Input{SQL: "create temp table tt(x); insert into tt values (1)"}, false); dbstore.Code(err) != "invalid_input" {
			t.Fatalf("temp object through exec = %v", err)
		}
		if got := f.rows("select id from a order by id"); got != `[[{"t":"int","v":"1"}]]` {
			t.Fatalf("partial apply: %s", got)
		}
		// Cancellation mid-statement rolls back too.
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err = f.runCtx(ctx, OpExec, Input{SQL: "insert into a(v) select 'z' from (with recursive c(x) as (select 1 union all select x+1 from c) select x from c)"}, false)
		if c := dbstore.Code(err); c != "timeout" && c != "cancelled" {
			t.Fatalf("cancel = %v", err)
		}
		if got := f.rows("select count(*) from a"); got != `[[{"t":"int","v":"1"}]]` {
			t.Fatalf("cancelled exec applied: %s", got)
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel2()
		_, err = f.runCtx(ctx2, OpQuery, Input{SQL: "with recursive c(x) as (select 1 union all select x+1 from c) select count(*) from c"}, false)
		if c := dbstore.Code(err); c != "timeout" && c != "cancelled" {
			t.Fatalf("query cancel = %v", err)
		}
	})
}

func TestSchemaShowApplyAndBounds(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		if c := f.code(OpSchemaApply, Input{SQL: "create table x(a)"}, false); c != "confirmation_required" {
			t.Fatalf("unconfirmed apply = %s", c)
		}
		f.must(OpSchemaApply, Input{SQL: `create table p(id integer primary key, name text unique not null check (length(name) > 0));
			create table c(id integer primary key, p integer references p(id) on delete cascade, g integer generated always as (id * 2));
			create index c_p on c(p); create view pv as select name from p;`}, true)
		if c := f.code(OpSchemaApply, Input{SQL: "create table y(a); create table p(a)"}, true); c != "backend_error" {
			t.Fatalf("failing apply = %s", c)
		}
		out := f.must(OpSchemaShow, Input{}, false)
		b, _ := json.Marshal(out.Result)
		for _, want := range []string{`"name":"p"`, `"type":"index","name":"c_p"`, `"type":"view","name":"pv"`, `"generated":true`, `"on_delete":"CASCADE"`, `"not_null":true`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("schema show lacks %s: %s", want, b)
			}
		}
		if strings.Contains(string(b), `"name":"y"`) {
			t.Fatal("failed apply left table y")
		}
		for i := 0; i < 5; i++ {
			f.must(OpExec, Input{SQL: "insert into p(name) values (?)", Params: params(t, "text:n"+strings.Repeat("x", i))}, false)
		}
		first := f.must(OpQuery, Input{SQL: "select name from p order by id", Limit: 2}, false)
		pg := first.Page.(page)
		if !pg.Truncated || pg.Returned != 2 || pg.Cursor == nil || len(first.Warnings) == 0 {
			t.Fatalf("page 1 = %+v", pg)
		}
		c, _ := json.Marshal(pg.Cursor)
		next := f.must(OpQuery, Input{SQL: "select name from p order by id", Limit: 10, Since: string(c)}, false)
		if b, _ := json.Marshal(next.Result.(map[string]any)["rows"]); string(b) != `[["nxx"],["nxxx"],["nxxxx"]]` || next.Page.(page).Truncated {
			t.Fatalf("page 2 = %s %+v", b, next.Page)
		}
		if c := f.code(OpQuery, Input{SQL: "select 1", Since: string(c)}, false); c != "invalid_input" {
			t.Fatalf("cursor for another query = %s", c)
		}
		small := f.must(OpQuery, Input{SQL: "select name from p order by id", MaxBytes: 12}, false)
		if p := small.Page.(page); !p.Truncated || p.Returned != 2 {
			t.Fatalf("byte bound = %+v", p)
		}
		if c := f.code(OpQuery, Input{SQL: "select zeroblob(100)", MaxBytes: 10}, false); c != "limit_exceeded" {
			t.Fatalf("oversize row = %s", c)
		}
		if c := f.code(OpQuery, Input{SQL: "select 1", Limit: MaxLimit + 1}, false); c != "invalid_input" {
			t.Fatalf("over max limit = %s", c)
		}
	})
}
