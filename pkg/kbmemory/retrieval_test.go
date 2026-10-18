package kbmemory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbdoc"
	"github.com/qiangli/yoke/pkg/dbfts"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/spf13/cobra"
)

// This file covers Wave2 lexical retrieval (story a566e6387674) with short
// filtered in-process tests: the shared read pipeline, the explicit index
// sync this story owns, and the no-mutation-on-recall boundary between them.

func mustRetrieve(t *testing.T, q RetrieveQuery) RetrieveResult {
	t.Helper()
	env := Retrieve(context.Background(), q)
	if !env.OK || env.SchemaVersion != RetrievalSchema || env.Op != RetrievalOp {
		t.Fatalf("retrieve: %+v", env.Error)
	}
	res, ok := env.Result.(RetrieveResult)
	if !ok {
		t.Fatalf("retrieve result is %T, want RetrieveResult", env.Result)
	}
	return res
}

func retrieveCode(t *testing.T, q RetrieveQuery) string {
	t.Helper()
	env := Retrieve(context.Background(), q)
	if env.OK || env.Error == nil || env.SchemaVersion != ErrorSchema {
		t.Fatalf("retrieve: want an error envelope, got %+v", env.Result)
	}
	return env.Error.Code
}

func mustSync(t *testing.T, q SyncQuery) SyncResult {
	t.Helper()
	env := SyncIndex(context.Background(), q)
	if !env.OK || env.SchemaVersion != RetrievalSchema || env.Op != SyncOp {
		t.Fatalf("sync: %+v", env.Error)
	}
	res, ok := env.Result.(SyncResult)
	if !ok {
		t.Fatalf("sync result is %T, want SyncResult", env.Result)
	}
	return res
}

func syncCode(t *testing.T, q SyncQuery) string {
	t.Helper()
	env := SyncIndex(context.Background(), q)
	if env.OK || env.Error == nil || env.SchemaVersion != ErrorSchema {
		t.Fatalf("sync: want an error envelope, got %+v", env.Result)
	}
	return env.Error.Code
}

