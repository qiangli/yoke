package dbsql

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// richSchema exercises what §8.2 requires SQL interchange to preserve.
const richSchema = `
create table parent(id integer primary key autoincrement, name text not null unique collate nocase check (length(name) > 0), born date default (date('now')));
create table child(id integer primary key, parent integer not null references parent(id) on delete cascade, doubled integer generated always as (id * 2) stored, note text);
create table loose(a, b numeric, c real);
create table pairs(k text, v blob, primary key (k)) without rowid;
create table typed(i integer, f real, t text, b blob) strict;
create index child_parent on child(parent);
create unique index loose_a on loose(a) where a is not null;
create view parent_names as select name from parent;
create table audit(msg text);
create trigger parent_audit after insert on parent begin insert into audit(msg) values ('added ' || new.name); end;
pragma user_version = 42;`

func populate(t *testing.T, f *fixture) {
	t.Helper()
	f.must(OpSchemaApply, Input{SQL: richSchema}, true)
	f.must(OpExec, Input{Statements: []Statement{
		{SQL: "insert into parent(name, born) values ('Ada', '1815-12-10'), ('Bob', '2024-01-02 03:04:05')"},
		{SQL: "insert into child(parent, note) values (1, 'x'), (2, null)"},
		{SQL: "insert into loose(rowid, a, b, c) values (7, ?, ?, ?), (100, ?, ?, ?), (3, ?, ?, ?)",
			Params: params(t, "int:9223372036854775807", "text:not a number", "f64:-0", "int:-9007199254740993", "f64:1e308", "f64:Inf", "bytes:", "null", "f64:0.1")},
		{SQL: "insert into pairs values (?, ?), (?, ?)", Params: params(t, "text:zero", "bytes:AAAA", "text:☃ snow", "null")},
		{SQL: "insert into typed values (?, ?, ?, ?)", Params: params(t, "int:-1", "f64:2.5e-308", "text:t\n\"q\"", "bytes:/w==")},
		{SQL: "delete from parent where id = 2"},
		{SQL: "insert into parent(name) values ('Cy')"},
	}}, false)
}

func exportTo(t *testing.T, f *fixture) (string, manifest) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "export")
	out := f.must(OpExport, Input{Out: dir}, false)
	if out.Result.(map[string]any)["snapshot"].(snapshot).Method != "read-txn" {
		t.Fatalf("export result %+v", out.Result)
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, m
}

// fingerprint is the export minus where it came from: two databases with
// the same logical content export identical schema and record files.
func fingerprint(t *testing.T, m manifest) string {
	t.Helper()
	m.Source = manifestSource{}
	b, _ := json.Marshal(struct {
		S schemaDef
		F []recordFile
	}{m.SchemaDef, m.Files})
	return string(b)
}

