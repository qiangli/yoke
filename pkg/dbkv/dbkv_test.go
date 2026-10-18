package dbkv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

type fixture struct {
	t      *testing.T
	svc    dblifecycle.Service
	target dbstore.Target
}

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
		f.target = dbstore.Target{Engine: "bbolt", Mode: dbstore.Memory, Session: mem.ID(), Name: "t"}
	} else {
		f.target = dbstore.Target{Engine: "bbolt", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "t.db")}
	}
	if _, err := f.svc.Create(context.Background(), f.target, time.Second); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) run(op string, in Input, yes bool) (dblifecycle.Outcome, error) {
	b, _ := json.Marshal(in)
	_, out, err := f.svc.RunCategory(context.Background(), f.target, op, b, yes, time.Second)
	return out, err
}

func (f *fixture) must(op string, in Input, yes bool) dblifecycle.Outcome {
	f.t.Helper()
	out, err := f.run(op, in, yes)
	if err != nil {
		f.t.Fatalf("%s: %v", op, err)
	}
	return out
}

func (f *fixture) code(op string, in Input, yes bool) string {
	f.t.Helper()
	_, err := f.run(op, in, yes)
	return dbstore.Code(err)
}

func txt(s string) *Value { return &Value{Text: &s} }
func b64(s string) *Value { return &Value{B64: &s} }

func (f *fixture) get(bucket, key string) (string, bool) {
	f.t.Helper()
	r := f.must(OpGet, Input{Bucket: bucket, Key: txt(key)}, false).Result.(map[string]any)
	if !r["found"].(bool) {
		return "", false
	}
	v := r["value"].(Value)
	b, err := v.Bytes()
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b), true
}

var modes = []dbstore.Mode{dbstore.Persistent, dbstore.Memory}

func TestLifecycleGetPutDeleteBothModes(t *testing.T) {
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			if c := f.code(OpPut, Input{Bucket: "b", Key: txt("k"), Value: txt("v")}, false); c != "not_found" {
				t.Fatalf("put into a missing bucket must not create it, got %q", c)
			}
			f.must(OpBucketCreate, Input{Bucket: "b"}, false)
			if c := f.code(OpBucketCreate, Input{Bucket: "b"}, false); c != "exists" {
				t.Fatalf("duplicate bucket: %q", c)
			}
			r := f.must(OpPut, Input{Bucket: "b", Key: txt("k"), Value: txt("v")}, false).Result.(map[string]any)
			if !r["created"].(bool) {
				t.Fatalf("%v", r)
			}
			if v, ok := f.get("b", "k"); !ok || v != "v" {
				t.Fatalf("get %q %v", v, ok)
			}
			f.must(OpPut, Input{Bucket: "b", Key: txt("k"), Value: txt("w")}, false)
			if v, _ := f.get("b", "k"); v != "w" {
				t.Fatalf("update %q", v)
			}
			if d := f.must(OpDelete, Input{Bucket: "b", Key: txt("k")}, false).Result.(map[string]any)["deleted"]; d != true {
				t.Fatalf("delete %v", d)
			}
			if _, ok := f.get("b", "k"); ok {
				t.Fatal("deleted key still present")
			}
			if c := f.code(OpBucketDrop, Input{Bucket: "b"}, false); c != "confirmation_required" {
				t.Fatalf("drop without --yes: %q", c)
			}
			f.must(OpBucketDrop, Input{Bucket: "b"}, true)
			if l := f.must(OpBucketList, Input{}, false).Result.(map[string]any)["buckets"].([]string); len(l) != 0 {
				t.Fatalf("buckets %v", l)
			}
			if c := f.code(OpGet, Input{Bucket: "b", Key: txt("k")}, false); c != "not_found" {
				t.Fatalf("get after drop: %q", c)
			}
		})
	}
}

func TestBinaryAndInvalidInput(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.must(OpBucketCreate, Input{Bucket: "b"}, false)
	f.must(OpPut, Input{Bucket: "b", Key: b64("AP8A"), Value: b64("/wD/")}, false)
	r := f.must(OpGet, Input{Bucket: "b", Key: b64("AP8A")}, false).Result.(map[string]any)
	if v := r["value"].(Value); v.B64 == nil || *v.B64 != "/wD/" {
		t.Fatalf("binary value not preserved: %+v", v)
	}
	for name, in := range map[string]Input{
		"empty key":     {Bucket: "b", Key: txt(""), Value: txt("v")},
		"no value":      {Bucket: "b", Key: txt("k")},
		"both encoding": {Bucket: "b", Key: &Value{Text: new(string), B64: new(string)}, Value: txt("v")},
		"bad b64":       {Bucket: "b", Key: b64("!!"), Value: txt("v")},
		"no bucket":     {Key: txt("k"), Value: txt("v")},
	} {
		if c := f.code(OpPut, in, false); c != "invalid_input" {
			t.Errorf("%s: %q", name, c)
		}
	}
	if c := f.code(OpPut, Input{Bucket: "b", Key: txt("k"), Value: txt("v"), TTL: "5s"}, false); c != "unsupported" {
		t.Errorf("ttl: %q", c)
	}
}