// mkIndex creates the bleve rank target the sync reconciles. The capture
// side owns no FTS target, so tests create it explicitly, as operators do.
func mkIndex(t *testing.T, target, session string) {
	t.Helper()
	tt, err := dblifecycle.ParseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A memory target is created through its owning session (its own for a
	// mem locator, an explicit shared owner for a persistent target), the
	// same admission the capture verbs use.
	by := session
	if tt.Mode == dbstore.Memory {
		by = tt.Session
	}
	if by != "" {
		res, err := dblifecycle.Call(ctx, by, dblifecycle.Request{Op: "create", Target: tt, Wait: 5 * time.Second})
		if err != nil || !res.OK {
			t.Fatalf("create index: %+v %v", res, err)
		}
		return
	}
	if _, err := (dblifecycle.Service{}).Create(ctx, tt, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func addBody(t *testing.T, target, session, body string, tags []string) Record {
	t.Helper()
	req := Request{Verb: "add", Target: target, Session: session, Body: &body}
	if tags != nil {
		req.Tags = &tags
	}
	return record(t, must(t, req))
}

// pair wires one memory collection with its rank index in either mode.
func pair(t *testing.T, doc, idx, session string) {
	t.Helper()
	must(t, Request{Verb: "init", Target: doc, Session: session})
	mkIndex(t, idx, session)
}

func query(doc, idx, session string) RetrieveQuery {
	return RetrieveQuery{Target: doc, Index: idx, Session: session, Limit: 10}
}

func syncQ(doc, idx, session string) SyncQuery {
	return SyncQuery{Target: doc, Index: idx, Session: session}
}

// refOf is the test-side mirror of Hit.Ref: the stable record ID
// verbatim. The canonical source target is asserted separately in the
// freshness report and the envelope target; a record is named by the
// tuple (target, collection, ID) with no parsed locator syntax.
func refOf(_ *testing.T, _ string, id string) string { return id }

// TestRetrieveLexicalRoundTrip is the whole Wave2 path: capture, explicit
// sync, then recall serving stable refs, scores and the actual mode with a
// fresh population report.
func TestRetrieveLexicalRoundTrip(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	a := addBody(t, doc, "", "the quick wheat field", []string{"farm"})
	b := addBody(t, doc, "", "a slow river barge", []string{"river"})

	got := mustSync(t, syncQ(doc, idx, ""))
	if got.SourceDocuments != 2 || got.Added != 2 || got.IndexDocuments != 2 {
		t.Fatalf("sync = %+v, want 2 added of 2", got)
	}
	// A second sync is a no-op: the export re-encoding is not an update.
	again := mustSync(t, syncQ(doc, idx, ""))
	if again.Added != 0 || again.Updated != 0 || again.Deleted != 0 {
		t.Fatalf("resync = %+v, want no writes", again)
	}

	res := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, a.ID) || res.Hits[0].Mode != ModeLexical ||
		res.Hits[0].Record.Body != "the quick wheat field" || res.Hits[0].Revision != 1 {
		t.Fatalf("recall = %+v", res.Hits)
	}
	if res.Mode != ModeLexical {
		t.Fatalf("mode = %q, want lexical", res.Mode)
	}
	if f := res.Freshness; f.SourceDocuments != 2 || f.IndexDocuments != 2 || f.Stale || f.DroppedStale != 0 {
		t.Fatalf("freshness = %+v, want 2/2 fresh", f)
	}

	tagged := query(doc, idx, "")
	tagged.Tags = []string{"river"}
	res = mustRetrieve(t, tagged)
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, b.ID) {
		t.Fatalf("tag filter = %+v", res.Hits)
	}
}

// TestRetrieveUnsupportedModes pins the Wave4 boundary: semantic and hybrid
// name the missing embedding backend instead of substituting lexical.
func TestRetrieveUnsupportedModes(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	for _, mode := range []string{ModeSemantic, ModeHybrid} {
		q := query(doc, idx, "")
		q.Mode = mode
		if c := retrieveCode(t, q); c != "unsupported" {
			t.Fatalf("mode %s = %s, want unsupported", mode, c)
		}
	}
	q := query(doc, idx, "")
	q.Mode = "vibes"
	if c := retrieveCode(t, q); c != "invalid_input" {
		t.Fatalf("mode vibes = %s, want invalid_input", c)
	}
}

// TestRetrieveNeverProvisions pins the read boundary: recall on a missing
// index fails naming it and creates nothing on disk.
func TestRetrieveNeverProvisions(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "missing.bleve"))
	must(t, Request{Verb: "init", Target: doc})
	addBody(t, doc, "", "a body worth ranking", nil)
	if c := retrieveCode(t, query(doc, idx, "")); c != "not_found" {
		t.Fatalf("recall on missing index = %s, want not_found", c)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "missing*")); len(entries) != 0 {
		t.Fatalf("recall created %v", entries)
	}
}

