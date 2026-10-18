package dbvec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
	"github.com/qiangli/yoke/pkg/dbstore/vecdb"
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
		f.target = dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "v"}
	} else {
		f.target = dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "v.db")}
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

func (f *fixture) get(collection, id string) Item {
	f.t.Helper()
	return *f.must(OpGet, Input{Collection: collection, ID: id}, false).Result.(*Item)
}

func (f *fixture) add(name string, dim int, metric string) {
	f.t.Helper()
	f.must(OpCollectionAdd, Input{Collection: name, Dimension: dim, Metric: metric, Model: "test-model@1"}, true)
}

func (f *fixture) put(collection, id string, v []float32, meta string) {
	f.t.Helper()
	in := Input{Collection: collection, ID: id, Vector: tagged(v)}
	if meta != "" {
		in.Metadata = json.RawMessage(meta)
	}
	f.must(OpPut, in, false)
}

// search returns "id@distance" for each hit, in order.
func (f *fixture) search(in Input) string {
	f.t.Helper()
	out := f.must(OpSearch, in, false)
	var s []string
	for _, h := range out.Result.(map[string]any)["hits"].([]Hit) {
		s = append(s, fmt.Sprintf("%s@%.4f", h.ID, h.Distance))
	}
	return strings.Join(s, ",")
}

func modes(t *testing.T, fn func(t *testing.T, f *fixture)) {
	for _, mode := range []dbstore.Mode{dbstore.Persistent, dbstore.Memory} {
		t.Run(string(mode), func(t *testing.T) { fn(t, newFixture(t, mode)) })
	}
}

func rev(n int64) *int64 { return &n }

func tagged(v []float32) json.RawMessage {
	b, _ := json.Marshal(encodeVector(v))
	return b
}

func decoded(t *testing.T, v *Vector) []float32 {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(v.B64)
	if err != nil {
		t.Fatal(err)
	}
	f, err := vecdb.ParseF32(b)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func sameBits(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Float32bits(a[i]) != math.Float32bits(b[i]) {
			return false
		}
	}
	return true
}

// float32 values a decimal round trip would not keep: the smallest
// subnormal, a value needing all 24 mantissa bits, and negative zero.
var lossy = []float32{math.SmallestNonzeroFloat32, 0.70710677, float32(math.Copysign(0, -1)), 16777215}

