package dbvec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// populate gives a fixture two collections with lossy floats, nested
// metadata and a bumped revision.
func populate(f *fixture) {
	f.t.Helper()
	f.must(OpCollectionAdd, Input{Collection: "docs", Dimension: 4, Metric: "cosine", Model: "nomic-embed-text@v1.5"}, true)
	f.must(OpCollectionAdd, Input{Collection: "imgs", Dimension: 2, Metric: "l2", Model: "clip@2"}, true)
	f.put("docs", "a", lossy, `{"src":"kb/a.md","n":12345678901234567890,"dec":1.50,"tags":["x",{"y":null}]}`)
	f.put("docs", "b", []float32{1, 2, 3, 4}, "")
	f.must(OpPut, Input{Collection: "docs", ID: "b", IfRevision: rev(1), Vector: tagged([]float32{4, 3, 2, 1})}, false)
	f.put("imgs", "é/1", []float32{0.1, -0.2}, `{"w":640}`)
}

func exportTo(t *testing.T, f *fixture) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "export")
	res := f.must(OpExport, Input{Out: out}, false).Result.(map[string]any)
	if res["records"] != int64(3) || res["collections"] != 2 {
		t.Fatalf("export: %v", res)
	}
	return out
}

// sameContents compares two fixtures item by item, bits included.
func sameContents(t *testing.T, a, b *fixture) {
	t.Helper()
	la, _ := json.Marshal(a.must(OpCollectionList, Input{}, false).Result)
	lb, _ := json.Marshal(b.must(OpCollectionList, Input{}, false).Result)
	if string(la) != string(lb) {
		t.Fatalf("collections differ:\n%s\n%s", la, lb)
	}
	for _, k := range [][2]string{{"docs", "a"}, {"docs", "b"}, {"imgs", "é/1"}} {
		x, y := a.get(k[0], k[1]), b.get(k[0], k[1])
		if x.Revision != y.Revision || string(x.Metadata) != string(y.Metadata) || !sameBits(decoded(t, x.Vector), decoded(t, y.Vector)) {
			t.Fatalf("%s/%s differs: %+v vs %+v", k[0], k[1], x, y)
		}
	}
}

func TestExportImportRoundTripsAcrossModes(t *testing.T) {
	for _, dir := range [][2]dbstore.Mode{
		{dbstore.Memory, dbstore.Persistent},
		{dbstore.Persistent, dbstore.Memory},
		{dbstore.Persistent, dbstore.Persistent},
		{dbstore.Memory, dbstore.Memory},
	} {
		t.Run(string(dir[0])+"-to-"+string(dir[1]), func(t *testing.T) {
			src := newFixture(t, dir[0])
			populate(src)
			out := exportTo(t, src)
			dst := newFixture(t, dir[1])
			if c := dst.code(OpImport, Input{In: out, Replace: true}, false); c != "confirmation_required" {
				t.Fatalf("unconfirmed replace: %s", c)
			}
			res := dst.must(OpImport, Input{In: out, Replace: true}, true).Result.(map[string]any)
			if res["records_applied"] != int64(3) || res["atomic"] != true {
				t.Fatalf("import: %v", res)
			}
			sameContents(t, src, dst)
			// The imported collection is live: search and the persisted
			// dimension behave as in the source.
			if got := dst.search(Input{Collection: "docs", Vector: tagged(lossy), Limit: 1}); !strings.HasPrefix(got, "a@0.0000") {
				t.Fatalf("search after import: %s", got)
			}
			if c := dst.code(OpPut, Input{Collection: "imgs", ID: "x", Vector: json.RawMessage(`[1,2,3]`)}, false); c != "invalid_input" {
				t.Fatalf("imported dimension: %s", c)
			}
		})
	}
}

func TestImportMergeConflictPolicies(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		populate(f)
		out := exportTo(t, f)
		// error (the default) aborts with nothing applied.
		f.put("imgs", "new", []float32{9, 9}, "")
		f.must(OpRm, Input{Collection: "docs", ID: "a"}, false)
		if c := f.code(OpImport, Input{In: out}, false); c != "conflict" {
			t.Fatalf("merge on existing ids: %s", c)
		}
		if c := f.code(OpGet, Input{Collection: "docs", ID: "a"}, false); c != "not_found" {
			t.Fatalf("failed import half-applied: %s", c)
		}
		// skip restores only the absent id at its exported revision.
		res := f.must(OpImport, Input{In: out, OnConflict: "skip"}, false).Result.(map[string]any)
		if res["records_applied"] != int64(1) || f.get("docs", "a").Revision != 1 {
			t.Fatalf("skip: %v", res)
		}
		// replace moves past both revisions so a holder of either conflicts.
		f.must(OpPut, Input{Collection: "docs", ID: "b", Vector: tagged([]float32{1, 1, 1, 1})}, false) // rev 3
		f.must(OpImport, Input{In: out, OnConflict: "replace"}, false)
		if b := f.get("docs", "b"); b.Revision != 4 || !sameBits(decoded(t, b.Vector), []float32{4, 3, 2, 1}) {
			t.Fatalf("replace: %+v", b)
		}
		if f.get("imgs", "new").Revision != 1 {
			t.Fatal("merge removed an item not in the export")
		}
		// --replace empties the vector collections first.
		f.must(OpImport, Input{In: out, Replace: true}, true)
		if c := f.code(OpGet, Input{Collection: "imgs", ID: "new"}, false); c != "not_found" {
			t.Fatalf("replace kept an item: %s", c)
		}
		if c := f.code(OpImport, Input{In: out, Replace: true, OnConflict: "skip"}, true); c != "invalid_input" {
			t.Fatalf("replace with on-conflict: %s", c)
		}
		// Merge needs the same dimension, metric and model.
		g := newFixture(t, f.target.Mode)
		g.must(OpCollectionAdd, Input{Collection: "docs", Dimension: 4, Metric: "cosine", Model: "other@1"}, true)
		g.must(OpCollectionAdd, Input{Collection: "imgs", Dimension: 2, Metric: "l2", Model: "clip@2"}, true)
		if c := g.code(OpImport, Input{In: out}, false); c != "conflict" {
			t.Fatalf("merge into another model: %s", c)
		}
		h := newFixture(t, f.target.Mode)
		if c := h.code(OpImport, Input{In: out}, false); c != "conflict" {
			t.Fatalf("merge without collections: %s", c)
		}
	})
}