// TestSyncOwnsFreshness covers the manager correction: a record added after
// the sync shows up as a stale count gap (new unindexed source records, not
// just hit differences); an update marks its hit stale; a deleted source
// record is dropped from recall and then deleted from the index by sync.
func TestSyncOwnsFreshness(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	a := addBody(t, doc, "", "first wheat note", nil)
	addBody(t, doc, "", "second wheat note", nil)
	mustSync(t, syncQ(doc, idx, ""))

	fresh := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10})
	if fresh.Freshness.Stale {
		t.Fatalf("freshness = %+v, want fresh", fresh.Freshness)
	}

	addBody(t, doc, "", "third wheat note, never indexed", nil)
	gap := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10})
	if f := gap.Freshness; !f.Stale || f.SourceDocuments != 3 || f.IndexDocuments != 2 {
		t.Fatalf("freshness = %+v, want stale 3/2 gap", f)
	}

	must(t, Request{Verb: "update", Target: doc, ID: a.ID, IfRevision: 1, Body: str("first wheat note, revised")})
	rev := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "revised"})
	if len(rev.Hits) != 0 {
		t.Fatalf("stale index unexpectedly ranks the revision: %+v", rev.Hits)
	}
	got := mustSync(t, syncQ(doc, idx, ""))
	if got.Added != 1 || got.Updated != 1 || got.IndexDocuments != 3 {
		t.Fatalf("sync = %+v, want +1 ~1 of 3", got)
	}
	after := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "revised"})
	if len(after.Hits) != 1 || after.Hits[0].Ref != refOf(t, doc, a.ID) || after.Hits[0].Revision != 2 || after.Hits[0].Stale {
		t.Fatalf("recall after sync = %+v", after.Hits)
	}
	if after.Freshness.Stale {
		t.Fatalf("freshness = %+v, want fresh after sync", after.Freshness)
	}

	// Deletion propagates: the source record goes first, recall drops the
	// hit, and the next sync deletes the index entry.
	docT, err := dblifecycle.ParseTarget(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docOp(context.Background(), Request{}, docT, dbdoc.OpRm,
		dbdoc.Input{Collection: Collection, ID: a.ID}, false); err != nil {
		t.Fatal(err)
	}
	dropped := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10})
	if f := dropped.Freshness; !f.Stale || f.DroppedStale != 1 || f.SourceDocuments != 2 || f.IndexDocuments != 3 {
		t.Fatalf("freshness = %+v, want stale with 1 dropped", f)
	}
	gone := refOf(t, doc, a.ID)
	for _, h := range dropped.Hits {
		if h.Ref == gone {
			t.Fatalf("deleted record still served: %+v", h)
		}
	}
	del := mustSync(t, syncQ(doc, idx, ""))
	if del.Deleted != 1 || del.IndexDocuments != 2 {
		t.Fatalf("sync = %+v, want 1 deleted of 2", del)
	}
	clean := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10})
	if clean.Freshness.Stale {
		t.Fatalf("freshness = %+v, want fresh after delete sync", clean.Freshness)
	}
}

// TestRetrieveFreshnessFingerprints covers the mb6698/mb6716 correction:
// staleness rests on bounded source/index snapshot fingerprints, never on
// counts. An edit to a record outside the query leaves the served hits
// identical yet reads stale, and a same-count delete-plus-add keeps every
// count at 2/2 on both sides while the revisions disagree.
func TestRetrieveFreshnessFingerprints(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	a := addBody(t, doc, "", "alpha wheat note", nil)
	b := addBody(t, doc, "", "beta wheat note", nil)
	mustSync(t, syncQ(doc, idx, ""))

	base := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "alpha"})
	if base.Freshness.Stale || len(base.Hits) != 1 || base.Hits[0].Ref != refOf(t, doc, a.ID) ||
		base.Freshness.SourceRevision != base.Freshness.IndexRevision {
		t.Fatalf("baseline = %+v", base.Freshness)
	}

	// Same-count edit outside the query: the served hit is unchanged, but
	// the source fingerprint moved.
	must(t, Request{Verb: "update", Target: doc, ID: b.ID, IfRevision: 1, Body: str("beta wheat note, revised")})
	edited := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "alpha"})
	if len(edited.Hits) != 1 || edited.Hits[0].Ref != refOf(t, doc, a.ID) {
		t.Fatalf("hits moved = %+v", edited.Hits)
	}
	if f := edited.Freshness; !f.Stale || f.SourceRevision == f.IndexRevision ||
		f.SourceDocuments != 2 || f.IndexDocuments != 2 {
		t.Fatalf("freshness = %+v, want stale with equal 2/2 counts", f)
	}

	mustSync(t, syncQ(doc, idx, ""))

	// Same-count delete-plus-add: 2/2 on both sides, revisions disagree.
	docT, err := dblifecycle.ParseTarget(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docOp(context.Background(), Request{}, docT, dbdoc.OpRm,
		dbdoc.Input{Collection: Collection, ID: a.ID}, false); err != nil {
		t.Fatal(err)
	}
	addBody(t, doc, "", "gamma wheat note", nil)
	swapped := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10})
	if f := swapped.Freshness; !f.Stale || f.SourceRevision == f.IndexRevision ||
		f.SourceDocuments != 2 || f.IndexDocuments != 2 {
		t.Fatalf("freshness = %+v, want stale with equal 2/2 counts", f)
	}
	swappedGone := refOf(t, doc, a.ID)
	for _, h := range swapped.Hits {
		if h.Ref == swappedGone {
			t.Fatalf("deleted record still served: %+v", h)
		}
	}
	final := mustSync(t, syncQ(doc, idx, ""))
	if final.Deleted != 1 || final.Added != 1 || final.IndexDocuments != 2 {
		t.Fatalf("sync = %+v, want -1 +1 of 2", final)
	}
	clean := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10})
	if clean.Freshness.Stale || clean.Freshness.SourceRevision != clean.Freshness.IndexRevision {
		t.Fatalf("freshness = %+v, want fresh after sync", clean.Freshness)
	}
}

