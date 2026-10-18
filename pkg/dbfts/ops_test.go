package dbfts

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
		f.svc.Mem = mem
		f.target = dbstore.Target{Engine: "bleve", Mode: mode, Session: mem.ID(), Name: "index"}
	} else {
		f.target = dbstore.Target{Engine: "bleve", Mode: mode, Path: filepath.Join(t.TempDir(), "index")}
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

func TestIndexOperationsBothModes(t *testing.T) {
	for _, mode := range []dbstore.Mode{dbstore.Memory, dbstore.Persistent} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			f.must(OpPut, Input{ID: "a", Doc: json.RawMessage(`{"title":"hello world","n":9007199254740993}`)}, false)
			if _, err := f.run(OpPut, Input{ID: "a", Doc: json.RawMessage(`{"title":"other"}`)}, false); dbstore.Code(err) != "exists" {
				t.Fatalf("duplicate: %v", err)
			}
			f.must(OpPut, Input{ID: "b", Doc: json.RawMessage(`{"title":"hello moon"}`)}, false)
			out := f.must(OpSearch, Input{Query: "hello", Limit: 1}, false)
			if out.Result.(map[string]any)["total"] != uint64(2) {
				t.Fatalf("search: %+v", out)
			}
			current := f.must(OpGet, Input{ID: "a"}, false).Result.(map[string]any)
			if _, err := f.run(OpPut, Input{ID: "a", Doc: json.RawMessage(`{"title":"lost"}`), Replace: true, IfRevision: "stale"}, false); dbstore.Code(err) != "conflict" {
				t.Fatalf("stale update: %v", err)
			}
			f.must(OpPut, Input{ID: "a", Doc: json.RawMessage(`{"title":"updated","n":9007199254740993}`), Replace: true, IfRevision: current["revision"].(string)}, false)
			got := f.must(OpGet, Input{ID: "a"}, false)
			if string(got.Result.(map[string]any)["doc"].(json.RawMessage)) != `{"title":"updated","n":9007199254740993}` {
				t.Fatalf("source changed: %+v", got)
			}
			f.must(OpDelete, Input{ID: "b"}, false)
			if _, err := f.run(OpGet, Input{ID: "b"}, false); dbstore.Code(err) != "not_found" {
				t.Fatalf("delete: %v", err)
			}
			f.must(OpCheck, Input{}, false)
			f.must(OpRebuild, Input{}, true)
			if mode == dbstore.Persistent {
				f.must(OpGet, Input{ID: "a"}, false)
			}
		})
	}
}

func TestLogicalRoundTripBothDirections(t *testing.T) {
	for _, pair := range [][2]dbstore.Mode{{dbstore.Memory, dbstore.Persistent}, {dbstore.Persistent, dbstore.Memory}, {dbstore.Memory, dbstore.Memory}, {dbstore.Persistent, dbstore.Persistent}} {
		t.Run(string(pair[0])+"-"+string(pair[1]), func(t *testing.T) {
			from := newFixture(t, pair[0])
			to := newFixture(t, pair[1])
			raw := json.RawMessage(`{"text":"hello","big":9007199254740993,"binary":"AAE="}`)
			from.must(OpPut, Input{ID: "id:1", Doc: raw}, false)
			dir := filepath.Join(t.TempDir(), "export")
			from.must(OpExport, Input{Out: dir}, false)
			if _, err := os.Stat(filepath.Join(dir, ManifestName)); err != nil {
				t.Fatal(err)
			}
			to.must(OpImport, Input{In: dir}, true)
			got := to.must(OpGet, Input{ID: "id:1"}, false)
			if string(got.Result.(map[string]any)["doc"].(json.RawMessage)) != string(raw) {
				t.Fatalf("roundtrip: %+v", got)
			}
			if _, err := to.run(OpImport, Input{In: dir}, true); dbstore.Code(err) != "conflict" {
				t.Fatalf("import conflict: %v", err)
			}
		})
	}
}

func TestConfiguredMappingAndReopen(t *testing.T) {
	t.Setenv("BASHY_COORD_DIR", filepath.Join(t.TempDir(), "coord"))
	target := dbstore.Target{Engine: "bleve", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "custom")}
	svc := dblifecycle.Service{}
	input := json.RawMessage(`{"mapping":{"default_analyzer":"keyword"}}`)
	if _, err := svc.CreateConfigured(context.Background(), target, input, time.Second); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Input{})
	_, out, err := svc.RunCategory(context.Background(), target, OpMappingShow, b, false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(out.Result)
	if !json.Valid(m) || !bytes.Contains(m, []byte(`"default_analyzer":"keyword"`)) {
		t.Fatalf("mapping not persisted: %s", m)
	}
}