func TestScanBoundsPrefixAndCursor(t *testing.T) {
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			f.must(OpBucketCreate, Input{Bucket: "b"}, false)
			var ops []BatchOp
			for _, k := range []string{"a1", "a2", "a3", "b1", "b2"} {
				ops = append(ops, BatchOp{Op: "put", Bucket: "b", Key: txt(k), Value: txt("v" + k)})
			}
			f.must(OpBatch, Input{Ops: ops}, false)
			var seen []string
			since := ""
			for pages := 0; ; pages++ {
				out := f.must(OpScan, Input{Bucket: "b", Prefix: txt("a"), Limit: 2, Since: since}, false)
				for _, e := range out.Result.(map[string]any)["entries"].([]entry) {
					seen = append(seen, *e.Key.Text)
				}
				p := out.Page.(page)
				if !p.Truncated {
					break
				}
				if pages > 5 {
					t.Fatal("cursor does not advance")
				}
				b, _ := json.Marshal(p.Cursor)
				since = string(b)
			}
			if strings.Join(seen, ",") != "a1,a2,a3" {
				t.Fatalf("prefix scan paged: %v", seen)
			}
			if c := f.code(OpScan, Input{Bucket: "b", Limit: 2, Since: `{"schema_version":"bashy-cursor-v1","kind":"pagination","token":"k:YQ==","binding":{"provider":"db","target":"x","query":"y"}}`}, false); c != "invalid_input" {
				t.Fatalf("foreign cursor: %q", c)
			}
			if c := f.code(OpScan, Input{Bucket: "b", Limit: MaxLimit + 1}, false); c != "invalid_input" {
				t.Fatalf("limit over max: %q", c)
			}
			if c := f.code(OpScan, Input{Bucket: "b", MaxBytes: 5}, false); c != "limit_exceeded" {
				t.Fatalf("one entry over max-bytes: %q", c)
			}
		})
	}
}

func TestCASAndBatchAtomicity(t *testing.T) {
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			f.must(OpBucketCreate, Input{Bucket: "b"}, false)
			f.must(OpCAS, Input{Bucket: "b", Key: txt("k"), ExpectAbsent: true, Value: txt("1")}, false)
			if c := f.code(OpCAS, Input{Bucket: "b", Key: txt("k"), ExpectAbsent: true, Value: txt("x")}, false); c != "conflict" {
				t.Fatalf("same-key insert conflict: %q", c)
			}
			if c := f.code(OpCAS, Input{Bucket: "b", Key: txt("k"), Expect: txt("0"), Value: txt("x")}, false); c != "conflict" {
				t.Fatalf("stale expectation: %q", c)
			}
			if c := f.code(OpCAS, Input{Bucket: "b", Key: txt("k"), Value: txt("x")}, false); c != "invalid_input" {
				t.Fatalf("cas without expectation: %q", c)
			}
			f.must(OpCAS, Input{Bucket: "b", Key: txt("k"), Expect: txt("1"), Value: txt("2")}, false)
			if v, _ := f.get("b", "k"); v != "2" {
				t.Fatalf("cas: %q", v)
			}
			// A failing check rolls back the puts before it.
			_, err := f.run(OpBatch, Input{Ops: []BatchOp{
				{Op: "put", Bucket: "b", Key: txt("p"), Value: txt("1")},
				{Op: "check", Bucket: "b", Key: txt("k"), Expect: txt("stale")},
			}}, false)
			if dbstore.Code(err) != "conflict" || !strings.Contains(err.Error(), "op 2") {
				t.Fatalf("batch: %v", err)
			}
			if _, ok := f.get("b", "p"); ok {
				t.Fatal("batch was half-applied")
			}
			// A missing bucket mid-batch also rolls everything back, buckets included.
			if c := f.code(OpBatch, Input{Ops: []BatchOp{
				{Op: "create_bucket", Bucket: "new"},
				{Op: "put", Bucket: "nope", Key: txt("k"), Value: txt("v")},
			}}, false); c != "not_found" {
				t.Fatalf("batch missing bucket: %q", c)
			}
			if l := f.must(OpBucketList, Input{}, false).Result.(map[string]any)["buckets"].([]string); len(l) != 1 {
				t.Fatalf("created bucket survived a rolled-back batch: %v", l)
			}
		})
	}
}