// TestSyncRefusesPartial pins the bound: a snapshot past the caps fails
// before any index write, so retrieval never ranks a half-built index.
func TestSyncRefusesPartial(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	addBody(t, doc, "", "one wheat", nil)
	addBody(t, doc, "", "two wheat", nil)
	q := syncQ(doc, idx, "")
	q.Limit = 1
	if c := syncCode(t, q); c != "limit_exceeded" {
		t.Fatalf("oversized sync = %s, want limit_exceeded", c)
	}
	res := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10})
	if res.Freshness.IndexDocuments != 0 {
		t.Fatalf("refused sync wrote the index: %+v", res.Freshness)
	}
}

// TestRetrieveMemoryModeThroughOwner runs the same round trip against an
// in-memory collection and index behind one explicit owner session.
func TestRetrieveMemoryModeThroughOwner(t *testing.T) {
	dir := isolate(t)
	s := owner(t, dir)
	doc := loc("sqlite:mem:" + s + "/scratch")
	idx := loc("bleve:mem:" + s + "/rank")
	pair(t, doc, idx, "")
	a := addBody(t, doc, "", "ephemeral wheat", []string{"x"})
	mustSync(t, syncQ(doc, idx, ""))
	res := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, a.ID) || res.Freshness.Stale {
		t.Fatalf("memory recall = %+v", res)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "*.db")); len(entries) != 0 {
		t.Fatalf("memory mode wrote %v", entries)
	}
}

// TestRetrieveCLIAndMCPParity runs the new constructors through both doors:
// the Cobra commands and the MCP tools share one parser and one envelope.
func TestRetrieveCLIAndMCPParity(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	a := addBody(t, doc, "", "parity wheat", nil)

	out, errOut, exit := MCPSyncIndexCommand().Run(context.Background(), []string{"--json", doc, "--index", idx}, "", dir)
	if exit != 0 || !strings.Contains(out, `"op":"memory.index.sync"`) || !strings.Contains(out, RetrievalSchema) {
		t.Fatalf("mcp sync: %d %s %s", exit, out, errOut)
	}
	mcpOut, _, exit := MCPRetrieveCommand().Run(context.Background(), []string{"--json", doc, "--index", idx, "--text", "wheat"}, "", dir)
	if exit != 0 {
		t.Fatalf("mcp retrieve: %d %s", exit, mcpOut)
	}
	cmd := newRetrieveCmd(context.Background, dir)
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{doc, "--index", idx, "--text", "wheat", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != mcpOut {
		t.Fatalf("cli/mcp differ:\ncli: %s\nmcp: %s", buf.String(), mcpOut)
	}
	var env Envelope
	var res RetrieveResult
	if err := json.Unmarshal([]byte(mcpOut), &env); err != nil {
		t.Fatal(err)
	}
	rb, _ := json.Marshal(env.Result)
	if err := json.Unmarshal(rb, &res); err != nil || len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, a.ID) {
		t.Fatalf("parity recall = %s (%v)", mcpOut, err)
	}
}

