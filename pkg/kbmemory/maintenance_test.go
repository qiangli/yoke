package kbmemory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/kblife"
)

// modes runs fn against a persistent pair and an owner-held in-memory pair.
func maintModes(t *testing.T, fn func(t *testing.T, dir, doc, idx, session string)) {
	t.Run("persistent", func(t *testing.T) {
		dir := isolate(t)
		fn(t, dir, loc("sqlite:file:"+filepath.Join(dir, "mem.db")), loc("bleve:file:"+filepath.Join(dir, "mem.bleve")), "")
	})
	t.Run("memory", func(t *testing.T) {
		dir := isolate(t)
		s := owner(t, dir)
		fn(t, dir, loc("sqlite:mem:"+s+"/scratch"), loc("bleve:mem:"+s+"/rank"), "")
	})
}

func runMaintain(t *testing.T, req MaintainRequest) Envelope {
	t.Helper()
	env := Maintain(context.Background(), req)
	if !env.OK || env.SchemaVersion != MaintenanceSchema {
		t.Fatalf("maintain %s: %+v", req.Verb, env.Error)
	}
	return env
}

func maintainCode(t *testing.T, req MaintainRequest) string {
	t.Helper()
	env := Maintain(context.Background(), req)
	if env.OK || env.Error == nil || env.SchemaVersion != ErrorSchema {
		t.Fatalf("maintain %s: want an error envelope, got %+v", req.Verb, env)
	}
	return env.Error.Code
}

func revOf(t *testing.T, doc, session, id string) int64 {
	t.Helper()
	env := must(t, Request{Verb: "inspect", Target: doc, Session: session, ID: id})
	return *env.Revision
}

func reportOf(t *testing.T, env Envelope) CandidateReport {
	t.Helper()
	rep, ok := env.Result.(CandidateReport)
	if !ok {
		t.Fatalf("candidates result is %T", env.Result)
	}
	return rep
}

func hasCandidate(rep CandidateReport, id, reason string) bool {
	for _, c := range rep.Candidates {
		if c.ID == id && c.Reason == reason {
			return true
		}
	}
	return false
}

func TestMaintainCandidatesExpiredStaleDuplicate(t *testing.T) {
	maintModes(t, func(t *testing.T, dir, doc, idx, session string) {
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		pin(t, func() time.Time { return t0 })
		must(t, Request{Verb: "init", Target: doc, Session: session})
		a := addBody(t, doc, session, "wheat  harvest\nnotes", nil)
		pin(t, func() time.Time { return t0.Add(time.Hour) })
		b := addBody(t, doc, session, "wheat harvest notes", []string{"other"})
		c := addBody(t, doc, session, "unique rye note", nil)
		pin(t, func() time.Time { return t0.Add(2 * time.Hour) })
		e := addBody(t, doc, session, "short lived", nil)
		runMaintain(t, MaintainRequest{Verb: "expire", Target: doc, Session: session, ID: e.ID, IfRevision: revOf(t, doc, session, e.ID)})
		pin(t, func() time.Time { return t0.Add(3 * time.Hour) })

		rep := reportOf(t, runMaintain(t, MaintainRequest{Verb: "candidates", Target: doc, Session: session,
			StaleBefore: t0.Add(30 * time.Minute).Format(time.RFC3339)}))
		if rep.Records != 4 || rep.Counts[ReasonExpired] != 1 || rep.Counts[ReasonStale] != 1 || rep.Counts[ReasonDuplicate] != 1 {
			t.Fatalf("report = %+v", rep)
		}
		if !hasCandidate(rep, e.ID, ReasonExpired) || !hasCandidate(rep, a.ID, ReasonStale) || !hasCandidate(rep, b.ID, ReasonDuplicate) {
			t.Fatalf("candidates = %+v", rep.Candidates)
		}
		for _, cd := range rep.Candidates {
			if cd.ID == b.ID && cd.Of != a.ID {
				t.Fatalf("duplicate keeps the oldest record %s, got of=%s", a.ID, cd.Of)
			}
			if cd.ID == c.ID {
				t.Fatalf("unique record listed: %+v", cd)
			}
		}
		// Reading never changes anything and stale is opt-in.
		again := reportOf(t, runMaintain(t, MaintainRequest{Verb: "candidates", Target: doc, Session: session}))
		if again.Counts[ReasonStale] != 0 || again.Records != 4 {
			t.Fatalf("no --stale-before must report no stale: %+v", again)
		}
		if got := revOf(t, doc, session, a.ID); got != 1 {
			t.Fatalf("candidates wrote: revision %d", got)
		}
	})
}

