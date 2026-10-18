package dbdoc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

type fixture struct {
	t      *testing.T
	svc    dblifecycle.Service
	mem    *dbstore.MemSpace
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
		f.svc, f.mem = dblifecycle.Service{Mem: mem}, mem
		f.target = dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "d"}
	} else {
		f.target = dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "d.db")}
	}
	if _, err := f.svc.Create(context.Background(), f.target, time.Second); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) runCtx(ctx context.Context, op string, in Input, yes bool) (dblifecycle.Outcome, error) {
	b, err := json.Marshal(in)
	if err != nil {
		f.t.Fatal(err)
	}
	_, out, err := f.svc.RunCategory(ctx, f.target, op, b, yes, time.Second)
	return out, err
}

func (f *fixture) run(op string, in Input, yes bool) (dblifecycle.Outcome, error) {
	return f.runCtx(context.Background(), op, in, yes)
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

func (f *fixture) get(collection, id string) Document {
	f.t.Helper()
	return *f.must(OpGet, Input{Collection: collection, ID: id}, false).Result.(*Document)
}

// ids returns the ids a find returned, in order.
func (f *fixture) ids(in Input) string {
	f.t.Helper()
	out := f.must(OpFind, in, false)
	var ids []string
	for _, d := range out.Result.(map[string]any)["documents"].([]Document) {
		ids = append(ids, d.ID)
	}
	return strings.Join(ids, ",")
}

func modes(t *testing.T, fn func(t *testing.T, f *fixture)) {
	for _, mode := range []dbstore.Mode{dbstore.Persistent, dbstore.Memory} {
		t.Run(string(mode), func(t *testing.T) { fn(t, newFixture(t, mode)) })
	}
}

func rev(n int64) *int64 { return &n }

const nested = `{"big":12345678901234567890,"dec":1.50,"exp":1.0e3,"neg":-0,"s":"<a&b> \"q\" é","t":true,"f":false,"n":null,"arr":[1,"1",[{"deep":{"x":[]}}],{}],"obj":{"k":{"v":9007199254740993}}}`

func TestDocCRUDBothModes(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		// Nothing is pre-created: a document op on a missing collection fails.
		if c := f.code(OpPut, Input{Collection: "users", Doc: json.RawMessage(`{}`)}, false); c != "not_found" {
			t.Fatalf("put before create: %s", c)
		}
		if b, _ := json.Marshal(f.must(OpCollectionList, Input{}, false).Result); string(b) != `{"collections":[]}` {
			t.Fatalf("fresh database lists collections: %s", b)
		}
		if c := f.code(OpCollectionCreate, Input{Collection: "users"}, false); c != "confirmation_required" {
			t.Fatalf("unconfirmed create: %s", c)
		}
		f.must(OpCollectionCreate, Input{Collection: "users"}, true)
		if c := f.code(OpCollectionCreate, Input{Collection: "users"}, true); c != "exists" {
			t.Fatalf("second create: %s", c)
		}
		for _, bad := range []string{"Users", "a__b", "1x", "x-y", `x"`, ""} {
			if c := f.code(OpCollectionCreate, Input{Collection: bad}, true); c != "invalid_input" {
				t.Fatalf("collection name %q: %s", bad, c)
			}
		}
		// put with an explicit id keeps nested types and number text exactly.
		res := f.must(OpPut, Input{Collection: "users", ID: "u1", Doc: json.RawMessage(nested)}, false).Result.(map[string]any)
		if res["revision"] != int64(1) || res["created"] != true {
			t.Fatalf("put: %v", res)
		}
		d := f.get("users", "u1")
		want := `{"arr":[1,"1",[{"deep":{"x":[]}}],{}],"big":12345678901234567890,"dec":1.50,"exp":1.0e3,"f":false,"n":null,"neg":-0,"obj":{"k":{"v":9007199254740993}},"s":"<a&b> \"q\" é","t":true}`
		if string(d.Doc) != want || d.Revision != 1 {
			t.Fatalf("stored doc\n got %s\nwant %s", d.Doc, want)
		}
		// A generated id is stable: it is what later reads use.
		gen := f.must(OpPut, Input{Collection: "users", Doc: json.RawMessage(`{"name":"gen"}`)}, false).Result.(map[string]any)["id"].(string)
		if len(gen) != 32 || string(f.get("users", gen).Doc) != `{"name":"gen"}` {
			t.Fatalf("generated id %q", gen)
		}
		for _, bad := range []string{`[1]`, `"s"`, `null`, `{"a":1,"a":2}`, `{"a":{"b":1,"b":1}}`} {
			if c := f.code(OpPut, Input{Collection: "users", ID: "x", Doc: json.RawMessage(bad)}, false); c != "invalid_input" {
				t.Fatalf("doc %s: %s", bad, c)
			}
		}
		if c := f.code(OpPut, Input{Collection: "users", ID: "u1", IfAbsent: true, Doc: json.RawMessage(`{}`)}, false); c != "exists" {
			t.Fatalf("if-absent on existing: %s", c)
		}
		if c := f.code(OpPut, Input{Collection: "users", ID: "u1", IfRevision: rev(7), Doc: json.RawMessage(`{}`)}, false); c != "conflict" {
			t.Fatalf("stale if-revision: %s", c)
		}
		if c := f.code(OpPut, Input{Collection: "users", ID: "nope", IfRevision: rev(1), Doc: json.RawMessage(`{}`)}, false); c != "conflict" {
			t.Fatalf("if-revision on absent: %s", c)
		}
		// patch is an RFC 7386 merge patch; null removes, nested merges.
		res = f.must(OpPatch, Input{Collection: "users", ID: "u1", IfRevision: rev(1), Patch: json.RawMessage(`{"n":null,"obj":{"k":{"w":2.50}},"t":"now text"}`)}, false).Result.(map[string]any)
		if res["revision"] != int64(2) {
			t.Fatalf("patch: %v", res)
		}
		if got := string(f.get("users", "u1").Doc); !strings.Contains(got, `"obj":{"k":{"v":9007199254740993,"w":2.50}}`) || strings.Contains(got, `"n":`) || !strings.Contains(got, `"t":"now text"`) || !strings.Contains(got, `"big":12345678901234567890`) {
			t.Fatalf("patched doc %s", got)
		}
		if c := f.code(OpPatch, Input{Collection: "users", ID: "u1", IfRevision: rev(1), Patch: json.RawMessage(`{"x":1}`)}, false); c != "conflict" {
			t.Fatalf("lost update not detected: %s", c)
		}
		if c := f.code(OpPatch, Input{Collection: "users", ID: "zz", Patch: json.RawMessage(`{"x":1}`)}, false); c != "not_found" {
			t.Fatalf("patch missing: %s", c)
		}
		if c := f.code(OpRm, Input{Collection: "users", ID: "u1", IfRevision: rev(1)}, false); c != "conflict" {
			t.Fatalf("rm stale: %s", c)
		}
		f.must(OpRm, Input{Collection: "users", ID: "u1", IfRevision: rev(2)}, false)
		if c := f.code(OpGet, Input{Collection: "users", ID: "u1"}, false); c != "not_found" {
			t.Fatalf("get after rm: %s", c)
		}
		// Unknown input fields for an op are refused, never ignored.
		if c := f.code(OpGet, Input{Collection: "users", ID: gen, Filter: json.RawMessage(`{"field":"a","op":"eq","value":1}`)}, false); c != "invalid_input" {
			t.Fatalf("get with filter: %s", c)
		}
		if c := f.code(OpCollectionDrop, Input{Collection: "users"}, false); c != "confirmation_required" {
			t.Fatalf("unconfirmed drop: %s", c)
		}
		out := f.must(OpCollectionDrop, Input{Collection: "users"}, true)
		if out.Result.(map[string]any)["documents"] != int64(1) {
			t.Fatalf("drop: %v", out.Result)
		}
		if c := f.code(OpGet, Input{Collection: "users", ID: gen}, false); c != "not_found" {
			t.Fatalf("get after drop: %s", c)
		}
	})
}