// TestRetrieveFiltersScopeTimeAndCancel pins the remaining query clauses:
// repos/OS scoping, the since/until window, truncation reporting, and a
// cancelled read that stays cancelled.
func TestRetrieveFiltersScopeTimeAndCancel(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	fixed := time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC)
	pin(t, func() time.Time { return fixed })
	must(t, Request{Verb: "add", Target: doc, Body: str("old wheat"), Repos: &[]string{"yoke"}, OS: str("linux")})
	pin(t, func() time.Time { return fixed.Add(48 * time.Hour) })
	must(t, Request{Verb: "add", Target: doc, Body: str("new wheat"), Repos: &[]string{"bashy"}})
	mustSync(t, syncQ(doc, idx, ""))

	repo := query(doc, idx, "")
	repo.Repos = []string{"yoke"}
	if res := mustRetrieve(t, repo); len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Record.Body, "old") {
		t.Fatalf("repos filter = %+v", res.Hits)
	}
	osQ := query(doc, idx, "")
	osQ.OS = "darwin"
	if res := mustRetrieve(t, osQ); len(res.Hits) != 0 {
		t.Fatalf("os filter = %+v", res.Hits)
	}
	since := query(doc, idx, "")
	since.Since = "2026-10-19T00:00:00Z"
	if res := mustRetrieve(t, since); len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Record.Body, "new") {
		t.Fatalf("since filter = %+v", res.Hits)
	}
	if q := query(doc, idx, ""); func() string {
		q.Since = "2026-10-20T00:00:00Z"
		q.Until = "2026-10-19T00:00:00Z"
		return retrieveCode(t, q)
	}() != "invalid_input" {
		t.Fatal("inverted window was accepted")
	}
	lim := query(doc, idx, "")
	lim.Limit = 1
	if res := mustRetrieve(t, lim); len(res.Hits) != 1 || !res.Freshness.Truncated {
		t.Fatalf("limit = %+v", res)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env := Retrieve(ctx, query(doc, idx, ""))
	if env.OK || env.Error == nil || env.Error.Code != "cancelled" {
		t.Fatalf("cancelled recall = %+v", env)
	}
}

// TestRetrieveSortsByScoreBestN pins the mb13:39 correction: dbfts orders
// _id, so the op queries at the max bounded population and sorts by score
// descending (stable by record ID) before the requested limit. Hits arrive
// in non-increasing score order, and limit 1 serves the top score.
func TestRetrieveSortsByScoreBestN(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	addBody(t, doc, "", "wheat", nil)
	addBody(t, doc, "", "wheat wheat", nil)
	addBody(t, doc, "", "wheat wheat wheat wheat", nil)
	mustSync(t, syncQ(doc, idx, ""))

	res := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if len(res.Hits) != 3 {
		t.Fatalf("recall = %+v", res.Hits)
	}
	for i := 1; i < len(res.Hits); i++ {
		if res.Hits[i].Score > res.Hits[i-1].Score {
			t.Fatalf("hits not score-ordered: %+v", res.Hits)
		}
	}
	top := res.Hits[0]
	one := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 1, Text: "wheat"})
	if len(one.Hits) != 1 || one.Hits[0].Ref != top.Ref || !one.Freshness.Truncated {
		t.Fatalf("limit 1 = %+v, want top %s truncated", one.Hits, top.Ref)
	}
}