func TestVecCollectionsAndItemsBothModes(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		// Nothing is pre-created.
		if c := f.code(OpPut, Input{Collection: "e", Vector: tagged([]float32{1, 0, 0, 0})}, false); c != "not_found" {
			t.Fatalf("put before add: %s", c)
		}
		if b, _ := json.Marshal(f.must(OpCollectionList, Input{}, false).Result); string(b) != `{"collections":[]}` {
			t.Fatalf("fresh database lists collections: %s", b)
		}
		if c := f.code(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "m"}, false); c != "confirmation_required" {
			t.Fatalf("unconfirmed add: %s", c)
		}
		for _, bad := range []Input{
			{Collection: "e", Dimension: 0, Metric: "l2", Model: "m"},
			{Collection: "e", Dimension: MaxDimension + 1, Metric: "l2", Model: "m"},
			{Collection: "e", Dimension: 4, Metric: "dot", Model: "m"},
			{Collection: "e", Dimension: 4, Metric: "l2"},
			{Collection: "e", Dimension: 4, Metric: "l2", Model: " m"},
			{Collection: "E", Dimension: 4, Metric: "l2", Model: "m"},
			{Collection: "a__e", Dimension: 4, Metric: "l2", Model: "m"},
		} {
			if c := f.code(OpCollectionAdd, bad, true); c != "invalid_input" {
				t.Fatalf("collection add %+v: %s", bad, c)
			}
		}
		f.must(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "nomic-embed-text@v1.5"}, true)
		if c := f.code(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "m"}, true); c != "exists" {
			t.Fatalf("second add: %s", c)
		}
		// Supplied vectors, as a tagged lossless vector or a number array.
		res := f.must(OpPut, Input{Collection: "e", ID: "a", Vector: tagged(lossy), Metadata: json.RawMessage(`{"k":"x","big":12345678901234567890}`)}, false).Result.(map[string]any)
		if res["revision"] != int64(1) || res["created"] != true {
			t.Fatalf("put: %v", res)
		}
		it := f.get("e", "a")
		if !sameBits(decoded(t, it.Vector), lossy) || string(it.Metadata) != `{"big":12345678901234567890,"k":"x"}` || it.Revision != 1 {
			t.Fatalf("stored item %+v %v", it, decoded(t, it.Vector))
		}
		f.must(OpPut, Input{Collection: "e", ID: "b", Vector: json.RawMessage(`[1, 0.5, -2, 3e2]`)}, false)
		if got := decoded(t, f.get("e", "b").Vector); !sameBits(got, []float32{1, 0.5, -2, 300}) || string(f.get("e", "b").Metadata) != `{}` {
			t.Fatalf("array vector %v", got)
		}
		gen := f.must(OpPut, Input{Collection: "e", Vector: json.RawMessage(`[0,0,0,1]`), Model: "nomic-embed-text@v1.5"}, false).Result.(map[string]any)["id"].(string)
		if len(gen) != 32 || f.get("e", gen).Revision != 1 {
			t.Fatalf("generated id %q", gen)
		}
		// Incompatible vectors are refused, never coerced.
		nan := []float32{float32(math.NaN()), 0, 0, 0}
		for name, v := range map[string]json.RawMessage{
			"short":    json.RawMessage(`[1,2,3]`),
			"long":     json.RawMessage(`[1,2,3,4,5]`),
			"nan":      tagged(nan),
			"overflow": json.RawMessage(`[1e39,0,0,0]`),
			"string":   json.RawMessage(`["1",0,0,0]`),
			"f64":      json.RawMessage(`{"t":"vec","dtype":"f64","b64":""}`),
			"badb64":   json.RawMessage(`{"t":"vec","dtype":"f32","b64":"!!"}`),
			"oddblob":  json.RawMessage(`{"t":"vec","dtype":"f32","b64":"AAAA"}`),
			"absent":   nil,
		} {
			if c := f.code(OpPut, Input{Collection: "e", ID: "x", Vector: v}, false); c != "invalid_input" {
				t.Fatalf("vector %s: %s", name, c)
			}
		}
		if c := f.code(OpPut, Input{Collection: "e", ID: "x", Vector: json.RawMessage(`[1,0,0,0]`), Model: "other@2"}, false); c != "invalid_input" {
			t.Fatalf("other model: %s", c)
		}
		for _, bad := range []string{`[1]`, `{"a":1,"a":2}`, `"s"`} {
			if c := f.code(OpPut, Input{Collection: "e", ID: "x", Vector: json.RawMessage(`[1,0,0,0]`), Metadata: json.RawMessage(bad)}, false); c != "invalid_input" {
				t.Fatalf("metadata %s: %s", bad, c)
			}
		}
		// Revision preconditions detect lost updates.
		if c := f.code(OpPut, Input{Collection: "e", ID: "a", IfAbsent: true, Vector: json.RawMessage(`[1,0,0,0]`)}, false); c != "exists" {
			t.Fatalf("if-absent on existing: %s", c)
		}
		if c := f.code(OpPut, Input{Collection: "e", ID: "a", IfRevision: rev(7), Vector: json.RawMessage(`[1,0,0,0]`)}, false); c != "conflict" {
			t.Fatalf("stale if-revision: %s", c)
		}
		if c := f.code(OpPut, Input{Collection: "e", ID: "nope", IfRevision: rev(1), Vector: json.RawMessage(`[1,0,0,0]`)}, false); c != "conflict" {
			t.Fatalf("if-revision on absent: %s", c)
		}
		res = f.must(OpPut, Input{Collection: "e", ID: "a", IfRevision: rev(1), Vector: json.RawMessage(`[2,0,0,0]`), Metadata: json.RawMessage(`{"k":"y"}`)}, false).Result.(map[string]any)
		if it := f.get("e", "a"); res["revision"] != int64(2) || it.Revision != 2 || !sameBits(decoded(t, it.Vector), []float32{2, 0, 0, 0}) || string(it.Metadata) != `{"k":"y"}` {
			t.Fatalf("update: %v %+v", res, it)
		}
		if c := f.code(OpRm, Input{Collection: "e", ID: "a", IfRevision: rev(1)}, false); c != "conflict" {
			t.Fatalf("rm stale: %s", c)
		}
		f.must(OpRm, Input{Collection: "e", ID: "a", IfRevision: rev(2)}, false)
		if c := f.code(OpGet, Input{Collection: "e", ID: "a"}, false); c != "not_found" {
			t.Fatalf("get after rm: %s", c)
		}
		if c := f.code(OpRm, Input{Collection: "e", ID: "a"}, false); c != "not_found" {
			t.Fatalf("rm twice: %s", c)
		}
		if got := f.search(Input{Collection: "e", Vector: json.RawMessage(`[2,0,0,0]`)}); strings.Contains(got, "a@") {
			t.Fatalf("deleted vector still searchable: %s", got)
		}
		// Unknown fields for an op are refused, never ignored; an embedding
		// request is not a field at all, so it can never reach a provider.
		if c := f.code(OpGet, Input{Collection: "e", ID: "b", Filter: json.RawMessage(`{"field":"k","op":"eq","value":1}`)}, false); c != "invalid_input" {
			t.Fatalf("get with filter: %s", c)
		}
		if _, err := DecodeInput(json.RawMessage(`{"collection":"e","text":"embed me","provider":"ollama"}`)); dbstore.Code(err) != "invalid_input" {
			t.Fatalf("text input: %v", err)
		}
		list, _ := json.Marshal(f.must(OpCollectionList, Input{}, false).Result)
		if string(list) != `{"collections":[{"name":"e","dimension":4,"metric":"l2","model":"nomic-embed-text@v1.5","dtype":"f32","vectors":2}]}` {
			t.Fatalf("list: %s", list)
		}
		if c := f.code(OpCollectionRm, Input{Collection: "e"}, false); c != "confirmation_required" {
			t.Fatalf("unconfirmed rm: %s", c)
		}
		if out := f.must(OpCollectionRm, Input{Collection: "e"}, true); out.Result.(map[string]any)["vectors"] != int64(2) {
			t.Fatalf("collection rm: %v", out.Result)
		}
		if c := f.code(OpGet, Input{Collection: "e", ID: "b"}, false); c != "not_found" {
			t.Fatalf("get after collection rm: %s", c)
		}
		// The name is reusable with another definition after rm.
		f.add("e", 2, "cosine")
		f.put("e", "b", []float32{1, 0}, "")
	})
}