func TestFilterIsTypedAndParameterized(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpCollectionCreate, Input{Collection: "c"}, true)
		docs := map[string]string{
			"a": `{"n":1,"s":"x","b":true,"tags":["p","q"],"o":{"v":10}}`,
			"b": `{"n":"1","s":"y","b":false,"o":{"v":20}}`,
			"c": `{"n":2.5,"s":"x' or 1=1 --","z":null,"o":{"v":30}}`,
			"d": `{"n":3,"o":{"v":40}}`,
		}
		for id, d := range docs {
			f.must(OpPut, Input{Collection: "c", ID: id, Doc: json.RawMessage(d)}, false)
		}
		for _, tc := range []struct{ filter, params, want string }{
			{`{"field":"n","op":"eq","value":1}`, ``, "a"},
			{`{"field":"n","op":"eq","value":"1"}`, ``, "b"},
			{`{"field":"n","op":"gte","value":2}`, ``, "c,d"},
			{`{"field":"n","op":"ne","value":1}`, ``, "b,c,d"},
			{`{"field":"b","op":"eq","value":true}`, ``, "a"},
			{`{"field":"z","op":"eq","value":null}`, ``, "c"},
			{`{"field":"z","op":"exists","value":true}`, ``, "c"},
			{`{"field":"b","op":"exists","value":false}`, ``, "c,d"},
			{`{"field":"tags[1]","op":"eq","value":"q"}`, ``, "a"},
			{`{"field":"o.v","op":"in","value":[20,40,"30"]}`, ``, "b,d"},
			{`{"and":[{"field":"o.v","op":"gt","param":"lo"},{"not":{"field":"s","op":"eq","param":"s"}}]}`, `{"lo":15,"s":"y"}`, "c,d"},
			{`{"or":[{"field":"s","op":"eq","value":"x"},{"field":"n","op":"eq","value":3}]}`, ``, "a,d"},
			// A value that looks like SQL is only ever a bound value.
			{`{"field":"s","op":"eq","param":"s"}`, `{"s":"x' or 1=1 --"}`, "c"},
			{``, ``, "a,b,c,d"},
		} {
			if got := f.ids(Input{Collection: "c", Filter: json.RawMessage(tc.filter), Params: json.RawMessage(tc.params)}); got != tc.want {
				t.Fatalf("filter %s %s: got %q want %q", tc.filter, tc.params, got, tc.want)
			}
		}
		for _, tc := range []struct{ filter, params string }{
			{`{"field":"s') or 1=1 --","op":"eq","value":1}`, ``},
			{`{"field":"a..b","op":"eq","value":1}`, ``},
			{`{"field":"a.[0]","op":"eq","value":1}`, ``},
			{`{"field":"n","op":"like","value":1}`, ``},
			{`{"field":"n","op":"lt","value":true}`, ``},
			{`{"field":"n","op":"eq","value":{"a":1}}`, ``},
			{`{"field":"n","op":"eq","value":99999999999999999999}`, ``},
			{`{"field":"n","op":"eq","param":"missing"}`, ``},
			{`{"field":"n","op":"eq","value":1}`, `{"unused":1}`},
			{`{"field":"n","op":"eq","value":1,"and":[]}`, ``},
			{`{"field":"n","op":"eq","value":1,"extra":1}`, ``},
		} {
			if c := f.code(OpFind, Input{Collection: "c", Filter: json.RawMessage(tc.filter), Params: json.RawMessage(tc.params)}, false); c != "invalid_input" {
				t.Fatalf("filter %s: %s", tc.filter, c)
			}
		}
	})
}