func TestMaintainExpireRevisionAndState(t *testing.T) {
	maintModes(t, func(t *testing.T, dir, doc, idx, session string) {
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		pin(t, func() time.Time { return t0 })
		must(t, Request{Verb: "init", Target: doc, Session: session})
		r := addBody(t, doc, session, "expiring note", nil)
		pin(t, func() time.Time { return t0.Add(time.Hour) })
		if got := maintainCode(t, MaintainRequest{Verb: "expire", Target: doc, Session: session, ID: r.ID, IfRevision: 7}); got != "conflict" {
			t.Fatalf("stale revision = %s, want conflict", got)
		}
		env := runMaintain(t, MaintainRequest{Verb: "expire", Target: doc, Session: session, ID: r.ID, IfRevision: 1})
		if env.Revision == nil || *env.Revision != 2 {
			t.Fatalf("expire revision = %v", env.Revision)
		}
		got := record(t, must(t, Request{Verb: "inspect", Target: doc, Session: session, ID: r.ID}))
		if got.ValidTo != t0.Add(time.Hour).Format(time.RFC3339) || got.State(clock()) != kblife.StateExpired {
			t.Fatalf("expired record = %+v state %s", got, got.State(clock()))
		}
		if got.Source != r.Source || got.Created != r.Created {
			t.Fatalf("provenance changed: %+v vs %+v", got.Source, r.Source)
		}
		if c := maintainCode(t, MaintainRequest{Verb: "expire", Target: doc, Session: session, ID: r.ID, IfRevision: 2}); c != "invalid_input" {
			t.Fatalf("expiring an expired record = %s", c)
		}
		// The flag matrix refuses misplaced and missing inputs before acting.
		for _, bad := range []MaintainRequest{
			{Verb: "expire", Target: doc, Session: session, ID: r.ID},
			{Verb: "candidates", Target: doc, Session: session, ID: r.ID},
			{Verb: "candidates", Target: doc, Session: session, Out: "x"},
			{Verb: "delete", Target: doc, Session: session, ID: r.ID, IfRevision: 2},
			{Verb: "restore", Target: doc, Session: session, In: "x"},
			{Verb: "bogus", Target: doc},
			{Verb: "candidates"},
		} {
			if c := maintainCode(t, bad); c != "invalid_input" {
				t.Fatalf("%+v = %s, want invalid_input", bad, c)
			}
		}
	})
}

func TestMaintainSupersedeViaCaptureWithIndex(t *testing.T) {
	maintModes(t, func(t *testing.T, dir, doc, idx, session string) {
		pair(t, doc, idx, session)
		old := addBody(t, doc, session, "old wheat guidance", nil)
		rep := addBody(t, doc, session, "new wheat guidance", nil)
		mustSync(t, syncQ(doc, idx, session))
		env := runMaintain(t, MaintainRequest{Verb: "supersede", Target: doc, Session: session, Index: idx, ID: old.ID,
			IfRevision: 1, Replacement: rep.ID, ReplacementRevision: 1})
		m := env.Result.(map[string]any)
		if m["committed"] != true || m["index"] == nil || m["index_error"] != nil {
			t.Fatalf("supersede result = %+v", m)
		}
		res := mustRetrieve(t, query(doc, idx, session))
		if len(res.Hits) != 1 || res.Hits[0].Ref != rep.ID || res.Freshness.Stale {
			t.Fatalf("recall after supersede = %+v", res)
		}
		got := record(t, must(t, Request{Verb: "inspect", Target: doc, Session: session, ID: old.ID}))
		if got.SupersededBy != rep.ID {
			t.Fatalf("old = %+v", got)
		}
	})
}