func TestImportValidatesBeforeChanging(t *testing.T) {
	f := newFixture(t, dbstore.Memory)
	populate(f)
	out := exportTo(t, f)
	if c := f.code(OpExport, Input{Out: out}, false); c != "exists" {
		t.Fatalf("export over existing dir: %s", c)
	}
	if c := f.code(OpExport, Input{Out: "rel"}, false); c != "invalid_input" {
		t.Fatalf("relative out: %s", c)
	}
	mutate := func(t *testing.T, edit func(dir string)) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "x")
		if err := os.CopyFS(dir, os.DirFS(out)); err != nil {
			t.Fatal(err)
		}
		edit(dir)
		return dir
	}
	manifestEdit := func(dir string, fn func(m map[string]any)) {
		b, _ := os.ReadFile(filepath.Join(dir, ManifestName))
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		fn(m)
		b, _ = json.Marshal(m)
		_ = os.WriteFile(filepath.Join(dir, ManifestName), b, 0o600)
	}
	recordEdit := func(dir string, fn func(string) string) {
		p := filepath.Join(dir, "records-000000.jsonl")
		b, _ := os.ReadFile(p)
		_ = os.WriteFile(p, []byte(fn(string(b))), 0o600)
	}
	for name, tc := range map[string]struct {
		edit func(string)
		code string
	}{
		"schema v2":     {func(d string) { manifestEdit(d, func(m map[string]any) { m["schema_version"] = "bashy-db-export-v2" }) }, "version_unsupported"},
		"format 2.0":    {func(d string) { manifestEdit(d, func(m map[string]any) { m["format_version"] = "2.0" }) }, "version_unsupported"},
		"not an export": {func(d string) { manifestEdit(d, func(m map[string]any) { m["schema_version"] = "x" }) }, "malformed_input"},
		"documents":     {func(d string) { manifestEdit(d, func(m map[string]any) { m["category"] = "documents" }) }, "unsupported"},
		"vec format": {func(d string) {
			manifestEdit(d, func(m map[string]any) { m["schema_def"].(map[string]any)["vec_format"] = "9" })
		}, "version_unsupported"},
		"bad metric": {func(d string) {
			manifestEdit(d, func(m map[string]any) {
				m["schema_def"].(map[string]any)["collections"].([]any)[0].(map[string]any)["metric"] = "dot"
			})
		}, "malformed_input"},
		"escaping path": {func(d string) {
			manifestEdit(d, func(m map[string]any) { m["files"].([]any)[0].(map[string]any)["path"] = "../x" })
		}, "malformed_input"},
		"no manifest": {func(d string) { _ = os.Remove(filepath.Join(d, ManifestName)) }, "not_found"},
		"tampered": {func(d string) {
			recordEdit(d, func(s string) string { return strings.Replace(s, `"revision":1`, `"revision":7`, 1) })
		}, "integrity"},
		"untagged": {func(d string) {
			recordEdit(d, func(s string) string {
				lines := strings.SplitN(s, "\n", 2)
				var r map[string]any
				_ = json.Unmarshal([]byte(lines[0]), &r)
				r["vector"] = []float32{1, 2, 3, 4}
				b, _ := json.Marshal(r)
				return string(b) + "\n" + lines[1]
			})
		}, "malformed_input"},
		"wrong dimension": {func(d string) {
			manifestEdit(d, func(m map[string]any) {
				m["schema_def"].(map[string]any)["collections"].([]any)[0].(map[string]any)["dimension"] = 3
			})
		}, "malformed_input"},
		"no newline": {func(d string) { recordEdit(d, func(s string) string { return strings.TrimSuffix(s, "\n") }) }, "malformed_input"},
	} {
		t.Run(name, func(t *testing.T) {
			dst := newFixture(t, dbstore.Persistent)
			dst.add("keep", 2, "l2")
			dst.put("keep", "k", []float32{1, 1}, "")
			if c := dst.code(OpImport, Input{In: mutate(t, tc.edit), Replace: true}, true); c != tc.code {
				t.Fatalf("got %s want %s", c, tc.code)
			}
			// Validation runs before the destructive replace.
			if dst.get("keep", "k").Revision != 1 {
				t.Fatal("rejected import changed the target")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.runCtx(ctx, OpImport, Input{In: out, Replace: true}, true); dbstore.Code(err) != "cancelled" {
		t.Fatalf("cancelled import: %v", err)
	}
	if _, err := f.runCtx(ctx, OpExport, Input{Out: filepath.Join(t.TempDir(), "c")}, false); dbstore.Code(err) != "cancelled" {
		t.Fatalf("cancelled export: %v", err)
	}
}