func TestSearchMetricsFiltersAndPages(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		for _, m := range Metrics {
			f.add(m, 2, m)
			for id, v := range map[string][]float32{"a": {1, 0}, "b": {0, 1}, "c": {3, 4}, "d": {-1, 0}} {
				f.put(m, id, v, fmt.Sprintf(`{"id":%q,"even":%t}`, id, id == "b" || id == "d"))
			}
		}
		q := json.RawMessage(`[1,0]`)
		for m, want := range map[string]string{
			"l2":     "a@0.0000,b@1.4142,d@2.0000,c@4.4721",
			"l1":     "a@0.0000,b@2.0000,d@2.0000,c@6.0000",
			"cosine": "a@0.0000,c@0.4000,b@1.0000,d@2.0000",
		} {
			if got := f.search(Input{Collection: m, Vector: q}); got != want {
				t.Fatalf("%s: got %s want %s", m, got, want)
			}
		}
		// Metadata filters use the db doc grammar with bound parameters.
		if got := f.search(Input{Collection: "l2", Vector: q, Filter: json.RawMessage(`{"field":"even","op":"eq","value":true}`)}); got != "b@1.4142,d@2.0000" {
			t.Fatalf("filter: %s", got)
		}
		if got := f.search(Input{Collection: "l2", Vector: q, Filter: json.RawMessage(`{"field":"id","op":"in","param":"ids"}`), Params: json.RawMessage(`{"ids":["c","x' or 1=1 --"]}`)}); got != "c@4.4721" {
			t.Fatalf("param filter: %s", got)
		}
		if c := f.code(OpSearch, Input{Collection: "l2", Vector: q, Filter: json.RawMessage(`{"field":"id') or 1=1 --","op":"eq","value":1}`)}, false); c != "invalid_input" {
			t.Fatalf("bad filter: %s", c)
		}
		if c := f.code(OpSearch, Input{Collection: "l2", Vector: json.RawMessage(`[1,0,0]`)}, false); c != "invalid_input" {
			t.Fatalf("query dimension: %s", c)
		}
		if c := f.code(OpSearch, Input{Collection: "cosine", Vector: json.RawMessage(`[0,0]`)}, false); c != "invalid_input" {
			t.Fatalf("zero cosine query: %s", c)
		}
		if c := f.code(OpPut, Input{Collection: "cosine", ID: "z", Vector: json.RawMessage(`[0,0]`)}, false); c != "invalid_input" {
			t.Fatalf("zero cosine put: %s", c)
		}
		if c := f.code(OpSearch, Input{Collection: "l2", Vector: q, Model: "other"}, false); c != "invalid_input" {
			t.Fatalf("query from another model: %s", c)
		}
		// Pages resume exactly after the last hit, through equal distances.
		var all []string
		since := ""
		for pages := 0; ; pages++ {
			if pages > 5 {
				t.Fatal("pagination does not end")
			}
			out := f.must(OpSearch, Input{Collection: "l1", Vector: q, Limit: 2, Since: since}, false)
			for _, h := range out.Result.(map[string]any)["hits"].([]Hit) {
				all = append(all, h.ID)
			}
			p := out.Page.(page)
			if !p.Truncated {
				break
			}
			b, _ := json.Marshal(p.Cursor)
			since = string(b)
		}
		if strings.Join(all, ",") != "a,b,d,c" {
			t.Fatalf("pages: %v", all)
		}
		out := f.must(OpSearch, Input{Collection: "l1", Vector: q, Limit: 1}, false)
		b, _ := json.Marshal(out.Page.(page).Cursor)
		if c := f.code(OpSearch, Input{Collection: "l1", Vector: json.RawMessage(`[0,1]`), Limit: 1, Since: string(b)}, false); c != "invalid_input" {
			t.Fatalf("cursor for another query: %s", c)
		}
		// Vectors only when asked, and bounded by max_bytes.
		out = f.must(OpSearch, Input{Collection: "l2", Vector: q, Limit: 1, IncludeVectors: true}, false)
		if h := out.Result.(map[string]any)["hits"].([]Hit)[0]; h.Vector == nil || !sameBits(decoded(t, h.Vector), []float32{1, 0}) {
			t.Fatalf("include vectors: %+v", h)
		}
		if out := f.must(OpSearch, Input{Collection: "l2", Vector: q, Limit: 1}, false); out.Result.(map[string]any)["hits"].([]Hit)[0].Vector != nil {
			t.Fatal("vector returned unasked")
		}
		out = f.must(OpSearch, Input{Collection: "l2", Vector: q, MaxBytes: 150}, false)
		if p := out.Page.(page); !p.Truncated || p.Returned != 2 || p.Cursor == nil {
			t.Fatalf("max-bytes page: %+v", p)
		}
		if c := f.code(OpSearch, Input{Collection: "l2", Vector: q, MaxBytes: 10}, false); c != "limit_exceeded" {
			t.Fatalf("oversized hit: %s", c)
		}
		if c := f.code(OpSearch, Input{Collection: "l2", Vector: q, Limit: MaxLimit + 1}, false); c != "invalid_input" {
			t.Fatalf("limit over max: %s", c)
		}
	})
}