func TestMaintainDeletePropagatesToIndex(t *testing.T) {
	maintModes(t, func(t *testing.T, dir, doc, idx, session string) {
		pair(t, doc, idx, session)
		keep := addBody(t, doc, session, "keep wheat", nil)
		gone := addBody(t, doc, session, "delete wheat", nil)
		mustSync(t, syncQ(doc, idx, session))

		if c := maintainCode(t, MaintainRequest{Verb: "delete", Target: doc, Session: session, ID: gone.ID, IfRevision: 1}); c != "invalid_input" {
			t.Fatalf("delete without --yes = %s", c)
		}
		if c := maintainCode(t, MaintainRequest{Verb: "delete", Target: doc, Session: session, ID: gone.ID, IfRevision: 3, Yes: true}); c != "conflict" {
			t.Fatalf("delete stale revision = %s", c)
		}
		env := runMaintain(t, MaintainRequest{Verb: "delete", Target: doc, Session: session, Index: idx, ID: gone.ID, IfRevision: 1, Yes: true})
		m := env.Result.(map[string]any)
		if m["deleted"] != true || m["committed"] != true {
			t.Fatalf("delete result = %+v", m)
		}
		var sr SyncResult
		b, _ := json.Marshal(m["index"])
		_ = json.Unmarshal(b, &sr)
		if sr.Deleted != 1 || sr.IndexDocuments != 1 {
			t.Fatalf("index sync = %+v", sr)
		}
		if c := code(t, Request{Verb: "inspect", Target: doc, Session: session, ID: gone.ID}); c != "not_found" {
			t.Fatalf("deleted record inspect = %s", c)
		}
		res := mustRetrieve(t, query(doc, idx, session))
		if len(res.Hits) != 1 || res.Hits[0].Ref != keep.ID || res.Freshness.Stale || res.Freshness.IndexDocuments != 1 {
			t.Fatalf("recall after delete = %+v", res)
		}
	})
}

// TestMaintainInterruptedIndexingRecovers: the delete commits, the explicit
// index sync cannot run (the index target does not exist yet), recall never
// serves the deleted record, and a later explicit sync converges.
func TestMaintainInterruptedIndexingRecovers(t *testing.T) {
	maintModes(t, func(t *testing.T, dir, doc, idx, session string) {
		pair(t, doc, idx, session)
		keep := addBody(t, doc, session, "keep wheat", nil)
		gone := addBody(t, doc, session, "delete wheat", nil)
		mustSync(t, syncQ(doc, idx, session))

		// Interrupt 1: the sync is skipped entirely (no --index).
		runMaintain(t, MaintainRequest{Verb: "delete", Target: doc, Session: session, ID: gone.ID, IfRevision: 1, Yes: true})
		res := mustRetrieve(t, query(doc, idx, session))
		if len(res.Hits) != 1 || res.Hits[0].Ref != keep.ID || !res.Freshness.Stale || res.Freshness.IndexDocuments != 2 {
			t.Fatalf("recall with an unsynced delete = %+v", res)
		}
		// Interrupt 2: the sync starts but is cancelled.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if env := SyncIndex(ctx, syncQ(doc, idx, session)); env.OK {
			t.Fatalf("cancelled sync succeeded")
		}
		if res := mustRetrieve(t, query(doc, idx, session)); !res.Freshness.Stale {
			t.Fatalf("a cancelled sync left the index looking fresh")
		}
		// Recovery: the same explicit pipeline converges.
		if sr := mustSync(t, syncQ(doc, idx, session)); sr.Deleted != 1 || sr.IndexDocuments != 1 {
			t.Fatalf("recovery sync = %+v", sr)
		}
		if res := mustRetrieve(t, query(doc, idx, session)); res.Freshness.Stale || len(res.Hits) != 1 {
			t.Fatalf("recall after recovery = %+v", res)
		}
	})
}

func TestMaintainIndexFailureAfterCommitIsReportedNotRetried(t *testing.T) {
	maintModes(t, func(t *testing.T, dir, doc, idx, session string) {
		must(t, Request{Verb: "init", Target: doc, Session: session})
		gone := addBody(t, doc, session, "delete wheat", nil)
		// The rank index was never created: the sync fails after the commit.
		env := runMaintain(t, MaintainRequest{Verb: "delete", Target: doc, Session: session, Index: idx, ID: gone.ID, IfRevision: 1, Yes: true})
		m := env.Result.(map[string]any)
		if m["committed"] != true || m["index_error"] == nil || m["recover"] == nil || m["index"] != nil {
			t.Fatalf("result = %+v", m)
		}
		if c := code(t, Request{Verb: "inspect", Target: doc, Session: session, ID: gone.ID}); c != "not_found" {
			t.Fatalf("the delete did not commit: %s", c)
		}
		mkIndex(t, idx, session)
		if sr := mustSync(t, syncQ(doc, idx, session)); sr.IndexDocuments != 0 {
			t.Fatalf("recovery sync = %+v", sr)
		}
	})
}