func TestFindPaginationIsBounded(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpCollectionCreate, Input{Collection: "c"}, true)
		for i := 0; i < 7; i++ {
			f.must(OpPut, Input{Collection: "c", ID: fmt.Sprintf("id%02d", i), Doc: json.RawMessage(fmt.Sprintf(`{"i":%d,"pad":"%s"}`, i, strings.Repeat("x", 100)))}, false)
		}
		filter := json.RawMessage(`{"field":"i","op":"gte","value":1}`)
		var all []string
		since := ""
		for pages := 0; ; pages++ {
			if pages > 5 {
				t.Fatal("pagination does not end")
			}
			out := f.must(OpFind, Input{Collection: "c", Filter: filter, Limit: 2, Since: since}, false)
			for _, d := range out.Result.(map[string]any)["documents"].([]Document) {
				all = append(all, d.ID)
			}
			p := out.Page.(page)
			if !p.Truncated {
				break
			}
			b, _ := json.Marshal(p.Cursor)
			since = string(b)
		}
		if strings.Join(all, ",") != "id01,id02,id03,id04,id05,id06" {
			t.Fatalf("pages: %v", all)
		}
		// A cursor is bound to its query and target.
		out := f.must(OpFind, Input{Collection: "c", Filter: filter, Limit: 2}, false)
		b, _ := json.Marshal(out.Page.(page).Cursor)
		if c := f.code(OpFind, Input{Collection: "c", Limit: 2, Since: string(b)}, false); c != "invalid_input" {
			t.Fatalf("cursor on another query: %s", c)
		}
		// max_bytes truncates with a cursor, and one oversized document is
		// limit_exceeded rather than a silent cut.
		out = f.must(OpFind, Input{Collection: "c", MaxBytes: 350}, false)
		if p := out.Page.(page); !p.Truncated || p.Returned != 2 || p.Cursor == nil {
			t.Fatalf("max-bytes page: %+v", p)
		}
		if c := f.code(OpFind, Input{Collection: "c", MaxBytes: 50}, false); c != "limit_exceeded" {
			t.Fatalf("oversized: %s", c)
		}
		if c := f.code(OpFind, Input{Collection: "c", Limit: MaxLimit + 1}, false); c != "invalid_input" {
			t.Fatalf("limit over max: %s", c)
		}
	})
}