func TestInterchangeRoundTripsAcrossModes(t *testing.T) {
	for _, tt := range []struct{ from, to dbstore.Mode }{
		{dbstore.Persistent, dbstore.Persistent},
		{dbstore.Memory, dbstore.Memory},
		{dbstore.Memory, dbstore.Persistent},
		{dbstore.Persistent, dbstore.Memory},
	} {
		t.Run(string(tt.from)+"-to-"+string(tt.to), func(t *testing.T) {
			src := newFixture(t, tt.from)
			populate(t, src)
			dir, m := exportTo(t, src)
			// The source keeps serving reads and writes during export.
			if len(m.SchemaDef.Sequences) != 1 || m.SchemaDef.UserVersion != 42 {
				t.Fatalf("manifest schema_def %+v", m.SchemaDef)
			}
			dst := newFixture(t, tt.to)
			if c := dst.code(OpImport, Input{In: dir}, false); c != "conflict" {
				t.Fatalf("merge into a different schema = %s", c)
			}
			if c := dst.code(OpImport, Input{In: dir, Replace: true}, false); c != "confirmation_required" {
				t.Fatalf("unconfirmed replace = %s", c)
			}
			res := dst.must(OpImport, Input{In: dir, Replace: true}, true)
			if res.Result.(map[string]any)["records_applied"].(int64) != 13 {
				t.Fatalf("import result %+v", res.Result)
			}
			_, again := exportTo(t, dst)
			if a, b := fingerprint(t, m), fingerprint(t, again); a != b {
				t.Fatalf("round trip changed the database\n src %s\n dst %s", a, b)
			}
			// Exact values: precision, Inf, empty blob, rowid gaps, DATE text.
			// SQLite itself stores an integral REAL compactly, so the bound
			// -0.0 already reads back as 0.0 from the source.
			got := dst.rows("select rowid, +a, +b, +c from loose order by rowid")
			want := `[[{"t":"int","v":"3"},{"t":"bytes","b64":""},null,{"t":"f64","v":0.1}],[{"t":"int","v":"7"},{"t":"int","v":"9223372036854775807"},"not a number",{"t":"f64","v":0}],[{"t":"int","v":"100"},{"t":"int","v":"-9007199254740993"},{"t":"f64","v":1e+308},{"t":"f64","v":"Inf"}]]`
			if got != want {
				t.Fatalf("loose\n got %s\nwant %s", got, want)
			}
			if got := dst.rows("select group_concat(id), (select +born from parent where id = 1), (select count(*) from audit), (select seq from sqlite_sequence) from parent"); got != `[["1,3","1815-12-10",{"t":"int","v":"3"},{"t":"int","v":"3"}]]` {
				t.Fatalf("parent/audit/sequence after import: %s", got)
			}
			// The trigger was recreated after loading (audit not doubled), and works.
			dst.must(OpExec, Input{SQL: "insert into parent(name) values ('Dee')"}, false)
			if got := dst.rows("select count(*), (select max(id) from parent) from audit"); got != `[[{"t":"int","v":"4"},{"t":"int","v":"4"}]]` {
				t.Fatalf("trigger/autoincrement after import: %s", got)
			}
		})
	}
}

// The AUTOINCREMENT high-water mark survives a round trip even when it is
// above every surviving row, so a deleted ID is never handed out again.
func TestImportKeepsSequenceAboveDeletedHighID(t *testing.T) {
	for _, tt := range []struct{ from, to dbstore.Mode }{
		{dbstore.Persistent, dbstore.Memory},
		{dbstore.Memory, dbstore.Persistent},
	} {
		t.Run(string(tt.from)+"-to-"+string(tt.to), func(t *testing.T) {
			src := newFixture(t, tt.from)
			src.must(OpSchemaApply, Input{SQL: "create table s(id integer primary key autoincrement, v text)"}, true)
			src.must(OpExec, Input{SQL: "insert into s(v) values ('a'), ('b'), ('c'), ('d'), ('e'); delete from s where id >= 4"}, false)
			dir, _ := exportTo(t, src)
			dst := newFixture(t, tt.to)
			dst.must(OpImport, Input{In: dir, Replace: true}, true)
			if got := dst.rows("select max(id), (select count(*) from sqlite_sequence where name = 's'), (select seq from sqlite_sequence where name = 's') from s"); got != `[[{"t":"int","v":"3"},{"t":"int","v":"1"},{"t":"int","v":"5"}]]` {
				t.Fatalf("sequence after import: %s", got)
			}
			dst.must(OpExec, Input{SQL: "insert into s(v) values ('f')"}, false)
			if got := dst.rows("select max(id) from s"); got != `[[{"t":"int","v":"6"}]]` {
				t.Fatalf("next generated id: %s", got)
			}
		})
	}
}

func TestImportMergeConflictPolicies(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpSchemaApply, Input{SQL: "create table m(id integer primary key, v text not null check (v <> 'bad'))"}, true)
		f.must(OpExec, Input{SQL: "insert into m values (1, 'one'), (2, 'two')"}, false)
		dir, _ := exportTo(t, f)
		f.must(OpExec, Input{SQL: "update m set v = 'changed' where id = 1; insert into m values (3, 'three')"}, false)
		if c := f.code(OpImport, Input{In: dir}, false); c != "conflict" {
			t.Fatalf("default on-conflict = %s", c)
		}
		if got := f.rows("select v from m order by id"); got != `[["changed"],["two"],["three"]]` {
			t.Fatalf("failed import changed rows: %s", got)
		}
		skip := f.must(OpImport, Input{In: dir, OnConflict: "skip"}, false)
		if skip.Result.(map[string]any)["records_applied"].(int64) != 0 {
			t.Fatalf("skip applied %+v", skip.Result)
		}
		f.must(OpImport, Input{In: dir, OnConflict: "replace"}, false)
		if got := f.rows("select v from m order by id"); got != `[["one"],["two"],["three"]]` {
			t.Fatalf("replace policy: %s", got)
		}
		if c := f.code(OpImport, Input{In: dir, OnConflict: "replace", Replace: true}, true); c != "invalid_input" {
			t.Fatalf("--replace with --on-conflict = %s", c)
		}
	})
}