// sibling names a second target next to loc in the same storage mode.
func sibling(l string) string {
	if strings.Contains(l, "mem.") {
		return strings.Replace(l, "mem.", "memcopy.", 1)
	}
	return l + "copy"
}

func mustTarget(t *testing.T, l string) dbstore.Target {
	t.Helper()
	tt, err := dblifecycle.ParseTarget(l)
	if err != nil {
		t.Fatal(err)
	}
	return tt
}

func recordsOf(t *testing.T, doc, session string) map[string]Record {
	t.Helper()
	snap, err := snapshotSource(context.Background(), RetrieveQuery{Target: doc, Session: session}, mustTarget(t, doc), MaxSyncRecords, MaxSyncBytes)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Record{}
	for id, d := range snap {
		var r Record
		if err := json.Unmarshal(d.raw, &r); err != nil {
			t.Fatal(err)
		}
		out[id] = r
	}
	return out
}

func TestMaintainExportImportRoundTrip(t *testing.T) {
	maintModes(t, func(t *testing.T, dir, doc, idx, session string) {
		pair(t, doc, idx, session)
		a := addBody(t, doc, session, "first wheat", []string{"x"})
		b := addBody(t, doc, session, "second wheat", nil)
		must(t, Request{Verb: "forget", Target: doc, Session: session, ID: b.ID, IfRevision: 1, Reason: "obsolete"})
		before := recordsOf(t, doc, session)

		exp := filepath.Join(dir, "exp")
		runMaintain(t, MaintainRequest{Verb: "export", Target: doc, Session: session, Out: exp})
		if c := maintainCode(t, MaintainRequest{Verb: "export", Target: doc, Session: session, Out: exp}); c != "exists" {
			t.Fatalf("export over an existing directory = %s", c)
		}

		// Into a second target: IDs, provenance and lifecycle arrive verbatim.
		doc2, idx2 := sibling(doc), sibling(idx)
		pair(t, doc2, idx2, session)
		runMaintain(t, MaintainRequest{Verb: "import", Target: doc2, Session: session, Index: idx2, In: exp})
		if after := recordsOf(t, doc2, session); !reflect.DeepEqual(before, after) {
			t.Fatalf("import changed records:\n%+v\n%+v", before, after)
		}
		if got := mustRetrieve(t, query(doc2, idx2, session)); len(got.Hits) != 1 || got.Hits[0].Ref != a.ID || got.Freshness.Stale {
			t.Fatalf("recall on the imported copy = %+v", got)
		}
		if c := maintainCode(t, MaintainRequest{Verb: "import", Target: doc2, Session: session, In: exp}); c == "" {
			t.Fatal("conflicting import must fail")
		}
		runMaintain(t, MaintainRequest{Verb: "import", Target: doc2, Session: session, In: exp, OnConflict: "skip"})

		// Replace import empties the target first and the index follows.
		extra := addBody(t, doc2, session, "extra only in copy", nil)
		mustSync(t, syncQ(doc2, idx2, session))
		if c := maintainCode(t, MaintainRequest{Verb: "import", Target: doc2, Session: session, In: exp, Replace: true}); c != "invalid_input" {
			t.Fatalf("replace without --yes = %s", c)
		}
		runMaintain(t, MaintainRequest{Verb: "import", Target: doc2, Session: session, Index: idx2, In: exp, Replace: true, Yes: true})
		if after := recordsOf(t, doc2, session); !reflect.DeepEqual(before, after) {
			t.Fatalf("replace import: %+v", after)
		}
		if _, ok := recordsOf(t, doc2, session)[extra.ID]; ok {
			t.Fatal("replace import kept the extra record")
		}
		if got := mustRetrieve(t, query(doc2, idx2, session)); got.Freshness.Stale || got.Freshness.IndexDocuments != 2 {
			t.Fatalf("index after replace import = %+v", got.Freshness)
		}
	})
}