func TestIndexesAreExclusiveEnforcedAndUsed(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpCollectionCreate, Input{Collection: "c"}, true)
		f.must(OpPut, Input{Collection: "c", ID: "1", Doc: json.RawMessage(`{"email":"a@x","age":5}`)}, false)
		f.must(OpPut, Input{Collection: "c", ID: "2", Doc: json.RawMessage(`{"email":"a@x","age":6}`)}, false)
		if c := f.code(OpIndexAdd, Input{Collection: "c", Index: "by_email", Fields: []string{"email"}, Unique: true}, false); c != "confirmation_required" {
			t.Fatalf("unconfirmed index add: %s", c)
		}
		if c := f.code(OpIndexAdd, Input{Collection: "c", Index: "by_email", Fields: []string{"email"}, Unique: true}, true); c != "conflict" {
			t.Fatalf("unique over duplicates: %s", c)
		}
		if out := f.must(OpIndexList, Input{Collection: "c"}, false); len(out.Result.(map[string]any)["indexes"].([]IndexDef)) != 0 {
			t.Fatalf("failed index left metadata: %v", out.Result)
		}
		f.must(OpRm, Input{Collection: "c", ID: "2"}, false)
		f.must(OpIndexAdd, Input{Collection: "c", Index: "by_email", Fields: []string{"email"}, Unique: true}, true)
		f.must(OpIndexAdd, Input{Collection: "c", Index: "by_age", Fields: []string{"age", "email"}}, true)
		if c := f.code(OpIndexAdd, Input{Collection: "c", Index: "bad", Fields: []string{"a'b"}}, true); c != "invalid_input" {
			t.Fatalf("bad index field: %s", c)
		}
		if c := f.code(OpPut, Input{Collection: "c", ID: "3", Doc: json.RawMessage(`{"email":"a@x"}`)}, false); c != "conflict" {
			t.Fatalf("unique put: %s", c)
		}
		// The query planner uses the expression index for a field filter.
		h, err := f.svc.Open(context.Background(), f.target, true, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		where, args, err := compileFilter(json.RawMessage(`{"field":"email","op":"eq","value":"a@x"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := h.(*sqlitedb.DB).SQL().Query("explain query plan select id from "+table("c")+" where "+where, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
		}
		rows.Close()
		h.Close()
		if !strings.Contains(plan.String(), "bashy_doc_i_c__by_email") {
			t.Fatalf("plan does not use the index: %s", plan.String())
		}
		f.must(OpIndexRm, Input{Collection: "c", Index: "by_email"}, true)
		if c := f.code(OpIndexRm, Input{Collection: "c", Index: "by_email"}, true); c != "not_found" {
			t.Fatalf("rm missing index: %s", c)
		}
		f.must(OpPut, Input{Collection: "c", ID: "3", Doc: json.RawMessage(`{"email":"a@x"}`)}, false)
	})
}

func TestBatchIsAtomic(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpCollectionCreate, Input{Collection: "c"}, true)
		f.must(OpPut, Input{Collection: "c", ID: "k", Doc: json.RawMessage(`{"v":1}`)}, false)
		ops := []BatchOp{
			{Op: "put", ID: "a", Doc: json.RawMessage(`{"v":1}`)},
			{Op: "patch", ID: "k", Patch: json.RawMessage(`{"v":2}`), IfRevision: rev(1)},
			{Op: "put", ID: "k", Doc: json.RawMessage(`{}`), IfAbsent: true},
		}
		if c := f.code(OpBatch, Input{Collection: "c", Ops: ops}, false); c != "exists" {
			t.Fatalf("failing batch: %s", c)
		}
		if got := f.ids(Input{Collection: "c"}); got != "k" || string(f.get("c", "k").Doc) != `{"v":1}` {
			t.Fatalf("batch half-applied: %s %s", got, f.get("c", "k").Doc)
		}
		ops[2] = BatchOp{Op: "rm", ID: "a"}
		out := f.must(OpBatch, Input{Collection: "c", Ops: ops}, false)
		if res := out.Result.(map[string]any)["results"].([]map[string]any); len(res) != 3 || res[1]["revision"] != int64(2) {
			t.Fatalf("batch results: %v", out.Result)
		}
		if c := f.code(OpBatch, Input{Collection: "c", Ops: []BatchOp{{Op: "upsert", ID: "x"}}}, false); c != "invalid_input" {
			t.Fatalf("bad batch op: %s", c)
		}
		if c := f.code(OpBatch, Input{Collection: "c", Ops: make([]BatchOp, MaxBatch+1)}, false); c != "invalid_input" {
			t.Fatalf("oversized batch: %s", c)
		}
	})
}

func TestPersistentSurvivesReopenMemoryEndsWithOwner(t *testing.T) {
	p := newFixture(t, dbstore.Persistent)
	p.must(OpCollectionCreate, Input{Collection: "c"}, true)
	p.must(OpPut, Input{Collection: "c", ID: "x", Doc: json.RawMessage(`{"v":1.50}`)}, false)
	reopened := &fixture{t: t, target: p.target}
	if string(reopened.get("c", "x").Doc) != `{"v":1.50}` {
		t.Fatal("persistent document lost on reopen")
	}
	m := newFixture(t, dbstore.Memory)
	m.must(OpCollectionCreate, Input{Collection: "c"}, true)
	m.must(OpPut, Input{Collection: "c", ID: "x", Doc: json.RawMessage(`{"v":1}`)}, false)
	m.mem.Close()
	if c := m.code(OpGet, Input{Collection: "c", ID: "x"}, false); c == "" {
		t.Fatal("memory document outlived its owner")
	}
}

// Writers using if-revision never lose an update: every contested write is
// a reported conflict, retried by the caller, never a silent overwrite.
func TestConcurrentRevisionUpdatesLoseNothing(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpCollectionCreate, Input{Collection: "c"}, true)
		f.must(OpPut, Input{Collection: "c", ID: "ctr", Doc: json.RawMessage(`{"n":0}`)}, false)
		const workers, each = 4, 10
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for done := 0; done < each; {
					out, err := f.run(OpGet, Input{Collection: "c", ID: "ctr"}, false)
					if err != nil {
						if dbstore.Code(err) == "busy" {
							continue
						}
						errs <- err
						return
					}
					d := out.Result.(*Document)
					var v struct{ N int }
					_ = json.Unmarshal(d.Doc, &v)
					_, err = f.run(OpPut, Input{Collection: "c", ID: "ctr", IfRevision: rev(d.Revision), Doc: json.RawMessage(fmt.Sprintf(`{"n":%d}`, v.N+1))}, false)
					switch dbstore.Code(err) {
					case "":
						done++
					case "conflict", "busy":
					default:
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		d := f.get("c", "ctr")
		if string(d.Doc) != fmt.Sprintf(`{"n":%d}`, workers*each) || d.Revision != workers*each+1 {
			t.Fatalf("lost updates: %s rev %d", d.Doc, d.Revision)
		}
	})
}

// An exclusive doc operation is refused while another client holds the
// database's native exclusive lock; ordinary reads are not serialized behind
// an exclusive lease.
func TestExclusiveOpsRespectNativeLock(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.must(OpCollectionCreate, Input{Collection: "c"}, true)
	h, err := (sqlitedb.Adapter{}).Open(context.Background(), f.target, dbstore.Options{Exclusive: true, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Input{Collection: "c", Index: "i", Fields: []string{"a"}})
	_, _, err = f.svc.RunCategory(context.Background(), f.target, OpIndexAdd, b, true, 200*time.Millisecond)
	if c := dbstore.Code(err); c != "busy" {
		t.Fatalf("exclusive op under another lock: %v", err)
	}
	// The holder's own handle runs doc operations directly.
	op, a, _, _ := dblifecycle.LookupCategoryOp(OpPut, json.RawMessage(`{"collection":"c","id":"h","doc":{"held":true}}`))
	if _, err := op.Run(context.Background(), h, json.RawMessage(`{"collection":"c","id":"h","doc":{"held":true}}`), a); err != nil {
		t.Fatalf("held write: %v", err)
	}
	h.Close()
	if string(f.get("c", "h").Doc) != `{"held":true}` {
		t.Fatal("held write lost")
	}
}

func TestReadHandleRefusesWrites(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.must(OpCollectionCreate, Input{Collection: "c"}, true)
	h, err := f.svc.Open(context.Background(), f.target, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	op, _, _, _ := dblifecycle.LookupCategoryOp(OpPut, nil)
	_, err = op.Run(context.Background(), h, json.RawMessage(`{"collection":"c","id":"x","doc":{}}`), dblifecycle.AccessWrite)
	if c := dbstore.Code(err); c != "read_only" && c != "busy" {
		t.Fatalf("write on read handle: %v", err)
	}
	var n int
	if err := h.(*sqlitedb.DB).SQL().QueryRow("select count(*) from " + table("c")).Scan(&n); err != nil || n != 0 {
		t.Fatalf("read handle wrote: %d %v", n, err)
	}
}