func TestConcurrentCASNoLostUpdate(t *testing.T) {
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			f.must(OpBucketCreate, Input{Bucket: "b"}, false)
			f.must(OpPut, Input{Bucket: "b", Key: txt("n"), Value: txt("0")}, false)
			var wg sync.WaitGroup
			wins := make(chan string, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := f.run(OpCAS, Input{Bucket: "b", Key: txt("n"), Expect: txt("0"), Value: txt("won")}, false); err == nil {
						wins <- "w"
					}
				}()
			}
			wg.Wait()
			close(wins)
			if len(wins) != 1 {
				t.Fatalf("%d contenders won one compare-and-set", len(wins))
			}
		})
	}
}

func TestCancelledOperation(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.must(OpBucketCreate, Input{Bucket: "b"}, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, _ := json.Marshal(Input{Bucket: "b", Key: txt("k"), Value: txt("v")})
	_, _, err := f.svc.RunCategory(ctx, f.target, OpPut, b, false, time.Second)
	if err == nil {
		t.Fatal("cancelled put succeeded")
	}
	if _, ok := f.get("b", "k"); ok {
		t.Fatal("cancelled put changed data")
	}
}

func TestPersistentReopenKeepsData(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.must(OpBucketCreate, Input{Bucket: "b"}, false)
	f.must(OpPut, Input{Bucket: "b", Key: txt("k"), Value: txt("v")}, false)
	g := &fixture{t: t, target: f.target}
	if v, ok := g.get("b", "k"); !ok || v != "v" {
		t.Fatalf("reopen: %q %v", v, ok)
	}
}

// seed fills a target with text, binary, empty-value and empty-bucket data.
func seed(f *fixture) {
	f.must(OpBucketCreate, Input{Bucket: "users"}, false)
	f.must(OpBucketCreate, Input{Bucket: "empty"}, false)
	f.must(OpBatch, Input{Ops: []BatchOp{
		{Op: "put", Bucket: "users", Key: txt("ada"), Value: txt("☃ snow \"q\"\n")},
		{Op: "put", Bucket: "users", Key: b64("AP8A"), Value: b64("/wD/")},
		{Op: "put", Bucket: "users", Key: txt("blank"), Value: txt("")},
	}}, false)
}

func dump(f *fixture) string {
	f.t.Helper()
	var sb strings.Builder
	for _, b := range f.must(OpBucketList, Input{}, false).Result.(map[string]any)["buckets"].([]string) {
		sb.WriteString("[" + b + "]")
		out := f.must(OpScan, Input{Bucket: b}, false)
		j, _ := json.Marshal(out.Result.(map[string]any)["entries"])
		sb.Write(j)
	}
	return sb.String()
}

func TestInterchangeRoundTripAcrossModes(t *testing.T) {
	for _, from := range modes {
		for _, to := range modes {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				src, dst := newFixture(t, from), newFixture(t, to)
				seed(src)
				dir := filepath.Join(t.TempDir(), "export")
				src.must(OpExport, Input{Out: dir}, false)
				if c := src.code(OpExport, Input{Out: dir}, false); c != "exists" {
					t.Fatalf("export over an existing dir: %q", c)
				}
				dst.must(OpImport, Input{In: dir}, false)
				if a, b := dump(src), dump(dst); a != b {
					t.Fatalf("round trip differs:\n%s\n%s", a, b)
				}
				// Re-importing the same data is idempotent, not a conflict.
				dst.must(OpImport, Input{In: dir}, false)
			})
		}
	}
}