func TestSearchFilterPaginationAndStaleCursor(t *testing.T) {
	f := newFixture(t, dbstore.Memory)
	f.must(OpPut, Input{ID: "a", Doc: json.RawMessage(`{"title":"hello world","group":"one"}`)}, false)
	f.must(OpPut, Input{ID: "b", Doc: json.RawMessage(`{"title":"hello moon","group":"two"}`)}, false)
	f.must(OpPut, Input{ID: "c", Doc: json.RawMessage(`{"title":"hello star","group":"one"}`)}, false)
	first := f.must(OpSearch, Input{Query: "hello", Filter: "group:one", Limit: 1}, false)
	if first.Result.(map[string]any)["total"] != uint64(2) {
		t.Fatalf("filter: %+v", first)
	}
	cursor := first.Page.(map[string]any)["next"].(string)
	if cursor == "" {
		t.Fatalf("missing cursor: %+v", first)
	}
	second := f.must(OpSearch, Input{Query: "hello", Filter: "group:one", Limit: 1, Since: cursor}, false)
	if second.Result.(map[string]any)["total"] != uint64(2) {
		t.Fatalf("page: %+v", second)
	}
	if _, err := f.run(OpSearch, Input{Query: "other", Filter: "group:one", Limit: 1, Since: cursor}, false); dbstore.Code(err) != "invalid_input" {
		t.Fatalf("query-bound cursor: %v", err)
	}
	f.must(OpPut, Input{ID: "d", Doc: json.RawMessage(`{"title":"hello sun","group":"one"}`)}, false)
	if _, err := f.run(OpSearch, Input{Query: "hello", Filter: "group:one", Limit: 1, Since: cursor}, false); dbstore.Code(err) != "invalid_input" {
		t.Fatalf("revision-bound cursor: %v", err)
	}
	if _, err := f.run(OpSearch, Input{Query: "hello", MaxBytes: 1}, false); dbstore.Code(err) != "limit_exceeded" {
		t.Fatalf("oversized first hit: %v", err)
	}
}

func TestBatchAtomicityAndMalformedImport(t *testing.T) {
	f := newFixture(t, dbstore.Memory)
	if _, err := f.run(OpBatch, Input{Documents: []DocumentEntry{{ID: "good", Doc: json.RawMessage(`{"text":"ok"}`)}, {ID: "bad", Doc: json.RawMessage(`[]`)}}}, false); dbstore.Code(err) != "invalid_input" {
		t.Fatalf("invalid batch: %v", err)
	}
	if _, err := f.run(OpGet, Input{ID: "good"}, false); dbstore.Code(err) != "not_found" {
		t.Fatalf("partial batch: %v", err)
	}
	f.must(OpPut, Input{ID: "keep", Doc: json.RawMessage(`{"text":"keep"}`)}, false)
	dir := filepath.Join(t.TempDir(), "export")
	f.must(OpExport, Input{Out: dir}, false)
	manifestPath := filepath.Join(dir, ManifestName)
	valid, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, bytes.Replace(valid, []byte(ExportSchema), []byte("bashy-fts-export-v9"), 1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(OpImport, Input{In: dir, Replace: true}, true); dbstore.Code(err) != "version_unsupported" {
		t.Fatalf("version: %v", err)
	}
	if err := os.WriteFile(manifestPath, valid, 0600); err != nil {
		t.Fatal(err)
	}
	recordsPath := filepath.Join(dir, RecordsName)
	if err := os.WriteFile(recordsPath, []byte("broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(OpImport, Input{In: dir, Replace: true}, true); dbstore.Code(err) != "malformed_input" {
		t.Fatalf("records: %v", err)
	}
	f.must(OpGet, Input{ID: "keep"}, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, _ := json.Marshal(Input{In: dir, Replace: true})
	if _, _, err := f.svc.RunCategory(ctx, f.target, OpImport, b, true, time.Second); dbstore.Code(err) != "cancelled" {
		t.Fatalf("cancel: %v", err)
	}
}

func TestMemoryOwnerLifetime(t *testing.T) {
	f := newFixture(t, dbstore.Memory)
	f.must(OpPut, Input{ID: "one", Doc: json.RawMessage(`{"text":"transient"}`)}, false)
	if err := f.svc.Mem.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(OpGet, Input{ID: "one"}, false); dbstore.Code(err) != "lifetime_ended" {
		t.Fatalf("closed owner: %v", err)
	}
}

func TestConcurrentMemoryRevisionConflict(t *testing.T) {
	f := newFixture(t, dbstore.Memory)
	f.must(OpPut, Input{ID: "same", Doc: json.RawMessage(`{"text":"base"}`)}, false)
	rev := f.must(OpGet, Input{ID: "same"}, false).Result.(map[string]any)["revision"].(string)
	var wg sync.WaitGroup
	codes := make(chan string, 2)
	for _, text := range []string{"first", "second"} {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			_, err := f.run(OpPut, Input{ID: "same", Doc: json.RawMessage(`{"text":"` + text + `"}`), Replace: true, IfRevision: rev}, false)
			codes <- dbstore.Code(err)
		}(text)
	}
	wg.Wait()
	close(codes)
	counts := map[string]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[""] != 1 || counts["conflict"] != 1 {
		t.Fatalf("concurrent updates: %+v", counts)
	}
}