// TestRetrieveDropsSuperseded pins lifecycle visibility at consumption
// through the capture supersede verb: the old record stays stored but is
// never served, counted apart from deleted-source drops. The index still
// holds the pre-supersede bytes here (no re-sync), so the drop proves a
// stale index cannot resurface a record the source closed.
func TestRetrieveDropsSuperseded(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	a := addBody(t, doc, "", "retired wheat note", nil)
	b := addBody(t, doc, "", "replacement wheat note", nil)
	mustSync(t, syncQ(doc, idx, ""))
	must(t, Request{Verb: "supersede", Target: doc, ID: a.ID, IfRevision: 1, Replacement: b.ID, ReplacementRevision: 1})

	res := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, b.ID) {
		t.Fatalf("recall = %+v", res.Hits)
	}
	if f := res.Freshness; !f.Stale || f.DroppedState != 1 || f.DroppedStale != 0 {
		t.Fatalf("freshness = %+v, want stale with 1 state drop", f)
	}

	got := mustSync(t, syncQ(doc, idx, ""))
	if got.Added != 0 || got.Updated != 2 || got.Deleted != 0 || got.IndexDocuments != 2 {
		t.Fatalf("sync = %+v, want ~2 of 2", got)
	}
	res = mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, b.ID) {
		t.Fatalf("recall after sync = %+v", res.Hits)
	}
	if f := res.Freshness; f.Stale || f.DroppedState != 1 {
		t.Fatalf("freshness = %+v, want fresh with 1 state drop", f)
	}
}

// TestRetrieveRefusesInheritedStoreFlags mirrors the capture rule for both
// new commands: a set kb store selector inherited from the mount parent is
// refused with invalid_input, never silently ignored.
func TestRetrieveRefusesInheritedStoreFlags(t *testing.T) {
	mount := func(child *cobra.Command) *cobra.Command {
		parent := &cobra.Command{Use: "kb"}
		for _, name := range inheritedStoreFlags {
			parent.PersistentFlags().String(name, "", "")
		}
		parent.AddCommand(child)
		return parent
	}
	run := func(parent *cobra.Command, args []string) *ExitError {
		parent.SetArgs(args)
		err := parent.Execute()
		ee, ok := err.(*ExitError)
		if !ok || ee == nil {
			t.Fatalf("execute = %v, want *ExitError", err)
		}
		return ee
	}
	doc, idx := "db:sqlite%3Afile%3Anone.db", "db:bleve%3Afile%3Anone.bleve"

	ee := run(mount(newRetrieveCmd(nil, "")), []string{"--dir", "x", "retrieve", doc, "--index", idx})
	if ee.Code != 2 || !strings.Contains(ee.Msg, "kb store") {
		t.Fatalf("retrieve --dir = %+v, want invalid_input refusal", ee)
	}
	ee = run(mount(newSyncIndexCmd(nil, "")), []string{"--repo", "x", "index-sync", doc, "--index", idx})
	if ee.Code != 2 || !strings.Contains(ee.Msg, "kb store") {
		t.Fatalf("sync --repo = %+v, want invalid_input refusal", ee)
	}
}

// TestRetrieveWriterFailure pins the shared writer contract for the new
// human output: a result the caller never saw must not read as success.
// It reuses the capture test's failing writer.
func TestRetrieveWriterFailure(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")

	cmd := newRetrieveCmd(context.Background, dir)
	cmd.SetOut(failWriter{})
	cmd.SetArgs([]string{doc, "--index", idx})
	if err := cmd.Execute(); err == nil {
		t.Fatal("retrieve to a failing writer succeeded")
	} else if ee, ok := err.(*ExitError); !ok || ee.Code != 1 {
		t.Fatalf("retrieve error = %#v, want ExitError code 1", err)
	}
	sync := newSyncIndexCmd(context.Background, dir)
	sync.SetOut(failWriter{})
	sync.SetArgs([]string{doc, "--index", idx})
	if err := sync.Execute(); err == nil {
		t.Fatal("index-sync to a failing writer succeeded")
	} else if ee, ok := err.(*ExitError); !ok || ee.Code != 1 {
		t.Fatalf("sync error = %#v, want ExitError code 1", err)
	}
}