func TestImportValidatesBeforeMutation(t *testing.T) {
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpSchemaApply, Input{SQL: "create table k(id integer primary key, v text)"}, true)
		f.must(OpExec, Input{SQL: "insert into k values (1, 'keep')"}, false)
		good, m := exportTo(t, f)
		mutate := func(name string, edit func(dir string)) string {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.CopyFS(dir, os.DirFS(good)); err != nil {
				t.Fatal(err)
			}
			edit(dir)
			return dir
		}
		rewrite := func(path string, fn func(string) string) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(fn(string(b))), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		records := m.Files[0].Path
		for _, tt := range []struct {
			name, code string
			edit       func(dir string)
		}{
			{"malformed", "malformed_input", func(d string) { rewrite(filepath.Join(d, ManifestName), func(string) string { return "{" }) }},
			{"major", "version_unsupported", func(d string) {
				rewrite(filepath.Join(d, ManifestName), func(s string) string {
					return strings.Replace(s, `"format_version": "1.0"`, `"format_version": "2.0"`, 1)
				})
			}},
			{"schema", "version_unsupported", func(d string) {
				rewrite(filepath.Join(d, ManifestName), func(s string) string { return strings.Replace(s, ExportSchema, "bashy-db-export-v9", 1) })
			}},
			{"checksum", "integrity", func(d string) {
				rewrite(filepath.Join(d, records), func(s string) string { return strings.Replace(s, "keep", "kEEp", 1) })
			}},
			{"untagged", "invalid_input", func(d string) {
				rewrite(filepath.Join(d, records), func(s string) string { return strings.Replace(s, `{"t":"int","v":"1"}`, `1`, 1) })
			}},
			{"escape", "malformed_input", func(d string) {
				rewrite(filepath.Join(d, ManifestName), func(s string) string { return strings.Replace(s, records, "../"+records, 1) })
			}},
			{"missing", "not_found", func(d string) { os.Remove(filepath.Join(d, ManifestName)) }},
		} {
			dir := mutate(tt.name, tt.edit)
			if c := f.code(OpImport, Input{In: dir, Replace: true}, true); c != tt.code {
				t.Errorf("%s => %s, want %s", tt.name, c, tt.code)
			}
		}
		// A smuggled second statement in a definition is refused, atomically.
		smuggle := mutate("smuggle", func(d string) {
			rewrite(filepath.Join(d, ManifestName), func(s string) string {
				return strings.Replace(s, `v text)"`, `v text); create table extra(x)"`, 1)
			})
		})
		if c := f.code(OpImport, Input{In: smuggle, Replace: true}, true); c != "malformed_input" {
			t.Errorf("smuggled definition => %s", c)
		}
		if got := f.rows("select v, (select count(*) from sqlite_schema) from k"); got != `[["keep",{"t":"int","v":"1"}]]` {
			t.Fatalf("rejected import changed the target: %s", got)
		}
		if c := f.code(OpExport, Input{Out: good}, false); c != "exists" {
			t.Fatalf("export over an existing directory = %s", c)
		}
	})
}

func TestImportCancellationRollsBack(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	f.must(OpSchemaApply, Input{SQL: "create table big(id integer primary key, v blob)"}, true)
	f.must(OpExec, Input{SQL: "insert into big(v) select randomblob(256) from (with recursive c(x) as (select 1 union all select x+1 from c limit 50000) select x from c)"}, false)
	dir, _ := exportTo(t, f)
	dst := newFixture(t, dbstore.Memory)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := dst.runCtx(ctx, OpImport, Input{In: dir, Replace: true}, true)
	if c := dbstore.Code(err); c != "timeout" && c != "cancelled" {
		t.Fatalf("cancelled import = %v", err)
	}
	if got := dst.rows("select count(*) from sqlite_schema"); got != `[[{"t":"int","v":"0"}]]` {
		t.Fatalf("cancelled import left state: %s", got)
	}
}