func TestImportConflictsReplaceAndValidation(t *testing.T) {
	src, dst := newFixture(t, dbstore.Persistent), newFixture(t, dbstore.Memory)
	seed(src)
	dir := filepath.Join(t.TempDir(), "export")
	src.must(OpExport, Input{Out: dir}, false)

	dst.must(OpBucketCreate, Input{Bucket: "users"}, false)
	dst.must(OpPut, Input{Bucket: "users", Key: txt("ada"), Value: txt("mine")}, false)
	dst.must(OpBucketCreate, Input{Bucket: "keep"}, false)
	if c := dst.code(OpImport, Input{In: dir}, false); c != "conflict" {
		t.Fatalf("default conflict policy: %q", c)
	}
	if l := dst.must(OpBucketList, Input{}, false).Result.(map[string]any)["buckets"].([]string); len(l) != 2 {
		t.Fatalf("failed import left partial state: %v", l)
	}
	if v, _ := dst.get("users", "ada"); v != "mine" {
		t.Fatalf("failed import changed a value: %q", v)
	}
	dst.must(OpImport, Input{In: dir, OnConflict: "skip"}, false)
	if v, _ := dst.get("users", "ada"); v != "mine" {
		t.Fatalf("skip overwrote: %q", v)
	}
	dst.must(OpImport, Input{In: dir, OnConflict: "replace"}, false)
	if v, _ := dst.get("users", "ada"); !strings.HasPrefix(v, "☃") {
		t.Fatalf("replace kept old value: %q", v)
	}
	if c := dst.code(OpImport, Input{In: dir, Replace: true}, false); c != "confirmation_required" {
		t.Fatalf("--replace without --yes: %q", c)
	}
	dst.must(OpImport, Input{In: dir, Replace: true}, true)
	if a, b := dump(src), dump(dst); a != b {
		t.Fatalf("replace did not make the target equal the export:\n%s\n%s", a, b)
	}

	// Corruption, wrong versions and bad paths are refused before any change.
	mutate := func(name string, fn func(m map[string]any)) string {
		d := filepath.Join(t.TempDir(), name)
		src.must(OpExport, Input{Out: d}, false)
		p := filepath.Join(d, ManifestName)
		b, _ := os.ReadFile(p)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		fn(m)
		b, _ = json.Marshal(m)
		_ = os.WriteFile(p, b, 0o644)
		return d
	}
	before := dump(dst)
	cases := map[string]struct {
		dir, code string
	}{
		"future format":  {mutate("v", func(m map[string]any) { m["format_version"] = "2.0" }), "version_unsupported"},
		"other schema":   {mutate("s", func(m map[string]any) { m["schema_version"] = "other-v9" }), "version_unsupported"},
		"wrong category": {mutate("c", func(m map[string]any) { m["category"] = "sql" }), "unsupported"},
		"path escape": {mutate("p", func(m map[string]any) {
			m["files"].([]any)[0].(map[string]any)["path"] = "../x"
		}), "malformed_input"},
		"bad checksum": {mutate("h", func(m map[string]any) {
			m["files"].([]any)[0].(map[string]any)["sha256"] = strings.Repeat("0", 64)
		}), "integrity"},
	}
	for name, c := range cases {
		if got := dst.code(OpImport, Input{In: c.dir, Replace: true}, true); got != c.code {
			t.Errorf("%s: %q, want %q", name, got, c.code)
		}
	}
	if dump(dst) != before {
		t.Fatal("a refused import changed the target")
	}
	bad := filepath.Join(t.TempDir(), "bad")
	src.must(OpExport, Input{Out: bad}, false)
	m, _ := readManifest(bad)
	_ = os.WriteFile(filepath.Join(bad, m.Files[0].Path), []byte("not json\n"), 0o644)
	if got := dst.code(OpImport, Input{In: bad}, false); got != "malformed_input" && got != "integrity" {
		t.Errorf("garbage record: %q", got)
	}
	if got := dst.code(OpImport, Input{In: filepath.Join(t.TempDir(), "missing")}, false); got != "malformed_input" {
		t.Errorf("missing export: %q", got)
	}
}

func TestExportCancelledLeavesNothing(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	seed(f)
	dir := filepath.Join(t.TempDir(), "export")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, _ := json.Marshal(Input{Out: dir})
	if _, _, err := f.svc.RunCategory(ctx, f.target, OpExport, b, false, time.Second); err == nil {
		t.Fatal("cancelled export succeeded")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("cancelled export left a directory")
	}
}

func TestKVOpsDeclareSpecs(t *testing.T) {
	for _, op := range []string{OpBucketCreate, OpBucketDrop, OpBucketList, OpGet, OpPut, OpDelete, OpScan, OpBatch, OpCAS, OpExport, OpImport} {
		if _, _, ok, err := dblifecycle.LookupCategoryOp(op, nil); !ok || err != nil {
			t.Errorf("%s not registered: %v", op, err)
		}
	}
	if s := Spec(OpBucketDrop, dbstore.Memory, dblifecycle.AccessExclusive); s.Hold != "exclusive" || s.MutationClass != "destructive" {
		t.Errorf("%+v", s)
	}
}