func TestBatchIsAtomic(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.add("e", 2, "l2")
		f.put("e", "k", []float32{1, 1}, `{"v":1}`)
		ops := []BatchOp{
			{Op: "put", ID: "a", Vector: json.RawMessage(`[1,0]`)},
			{Op: "put", ID: "k", Vector: json.RawMessage(`[2,2]`), IfRevision: rev(1)},
			{Op: "put", ID: "k", Vector: json.RawMessage(`[3,3]`), IfAbsent: true},
		}
		if c := f.code(OpBatch, Input{Collection: "e", Ops: ops}, false); c != "exists" {
			t.Fatalf("failing batch: %s", c)
		}
		if got := f.search(Input{Collection: "e", Vector: json.RawMessage(`[1,1]`)}); got != "k@0.0000" || f.get("e", "k").Revision != 1 {
			t.Fatalf("batch half-applied: %s", got)
		}
		ops[2] = BatchOp{Op: "rm", ID: "a"}
		out := f.must(OpBatch, Input{Collection: "e", Ops: ops}, false)
		if res := out.Result.(map[string]any)["results"].([]map[string]any); len(res) != 3 || res[1]["revision"] != int64(2) {
			t.Fatalf("batch results: %v", out.Result)
		}
		if c := f.code(OpBatch, Input{Collection: "e", Ops: []BatchOp{{Op: "patch", ID: "x"}}}, false); c != "invalid_input" {
			t.Fatalf("bad batch op: %s", c)
		}
		if c := f.code(OpBatch, Input{Collection: "e", Ops: make([]BatchOp, MaxBatch+1)}, false); c != "invalid_input" {
			t.Fatalf("oversized batch: %s", c)
		}
	})
}