// TestRetrieveLifecycleForgetAsOf covers the shared kblife visibility at
// consumption: a forgotten record stays stored but is never served, even
// when the index still holds its pre-forget bytes; a historical --as-of
// shows the then-valid interval; and reads mutate nothing observable.
func TestRetrieveLifecycleForgetAsOf(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	keep := addBody(t, doc, "", "kept wheat note", nil)
	gone := addBody(t, doc, "", "doomed wheat note", nil)
	mustSync(t, syncQ(doc, idx, ""))
	must(t, Request{Verb: "forget", Target: doc, ID: gone.ID, IfRevision: 1, Reason: "test"})

	// No re-sync: the index still ranks the pre-forget bytes, but the
	// source tombstone hides the record.
	res := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, keep.ID) {
		t.Fatalf("recall = %+v", res.Hits)
	}
	if f := res.Freshness; !f.Stale || f.DroppedState != 1 || f.DroppedStale != 0 {
		t.Fatalf("freshness = %+v, want stale with 1 state drop", f)
	}

	// Historical read: an hour before the forget both intervals were open
	// and no tombstone existed, so both records show.
	old := query(doc, idx, "")
	old.AsOf = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	back := mustRetrieve(t, old)
	if len(back.Hits) != 2 || back.Freshness.DroppedState != 0 {
		t.Fatalf("as-of recall = %+v", back.Hits)
	}

	// Reads mutate nothing observable: revisions and fingerprints repeat.
	again := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if again.Freshness.SourceRevision != res.Freshness.SourceRevision ||
		again.Freshness.IndexRevision != res.Freshness.IndexRevision ||
		again.Hits[0].Revision != res.Hits[0].Revision {
		t.Fatalf("recall moved: %+v vs %+v", again.Freshness, res.Freshness)
	}

	bad := query(doc, idx, "")
	bad.AsOf = "not-a-time"
	if c := retrieveCode(t, bad); c != "invalid_input" {
		t.Fatalf("bad --as-of = %s, want invalid_input", c)
	}
}

