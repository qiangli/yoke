package dbdoc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// seed fills a target with two collections, nested typed documents, a
// patched revision and index definitions.
func seed(f *fixture) {
	f.must(OpCollectionCreate, Input{Collection: "users"}, true)
	f.must(OpCollectionCreate, Input{Collection: "empty"}, true)
	f.must(OpIndexAdd, Input{Collection: "users", Index: "by_email", Fields: []string{"email"}, Unique: true}, true)
	f.must(OpIndexAdd, Input{Collection: "users", Index: "by_age_tag", Fields: []string{"age", "tags[0]"}}, true)
	f.must(OpPut, Input{Collection: "users", ID: "u1", Doc: json.RawMessage(nested)}, false)
	f.must(OpPut, Input{Collection: "users", ID: "ü/2 id", Doc: json.RawMessage(`{"email":"b@x","age":7,"tags":["t"]}`)}, false)
	f.must(OpPatch, Input{Collection: "users", ID: "ü/2 id", Patch: json.RawMessage(`{"age":8.25}`)}, false)
}

// dump is the logical content of a target: every collection, its index
// definitions and every document with id, revision and exact text.
func dump(f *fixture) string {
	f.t.Helper()
	var b strings.Builder
	t, err := f.svc.Open(context.Background(), f.target, true, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer t.Close()
	tx, err := begin(context.Background(), t, false)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.end()
	names, err := tx.collections()
	if err != nil {
		f.t.Fatal(err)
	}
	for _, n := range names {
		defs, _ := tx.indexes(n)
		d, _ := json.Marshal(defs)
		fmt.Fprintf(&b, "collection %s %s\n", n, d)
		rows, err := tx.conn.QueryContext(context.Background(), "select id, revision, doc from "+table(n)+" order by id")
		if err != nil {
			f.t.Fatal(err)
		}
		for rows.Next() {
			var id, doc string
			var rev int64
			_ = rows.Scan(&id, &rev, &doc)
			fmt.Fprintf(&b, "  %q %d %s\n", id, rev, doc)
		}
		rows.Close()
	}
	return b.String()
}

func TestInterchangeRoundTripsAcrossModes(t *testing.T) {
	for _, pair := range [][2]dbstore.Mode{
		{dbstore.Persistent, dbstore.Persistent}, {dbstore.Memory, dbstore.Memory},
		{dbstore.Memory, dbstore.Persistent}, {dbstore.Persistent, dbstore.Memory},
	} {
		t.Run(string(pair[0])+"-to-"+string(pair[1]), func(t *testing.T) {
			src := newFixture(t, pair[0])
			seed(src)
			want := dump(src)
			out := filepath.Join(t.TempDir(), "exp")
			res := src.must(OpExport, Input{Out: out}, false).Result.(map[string]any)
			if res["records"] != int64(2) || res["collections"] != 2 {
				t.Fatalf("export: %v", res)
			}
			if c := src.code(OpExport, Input{Out: out}, false); c != "exists" {
				t.Fatalf("export over existing dir: %s", c)
			}
			dst := newFixture(t, pair[1])
			// Merge needs the collections to exist; nothing is created implicitly.
			if c := dst.code(OpImport, Input{In: out}, false); c != "conflict" {
				t.Fatalf("merge into empty target: %s", c)
			}
			if c := dst.code(OpImport, Input{In: out, Replace: true}, false); c != "confirmation_required" {
				t.Fatalf("unconfirmed replace: %s", c)
			}
			r := dst.must(OpImport, Input{In: out, Replace: true}, true).Result.(map[string]any)
			if r["indexes_rebuilt"] != 2 || r["records_applied"] != int64(2) {
				t.Fatalf("import: %v", r)
			}
			if got := dump(dst); got != want {
				t.Fatalf("round trip differs\n got %s\nwant %s", got, want)
			}
			// The rebuilt unique index is enforced in the target.
			if c := dst.code(OpPut, Input{Collection: "users", ID: "n", Doc: json.RawMessage(`{"email":"b@x"}`)}, false); c != "conflict" {
				t.Fatalf("rebuilt unique index: %s", c)
			}
		})
	}
}

func TestImportMergeConflictPolicies(t *testing.T) {
	src := newFixture(t, dbstore.Persistent)
	seed(src)
	out := filepath.Join(t.TempDir(), "exp")
	src.must(OpExport, Input{Out: out}, false)
	dst := newFixture(t, dbstore.Memory)
	dst.must(OpImport, Input{In: out, Replace: true}, true)
	dst.must(OpPut, Input{Collection: "users", ID: "u1", Doc: json.RawMessage(`{"local":true}`)}, false)
	dst.must(OpPut, Input{Collection: "users", ID: "u1", Doc: json.RawMessage(`{"local":2}`)}, false)
	dst.must(OpRm, Input{Collection: "users", ID: "ü/2 id"}, false)
	before := dump(dst)
	if c := dst.code(OpImport, Input{In: out}, false); c != "conflict" {
		t.Fatalf("default error policy: %s", c)
	}
	if dump(dst) != before {
		t.Fatal("failed import changed the target")
	}
	r := dst.must(OpImport, Input{In: out, OnConflict: "skip"}, false).Result.(map[string]any)
	if r["records_applied"] != int64(1) || string(dst.get("users", "u1").Doc) != `{"local":2}` || dst.get("users", "ü/2 id").Revision != 2 {
		t.Fatalf("skip: %v", r)
	}
	dst.must(OpImport, Input{In: out, OnConflict: "replace"}, false)
	if d := dst.get("users", "u1"); !strings.Contains(string(d.Doc), `"big":12345678901234567890`) || d.Revision != 4 {
		t.Fatalf("replace moves past both revisions: %s rev %d", d.Doc, d.Revision)
	}
	if c := dst.code(OpImport, Input{In: out, OnConflict: "replace", Replace: true}, true); c != "invalid_input" {
		t.Fatalf("replace with on-conflict: %s", c)
	}
	// Merge into a collection with different index definitions is refused.
	dst.must(OpIndexRm, Input{Collection: "users", Index: "by_age_tag"}, true)
	if c := dst.code(OpImport, Input{In: out, OnConflict: "skip"}, false); c != "conflict" {
		t.Fatalf("index mismatch: %s", c)
	}
}

func TestImportValidatesBeforeMutation(t *testing.T) {
	src := newFixture(t, dbstore.Persistent)
	seed(src)
	good := filepath.Join(t.TempDir(), "exp")
	src.must(OpExport, Input{Out: good}, false)
	variant := func(name string, edit func(dir string)) string {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.CopyFS(dir, os.DirFS(good)); err != nil {
			t.Fatal(err)
		}
		edit(dir)
		return dir
	}
	editManifest := func(fn func(m map[string]any)) func(string) {
		return func(dir string) {
			p := filepath.Join(dir, ManifestName)
			b, _ := os.ReadFile(p)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			fn(m)
			b, _ = json.Marshal(m)
			_ = os.WriteFile(p, b, 0o600)
		}
	}
	appendRecord := func(line string) func(string) {
		return func(dir string) {
			p := filepath.Join(dir, "records-000001.jsonl")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, append(b, line...), 0o600)
		}
	}
	dst := newFixture(t, dbstore.Persistent)
	dst.must(OpCollectionCreate, Input{Collection: "keep"}, true)
	dst.must(OpPut, Input{Collection: "keep", ID: "k", Doc: json.RawMessage(`{"v":1}`)}, false)
	before := dump(dst)
	for _, tc := range []struct {
		name, code string
		edit       func(string)
	}{
		{"major", "version_unsupported", editManifest(func(m map[string]any) { m["format_version"] = "2.0" })},
		{"schema", "version_unsupported", editManifest(func(m map[string]any) { m["schema_version"] = "bashy-db-export-v2" })},
		{"category", "unsupported", editManifest(func(m map[string]any) { m["category"] = "sql" })},
		{"docformat", "version_unsupported", editManifest(func(m map[string]any) { m["schema_def"].(map[string]any)["doc_format"] = "9" })},
		{"path", "malformed_input", editManifest(func(m map[string]any) { m["files"].([]any)[0].(map[string]any)["path"] = "../x" })},
		{"tamper", "integrity", appendRecord(`{"id":"zz","revision":1,"doc":{}}` + "\n")},
		{"notjson", "malformed_input", appendRecord("{\n")},
		{"array", "malformed_input", appendRecord(`{"id":"zz","revision":1,"doc":[1]}` + "\n")},
		{"dupkey", "malformed_input", appendRecord(`{"id":"zz","revision":1,"doc":{"a":1,"a":2}}` + "\n")},
		{"norev", "malformed_input", appendRecord(`{"id":"zz","revision":0,"doc":{}}` + "\n")},
		{"extra", "malformed_input", appendRecord(`{"id":"zz","revision":1,"doc":{},"x":1}` + "\n")},
		{"nonewline", "malformed_input", appendRecord(`{"id":"zz","revision":1,"doc":{}}`)},
		// Trailing JSON after a valid record, with the manifest's count,
		// length and sha256 recomputed so only the parser can refuse it.
		{"trailing", "malformed_input", func(dir string) {
			rehash(t, dir, "records-000001.jsonl", func(s string) string {
				return strings.Replace(s, "\n", `{"id":"zz","revision":1,"doc":{}}`+"\n", 1)
			})
		}},
		{"missing", "not_found", func(dir string) { os.Remove(filepath.Join(dir, ManifestName)) }},
	} {
		dir := variant(tc.name, tc.edit)
		if c := dst.code(OpImport, Input{In: dir, Replace: true}, true); c != tc.code {
			t.Fatalf("%s: got %s want %s", tc.name, c, tc.code)
		}
		if dump(dst) != before {
			t.Fatalf("%s: rejected import changed the target", tc.name)
		}
	}
}