func TestMaintainImportRefusesForeignExport(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	must(t, Request{Verb: "init", Target: doc})
	bad := filepath.Join(dir, "bad")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if c := maintainCode(t, MaintainRequest{Verb: "import", Target: doc, In: bad}); c != "not_found" {
		t.Fatalf("no manifest = %s", c)
	}
	if err := os.WriteFile(filepath.Join(bad, "manifest.json"), []byte(`{"schema_def":{"collections":[{"name":"other"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := maintainCode(t, MaintainRequest{Verb: "import", Target: doc, In: bad}); c != "invalid_input" {
		t.Fatalf("foreign collection = %s", c)
	}
}

func TestMaintainBackupRestoreRoundTrip(t *testing.T) {
	maintModes(t, func(t *testing.T, dir, doc, idx, session string) {
		pair(t, doc, idx, session)
		a := addBody(t, doc, session, "backed up wheat", []string{"x"})
		before := recordsOf(t, doc, session)
		bk := filepath.Join(dir, "backup.db")
		runMaintain(t, MaintainRequest{Verb: "backup", Target: doc, Session: session, Out: bk})
		if c := maintainCode(t, MaintainRequest{Verb: "backup", Target: doc, Session: session, Out: bk}); c != "exists" {
			t.Fatalf("backup over an existing file = %s", c)
		}
		// Post-backup drift: a delete of the original and a new record.
		runMaintain(t, MaintainRequest{Verb: "delete", Target: doc, Session: session, ID: a.ID, IfRevision: 1, Yes: true})
		addBody(t, doc, session, "post backup", nil)
		mustSync(t, syncQ(doc, idx, session))

		if c := maintainCode(t, MaintainRequest{Verb: "restore", Target: doc, Session: session, In: bk}); c != "invalid_input" {
			t.Fatalf("restore without --yes = %s", c)
		}
		env := runMaintain(t, MaintainRequest{Verb: "restore", Target: doc, Session: session, Index: idx, In: bk, Yes: true})
		m := env.Result.(map[string]any)
		if m["verified"] == nil || m["index"] == nil {
			t.Fatalf("restore result = %+v", m)
		}
		if after := recordsOf(t, doc, session); !reflect.DeepEqual(before, after) {
			t.Fatalf("restore: %+v", after)
		}
		got := mustRetrieve(t, query(doc, idx, session))
		if len(got.Hits) != 1 || got.Hits[0].Ref != a.ID || got.Freshness.Stale {
			t.Fatalf("recall after restore = %+v", got)
		}
	})
}

func TestMaintainCancelledIsBounded(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	must(t, Request{Verb: "init", Target: doc})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env := Maintain(ctx, MaintainRequest{Verb: "candidates", Target: doc})
	if env.OK || env.Error == nil || env.ExitCode() == 0 {
		t.Fatalf("cancelled candidates = %+v", env)
	}
}

func TestMaintainTargetSafetyRefusals(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	must(t, Request{Verb: "init", Target: doc})
	a := addBody(t, doc, "", "keep me", nil)
	before := recordsOf(t, doc, "")

	// A foreign collection keeps export and backup from leaving through kb memory.
	foreign := loc("sqlite:file:" + filepath.Join(dir, "foreign.db"))
	must(t, Request{Verb: "init", Target: foreign})
	if _, err := docOp(context.Background(), Request{Target: foreign}, mustTarget(t, foreign), "doc.collection.create", map[string]any{"collection": "secrets"}, true); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "leak")
	if c := maintainCode(t, MaintainRequest{Verb: "export", Target: foreign, Out: out}); c != "invalid_input" {
		t.Fatalf("export with foreign collection = %s", c)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("refused export wrote a directory")
	}
	bk := filepath.Join(dir, "foreign.bak")
	if c := maintainCode(t, MaintainRequest{Verb: "backup", Target: foreign, Out: bk}); c != "invalid_input" {
		t.Fatalf("backup with foreign collection = %s", c)
	}
	if _, err := os.Stat(bk); err == nil {
		t.Fatal("refused backup published a file")
	}
	if c := maintainCode(t, MaintainRequest{Verb: "backup", Target: doc, Out: filepath.Join(dir, "x.bak"), RequestID: "r1"}); c != "unsupported" {
		t.Fatalf("backup --request-id = %s", c)
	}

	// Restore validates the snapshot before replacing anything.
	snap := filepath.Join(dir, "foreign.snap")
	if _, err := (dblifecycle.Service{}).Backup(context.Background(), mustTarget(t, foreign), snap, 0); err != nil {
		t.Fatal(err)
	}
	if c := maintainCode(t, MaintainRequest{Verb: "restore", Target: doc, In: snap, Yes: true}); c != "invalid_input" {
		t.Fatalf("restore of foreign snapshot = %s", c)
	}
	if after := recordsOf(t, doc, ""); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused restore mutated the target: %+v", after)
	}

	// Import validates memory records, not the collection name alone.
	bad := filepath.Join(dir, "exp")
	runMaintain(t, MaintainRequest{Verb: "export", Target: doc, Out: bad})
	rf := filepath.Join(bad, "records-000000.jsonl")
	b, err := os.ReadFile(rf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rf, []byte(strings.Replace(string(b), `"id":"`+a.ID+`","revision"`, `"id":"notmem","revision"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	target2 := loc("sqlite:file:" + filepath.Join(dir, "other.db"))
	must(t, Request{Verb: "init", Target: target2})
	if env := Maintain(context.Background(), MaintainRequest{Verb: "import", Target: target2, In: bad}); env.OK {
		t.Fatal("import of a record with a non-memory id succeeded")
	}
	if got := recordsOf(t, target2, ""); len(got) != 0 {
		t.Fatalf("refused import wrote %+v", got)
	}
	big := filepath.Join(dir, "big")
	if err := os.Mkdir(big, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(big, "manifest.json"), make([]byte, maxManifestBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if env := Maintain(context.Background(), MaintainRequest{Verb: "import", Target: target2, In: big}); env.OK {
		t.Fatal("oversized manifest accepted")
	}
}

func TestMaintainRestoreRefusesTruncatedValidation(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	must(t, Request{Verb: "init", Target: doc})
	addBody(t, doc, "", "keep me", nil)
	before := recordsOf(t, doc, "")

	src := loc("sqlite:file:" + filepath.Join(dir, "src.db"))
	must(t, Request{Verb: "init", Target: src})
	for _, b := range []string{"one", "two", "three"} {
		addBody(t, src, "", b, nil)
	}
	snap := filepath.Join(dir, "src.snap")
	runMaintain(t, MaintainRequest{Verb: "backup", Target: src, Out: snap})
	old := snapshotRecordCap
	snapshotRecordCap = 2
	defer func() { snapshotRecordCap = old }()
	if c := maintainCode(t, MaintainRequest{Verb: "restore", Target: doc, In: snap, Yes: true}); c != "limit_exceeded" {
		t.Fatalf("restore beyond validation cap = %s", c)
	}
	if after := recordsOf(t, doc, ""); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused restore mutated the target: %+v", after)
	}
}

func TestMaintainImportStagesSameBytes(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	session := ""
	must(t, Request{Verb: "init", Target: doc, Session: session})
	addBody(t, doc, session, "staged wheat", nil)
	exp := filepath.Join(dir, "exp")
	runMaintain(t, MaintainRequest{Verb: "export", Target: doc, Session: session, Out: exp})

	staged := filepath.Join(dir, "staged")
	if err := stageImportDir(context.Background(), exp, staged); err != nil {
		t.Fatal(err)
	}
	if err := checkImportDir(context.Background(), staged); err != nil {
		t.Fatal(err)
	}
	// Replace the source after staging with a foreign manifest and garbage records.
	if err := os.WriteFile(filepath.Join(exp, "manifest.json"), []byte(`{"schema_def":{"collections":[{"name":"other"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(exp)
	for _, e := range ents {
		if e.Name() != "manifest.json" {
			os.WriteFile(filepath.Join(exp, e.Name()), []byte("garbage\n"), 0o600)
		}
	}
	if err := checkImportDir(context.Background(), staged); err != nil {
		t.Fatalf("staged copy changed with its source: %v", err)
	}
	if c := maintainCode(t, MaintainRequest{Verb: "import", Target: doc, Session: session, In: exp}); c == "" {
		t.Fatal("replaced source must be refused")
	}
}