// A persistent collection — its definition, vectors and deletions —
// survives a restart (a fresh service and handle); a memory one ends with
// its owner.
func TestPersistentSurvivesRestartMemoryEndsWithOwner(t *testing.T) {
	p := newFixture(t, dbstore.Persistent)
	p.must(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "cosine", Model: "m@1"}, true)
	p.put("e", "keep", lossy, `{"k":1.50}`)
	p.put("e", "gone", []float32{1, 1, 1, 1}, "")
	p.must(OpRm, Input{Collection: "e", ID: "gone"}, false)
	restarted := &fixture{t: t, target: p.target}
	it := restarted.get("e", "keep")
	if !sameBits(decoded(t, it.Vector), lossy) || string(it.Metadata) != `{"k":1.50}` {
		t.Fatalf("persistent vector after restart: %+v", it)
	}
	if c := restarted.code(OpGet, Input{Collection: "e", ID: "gone"}, false); c != "not_found" {
		t.Fatalf("deleted vector after restart: %s", c)
	}
	if c := restarted.code(OpPut, Input{Collection: "e", ID: "x", Vector: json.RawMessage(`[1,2,3]`)}, false); c != "invalid_input" {
		t.Fatalf("dimension not persisted: %s", c)
	}
	list, _ := json.Marshal(restarted.must(OpCollectionList, Input{}, false).Result)
	if !strings.Contains(string(list), `"dimension":4,"metric":"cosine","model":"m@1"`) {
		t.Fatalf("definition after restart: %s", list)
	}
	m := newFixture(t, dbstore.Memory)
	m.add("e", 2, "l2")
	m.put("e", "x", []float32{1, 0}, "")
	m.mem.Close()
	if c := m.code(OpGet, Input{Collection: "e", ID: "x"}, false); c == "" {
		t.Fatal("memory vector outlived its owner")
	}
}