// rehash rewrites one record file of an export and fixes the manifest's
// records/bytes/sha256 for it, so the edit passes the integrity checks.
func rehash(t *testing.T, dir, path string, edit func(string) string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		t.Fatal(err)
	}
	out := edit(string(b))
	if err := os.WriteFile(filepath.Join(dir, path), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(dir, ManifestName)
	mb, _ := os.ReadFile(mp)
	var m map[string]any
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(out))
	for _, f := range m["files"].([]any) {
		if f := f.(map[string]any); f["path"] == path {
			f["records"], f["bytes"], f["sha256"] = strings.Count(out, "\n"), len(out), hex.EncodeToString(sum[:])
		}
	}
	mb, _ = json.Marshal(m)
	if err := os.WriteFile(mp, mb, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A replaced document moves strictly past both its current and its
// imported revision; when no later revision exists the import fails and
// rolls back instead of overflowing.
func TestImportReplaceRevisionExhaustion(t *testing.T) {
	src := newFixture(t, dbstore.Persistent)
	src.must(OpCollectionCreate, Input{Collection: "c"}, true)
	src.must(OpPut, Input{Collection: "c", ID: "a", Doc: json.RawMessage(`{"v":"new"}`)}, false)
	good := filepath.Join(t.TempDir(), "exp")
	src.must(OpExport, Input{Out: good}, false)
	withRev := func(rev string) string {
		dir := filepath.Join(t.TempDir(), rev)
		if err := os.CopyFS(dir, os.DirFS(good)); err != nil {
			t.Fatal(err)
		}
		rehash(t, dir, "records-000000.jsonl", func(s string) string {
			return strings.Replace(s, `"revision":1`, `"revision":`+rev, 1)
		})
		return dir
	}
	dst := newFixture(t, dbstore.Persistent)
	dst.must(OpCollectionCreate, Input{Collection: "c"}, true)
	dst.must(OpPut, Input{Collection: "c", ID: "a", Doc: json.RawMessage(`{"v":"old"}`)}, false)
	dst.must(OpPut, Input{Collection: "c", ID: "a", Doc: json.RawMessage(`{"v":"old"}`)}, false)
	dst.must(OpImport, Input{In: withRev("5"), OnConflict: "replace"}, false)
	if d := dst.get("c", "a"); d.Revision != 6 || string(d.Doc) != `{"v":"new"}` {
		t.Fatalf("replace over revision 5: %d %s", d.Revision, d.Doc)
	}
	before := dump(dst)
	if c := dst.code(OpImport, Input{In: withRev(strconv.FormatInt(math.MaxInt64, 10)), OnConflict: "replace"}, false); c != "limit_exceeded" {
		t.Fatalf("exhausted revision: got %s", c)
	}
	if dump(dst) != before {
		t.Fatal("exhausted-revision import changed the target")
	}
}

// Export refuses a record its own import would refuse, and removes the
// partial directory.
func TestExportRejectsOversizedRecord(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.must(OpCollectionCreate, Input{Collection: "c"}, true)
	h, err := f.svc.Open(context.Background(), f.target, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := begin(context.Background(), h, true)
	if err != nil {
		t.Fatal(err)
	}
	big := `{"s":"` + strings.Repeat("x", MaxRecordBytes) + `"}`
	if _, err := tx.exec("insert into "+table("c")+" (id, revision, doc) values (?, 1, ?)", "a", big); err != nil {
		t.Fatal(err)
	}
	if err := tx.commit(); err != nil {
		t.Fatal(err)
	}
	tx.end()
	h.Close()
	out := filepath.Join(t.TempDir(), "exp")
	if c := f.code(OpExport, Input{Out: out}, false); c != "limit_exceeded" {
		t.Fatalf("oversized export: got %s", c)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("failed export left %s: %v", out, err)
	}
}

func TestImportCancellationRollsBack(t *testing.T) {
	src := newFixture(t, dbstore.Memory)
	src.must(OpCollectionCreate, Input{Collection: "c"}, true)
	var ops []BatchOp
	for i := 0; i < 600; i++ {
		ops = append(ops, BatchOp{Op: "put", ID: fmt.Sprintf("%04d", i), Doc: json.RawMessage(`{"v":1}`)})
	}
	src.must(OpBatch, Input{Collection: "c", Ops: ops}, false)
	out := filepath.Join(t.TempDir(), "exp")
	src.must(OpExport, Input{Out: out}, false)
	dst := newFixture(t, dbstore.Persistent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dst.runCtx(ctx, OpImport, Input{In: out, Replace: true}, true); dbstore.Code(err) != "cancelled" {
		t.Fatalf("cancelled import: %v", err)
	}
	if got := dump(dst); got != "" {
		t.Fatalf("cancelled import left %s", got)
	}
	if _, err := src.runCtx(ctx, OpExport, Input{Out: filepath.Join(t.TempDir(), "x")}, false); dbstore.Code(err) != "cancelled" {
		t.Fatalf("cancelled export: %v", err)
	}
}