// TestRetrieveBusyOnConcurrentIndexWrite proves the generation guard: a
// writer committing between the index snapshot and the ranked query moves
// the dbfts batch counter, and the read returns busy with no output
// instead of mixing generations. Both writers are separate admitted
// writers on a memory index behind one explicit shared owner session —
// reads never exclude writers, so no timing luck is involved: the probe
// rendezvous holds the read inside the op until the writer has committed,
// and every writer joins before the read is released. The ABA case puts
// then deletes one temporary indexed ID (two batches, byte-identical
// before/after content), so only the counter can trip the guard: equal
// snapshot fingerprints beside the busy prove it.
func TestRetrieveBusyOnConcurrentIndexWrite(t *testing.T) {
	dir := isolate(t)
	s := owner(t, dir)
	doc := loc("sqlite:mem:" + s + "/scratch")
	idx := loc("bleve:mem:" + s + "/rank")
	pair(t, doc, idx, "")
	addBody(t, doc, "", "guarded wheat note", nil)
	mustSync(t, syncQ(doc, idx, ""))
	// raceRead runs one retrieval while write commits through the normal
	// path mid-read. The probe only rendezvous: it signals entry and
	// waits for the writer, so the generation moves strictly between the
	// op's two Generation reads. Timeouts fail the test, never hang it.
	// The read goroutine is always joined before returning, and the
	// writer runs inline on this goroutine, so no hook or writer
	// outlives the test.
	raceRead := func(text string, write func()) Envelope {
		t.Helper()
		entered := make(chan struct{})
		release := make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		enterIt := func() { enterOnce.Do(func() { close(entered) }) }
		releaseIt := func() { releaseOnce.Do(func() { close(release) }) }
		defer func() { releaseIt(); retrieveProbe = nil }()
		retrieveProbe = func(context.Context, dbstore.Handle) {
			enterIt()
			select {
			case <-release:
			case <-time.After(15 * time.Second):
			}
		}
		resCh := make(chan Envelope, 1)
		go func() {
			resCh <- Retrieve(context.Background(),
				RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: text})
		}()
		select {
		case <-entered:
		case <-time.After(15 * time.Second):
			t.Fatalf("read never entered the index")
		}
		write()
		releaseIt()
		select {
		case env := <-resCh:
			return env
		case <-time.After(30 * time.Second):
			t.Fatalf("raced read never returned")
			return Envelope{}
		}
	}
	// One racing write between the snapshot and the ranked query.
	env := raceRead("wheat", func() {
		addBody(t, doc, "", "racing wheat note", nil)
		mustSync(t, syncQ(doc, idx, ""))
	})
	if env.OK || env.Error == nil || env.Error.Code != "busy" {
		t.Fatalf("raced recall = %+v, want busy", env.Error)
	}
	// Content ABA on the browse path: put then delete one temporary
	// indexed ID through the normal admitted fts path, between the
	// read's index snapshot and its ranked query. The before/after index
	// content is byte-identical while the batch counter moved twice.
	mid := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if len(mid.Hits) != 2 || mid.Freshness.Stale {
		t.Fatalf("recall before ABA = %d hits stale=%v", len(mid.Hits), mid.Freshness.Stale)
	}
	env = raceRead("", func() {
		tmp := json.RawMessage(`{"body":"aba probe note"}`)
		ftsWrite(t, s, idx, dbfts.OpPut, dbfts.Input{ID: "aba-probe-tmp", Doc: tmp})
		ftsWrite(t, s, idx, dbfts.OpDelete, dbfts.Input{ID: "aba-probe-tmp"})
	})
	if env.OK || env.Error == nil || env.Error.Code != "busy" {
		t.Fatalf("ABA recall = %+v, want busy", env.Error)
	}
	// With no racing writer the path serves again: only the raced reads
	// failed, never the read itself. The ABA left the index content
	// identical, so the fingerprint matches the pre-ABA read exactly —
	// the busy above could only have come from the moved generation.
	res := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if len(res.Hits) != 2 || res.Freshness.Stale {
		t.Fatalf("recall after race = %+v fresh=%+v", res.Hits, res.Freshness)
	}
	if res.Freshness.IndexRevision != mid.Freshness.IndexRevision {
		t.Fatalf("post-ABA index revision = %s, want pre-ABA %s",
			res.Freshness.IndexRevision, mid.Freshness.IndexRevision)
	}
}

// ftsWrite runs one admitted full-text write through the owning session —
// the same admission production writers use — and joins it: the commit is
// durable when it returns. The read under test never writes; only this
// separate writer moves the index between the read's two generation
// observations.
func ftsWrite(t *testing.T, session, target, op string, in dbfts.Input) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	tt, err := dblifecycle.ParseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := dblifecycle.Call(ctx, session,
		dblifecycle.Request{Op: op, Target: tt, Wait: 5 * time.Second, Input: raw})
	if err != nil {
		t.Fatalf("fts %s: %v", op, err)
	}
	if !res.OK {
		t.Fatalf("fts %s: %+v", op, res)
	}
}

// TestSyncLargeSharedSnapshot proves the inherited owner wire cap: a sync
// snapshot past the old 64KiB request bound reconciles through a shared
// owner session instead of failing EOF.
func TestSyncLargeSharedSnapshot(t *testing.T) {
	dir := isolate(t)
	s := owner(t, dir)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, s)
	const n = 24
	for i := 0; i < n; i++ {
		addBody(t, doc, s, "large shared wheat "+strings.Repeat("grain ", 900), nil)
	}
	got := mustSync(t, syncQ(doc, idx, s))
	if got.SourceDocuments != n || got.Added != n || got.IndexDocuments != uint64(n) {
		t.Fatalf("sync = %+v, want %d added", got, n)
	}
	res := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Session: s, Limit: n, MaxBytes: 1 << 20, Text: "wheat"})
	if len(res.Hits) != n || res.Freshness.Stale {
		t.Fatalf("recall = %d hits stale=%v", len(res.Hits), res.Freshness.Stale)
	}
}