// Writers using if-revision never lose an update: every contested write is
// a reported conflict, retried by the caller, never a silent overwrite.
func TestConcurrentRevisionUpdatesLoseNothing(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.add("e", 1, "l2")
		f.put("e", "ctr", []float32{0}, "")
		const workers, each = 4, 8
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for done := 0; done < each; {
					out, err := f.run(OpGet, Input{Collection: "e", ID: "ctr"}, false)
					if err != nil {
						if dbstore.Code(err) == "busy" {
							continue
						}
						errs <- err
						return
					}
					it := out.Result.(*Item)
					n := decoded(t, it.Vector)[0]
					_, err = f.run(OpPut, Input{Collection: "e", ID: "ctr", IfRevision: rev(it.Revision), Vector: json.RawMessage(fmt.Sprintf("[%g]", n+1))}, false)
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
		it := f.get("e", "ctr")
		if v := decoded(t, it.Vector)[0]; v != workers*each || it.Revision != workers*each+1 {
			t.Fatalf("lost updates: %v rev %d", v, it.Revision)
		}
	})
}

// An exclusive vec operation is refused while another client holds the
// database's native exclusive lock; the holder's handle still runs ops.
func TestExclusiveOpsRespectNativeLock(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.add("e", 2, "l2")
	h, err := (sqlitedb.Adapter{}).Open(context.Background(), f.target, dbstore.Options{Exclusive: true, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Input{Collection: "e"})
	if _, _, err := f.svc.RunCategory(context.Background(), f.target, OpCollectionRm, b, true, 200*time.Millisecond); dbstore.Code(err) != "busy" {
		t.Fatalf("exclusive op under another lock: %v", err)
	}
	in := json.RawMessage(`{"collection":"e","id":"h","vector":[1,2]}`)
	op, a, _, _ := dblifecycle.LookupCategoryOp(OpPut, in)
	if _, err := op.Run(context.Background(), h, in, a); err != nil {
		t.Fatalf("held write: %v", err)
	}
	h.Close()
	if f.get("e", "h").Revision != 1 {
		t.Fatal("held write lost")
	}
}

func TestReadHandleRefusesWrites(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.add("e", 2, "l2")
	h, err := f.svc.Open(context.Background(), f.target, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	op, _, _, _ := dblifecycle.LookupCategoryOp(OpPut, nil)
	_, err = op.Run(context.Background(), h, json.RawMessage(`{"collection":"e","id":"x","vector":[1,2]}`), dblifecycle.AccessWrite)
	if c := dbstore.Code(err); c != "read_only" && c != "busy" {
		t.Fatalf("write on read handle: %v", err)
	}
	var n int
	if err := h.(*sqlitedb.DB).SQL().QueryRow("select count(*) from " + items("e")).Scan(&n); err != nil || n != 0 {
		t.Fatalf("read handle wrote: %d %v", n, err)
	}
}

func TestSpecDeclaresEffects(t *testing.T) {
	for _, tc := range []struct {
		op, in, class, hold string
	}{
		{OpSearch, `{}`, "read", "none"},
		{OpPut, `{}`, "write", "shared-writer"},
		{OpCollectionAdd, `{}`, "destructive", "exclusive"},
		{OpImport, `{}`, "write", "shared-writer"},
		{OpImport, `{"replace":true}`, "destructive", "exclusive"},
	} {
		op, a, ok, err := dblifecycle.LookupCategoryOp(tc.op, json.RawMessage(tc.in))
		if !ok || err != nil {
			t.Fatalf("%s not registered: %v", tc.op, err)
		}
		s := op.Spec(dbstore.Memory, a)
		if s.MutationClass != tc.class || s.Hold != tc.hold {
			t.Fatalf("%s %s: %+v", tc.op, tc.in, s)
		}
		for _, e := range s.Effects {
			if strings.HasPrefix(e, "fs.") && tc.op != OpImport {
				t.Fatalf("%s memory spec touches the filesystem: %v", tc.op, s.Effects)
			}
		}
	}
	if _, _, _, err := dblifecycle.LookupCategoryOp(OpSearch, json.RawMessage(`{"filter":{"field":"a'","op":"eq","value":1}}`)); dbstore.Code(err) != "invalid_input" {
		t.Fatalf("malformed filter admitted: %v", err)
	}
}
