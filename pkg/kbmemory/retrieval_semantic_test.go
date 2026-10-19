package kbmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbdoc"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbvec"
)

// This file covers the K2 semantic/hybrid slice (story 56508548a7df) with
// short filtered in-process tests: the shared pipeline extended for vector
// ranking, RRF fusion, lifecycle/deletion drops, filter-before-limit, the
// labeled lexical fallback, model/dimension validation through the vec
// category, and the local-door embedder against a fake door. A fake
// Embedder replaces the model everywhere except the EmbedLocal tests; real
// model proof stays with the manager gate.

const (
	semModel = "test-model"
	semDim   = 3
)

// mkVec creates the sqlite vector target and its embedding collection
// through the same admission the producer uses. Retrieval never provisions
// it: tests create it explicitly, as operators do.
func mkVec(t *testing.T, target, session string) {
	t.Helper()
	tt, err := dblifecycle.ParseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	by := session
	if tt.Mode == dbstore.Memory {
		by = tt.Session
	}
	if by != "" {
		res, err := dblifecycle.Call(ctx, by, dblifecycle.Request{Op: "create", Target: tt, Wait: 5 * time.Second})
		if err != nil || !res.OK {
			t.Fatalf("create vector target: %+v %v", res, err)
		}
	} else if _, err := (dblifecycle.Service{}).Create(ctx, tt, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(dbvec.Input{Collection: DefaultVectorCollection, Dimension: semDim, Metric: "cosine", Model: semModel})
	if err != nil {
		t.Fatal(err)
	}
	if by != "" {
		res, err := dblifecycle.Call(ctx, by, dblifecycle.Request{Op: dbvec.OpCollectionAdd, Target: tt, Wait: 5 * time.Second, Input: raw})
		if err != nil || !res.OK {
			t.Fatalf("collection add: %+v %v", res, err)
		}
		return
	}
	if _, _, err := (dblifecycle.Service{}).RunCategory(ctx, tt, dbvec.OpCollectionAdd, raw, true, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

// putVec stores one embedding for a memory record the way the K2 producer
// does: the stable record ID with the record JSON as the item metadata.
func putVec(t *testing.T, target, session string, r Record, vec []float32) {
	t.Helper()
	meta, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	rawVec, err := json.Marshal(vec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(dbvec.Input{Collection: DefaultVectorCollection, Ops: []dbvec.BatchOp{
		{Op: "put", ID: r.ID, Vector: rawVec, Metadata: meta, Model: semModel},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tt, err := dblifecycle.ParseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	by := session
	if tt.Mode == dbstore.Memory {
		by = tt.Session
	}
	if by != "" {
		res, err := dblifecycle.Call(ctx, by, dblifecycle.Request{Op: dbvec.OpBatch, Target: tt, Wait: 5 * time.Second, Input: raw})
		if err != nil || !res.OK {
			t.Fatalf("vec batch: %+v %v", res, err)
		}
		return
	}
	if _, _, err := (dblifecycle.Service{}).RunCategory(ctx, tt, dbvec.OpBatch, raw, true, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

// fakeEmbedder maps rank texts to fixed vectors and counts its calls, so
// tests prove paraphrase ranking and that lexical paths never embed.
type fakeEmbedder struct {
	calls int64
	table map[string][]float32
	err   error
}

func (f *fakeEmbedder) embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	atomic.AddInt64(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v, ok := f.table[text]
		if !ok {
			return nil, fmt.Errorf("fake embedder has no vector for %q", text)
		}
		out[i] = append([]float32(nil), v...)
	}
	return out, nil
}

func (f *fakeEmbedder) fn() Embedder { return f.embed }

func (f *fakeEmbedder) n() int64 { return atomic.LoadInt64(&f.calls) }

// trio wires one memory collection with its lexical index and its vector
// target in file mode.
func trio(t *testing.T, doc, idx, vec, session string) {
	t.Helper()
	must(t, Request{Verb: "init", Target: doc, Session: session})
	mkIndex(t, idx, session)
	mkVec(t, vec, session)
}

func semQuery(doc, idx, vec, session string, f *fakeEmbedder) RetrieveQuery {
	return RetrieveQuery{Target: doc, Index: idx, Session: session, Mode: ModeSemantic,
		Vector: vec, EmbedModel: semModel, Embed: f.fn(), Limit: 10}
}

// TestSemanticParaphraseRoundTrip is the vector path end to end: a query
// sharing no lexical token with the record still ranks it by embedding,
// serving the stable ref, the semantic mode and a fresh population report.
func TestSemanticParaphraseRoundTrip(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	dog := addBody(t, doc, "", "the dog runs at dawn", nil)
	addBody(t, doc, "", "a bus timetable", nil)
	mustSync(t, syncQ(doc, idx, ""))
	fake := &fakeEmbedder{table: map[string][]float32{
		"hound": {1, 0.1, 0.1},
		"dog":   {1, 0.1, 0.1},
		"bus":   {0.1, 1, 0.1},
	}}
	putVec(t, vec, "", dog, []float32{1, 0.1, 0.1})

	// Lexical alone cannot see the paraphrase.
	lex := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "hound"})
	if len(lex.Hits) != 0 {
		t.Fatalf("lexical paraphrase = %+v, want no hits", lex.Hits)
	}

	q := semQuery(doc, idx, vec, "", fake)
	q.Text = "hound"
	res := mustRetrieve(t, q)
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, dog.ID) {
		t.Fatalf("semantic recall = %+v", res.Hits)
	}
	h := res.Hits[0]
	if h.Mode != ModeSemantic || res.Mode != ModeSemantic || res.Fallback {
		t.Fatalf("mode = %q/%q fallback=%v, want semantic/semantic/false", h.Mode, res.Mode, res.Fallback)
	}
	if h.Revision != 1 || h.Stale || h.Score <= 0 {
		t.Fatalf("hit = %+v, want rev 1 fresh positive score", h)
	}
	if fr := res.Freshness; fr.SourceDocuments != 2 || fr.VectorDocuments != 1 ||
		fr.VectorModel != semModel || fr.VectorDimension != semDim || fr.VectorTarget == "" {
		t.Fatalf("freshness = %+v, want 2 source / 1 vector with identity", fr)
	}
	if fake.n() != 1 {
		t.Fatalf("embed calls = %d, want 1", fake.n())
	}
}

// TestSemanticNeedsRankText pins the browse boundary: an empty rank text is
// a lexical browse only, never an embedding of nothing.
func TestSemanticNeedsRankText(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	fake := &fakeEmbedder{}
	q := semQuery(doc, idx, vec, "", fake)
	q.Text = ""
	if c := retrieveCode(t, q); c != "invalid_input" {
		t.Fatalf("empty semantic text = %s, want invalid_input", c)
	}
	if fake.n() != 0 {
		t.Fatalf("embed calls = %d, want 0", fake.n())
	}
}

// TestSemanticUnsupportedWithoutBackend pins the explicit-configuration
// boundary beside the existing lexical-only test: no vector target and no
// model stays structured unsupported, never a silent lexical substitution.
func TestSemanticUnsupportedWithoutBackend(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	for _, q := range []RetrieveQuery{
		{Target: doc, Index: idx, Mode: ModeSemantic, Text: "wheat", Limit: 10},
		{Target: doc, Index: idx, Mode: ModeHybrid, Text: "wheat", Limit: 10},
		{Target: doc, Index: idx, Mode: ModeSemantic, Text: "wheat", Limit: 10,
			Vector: loc("sqlite:file:" + filepath.Join(dir, "v.db"))},
	} {
		if c := retrieveCode(t, q); c != "unsupported" {
			t.Fatalf("mode %s = %s, want unsupported", q.Mode, c)
		}
	}
}

// TestSemanticFallback serves the labeled lexical fallback the automatic
// consumer explicitly chooses: same hits as lexical, with Fallback set.
func TestSemanticFallback(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	pair(t, doc, idx, "")
	a := addBody(t, doc, "", "fallback wheat note", nil)
	mustSync(t, syncQ(doc, idx, ""))
	q := RetrieveQuery{Target: doc, Index: idx, Mode: ModeSemantic, Text: "wheat",
		Limit: 10, FallbackLexical: true}
	res := mustRetrieve(t, q)
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, a.ID) {
		t.Fatalf("fallback recall = %+v", res.Hits)
	}
	if res.Mode != ModeLexical || !res.Fallback || res.Hits[0].Mode != ModeLexical {
		t.Fatalf("fallback label = %q/%v/%q, want lexical/true/lexical", res.Mode, res.Fallback, res.Hits[0].Mode)
	}
	plain := mustRetrieve(t, RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if len(plain.Hits) != 1 || plain.Hits[0].Ref != res.Hits[0].Ref || plain.Freshness != res.Freshness {
		t.Fatalf("fallback differs from lexical: %+v vs %+v", res.Freshness, plain.Freshness)
	}
}

// TestSemanticEmbedErrorFallback covers a failing backend behind an
// explicit fallback choice: the labeled lexical list, not the error.
func TestSemanticEmbedErrorFallback(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	a := addBody(t, doc, "", "fallback wheat note", nil)
	mustSync(t, syncQ(doc, idx, ""))
	putVec(t, vec, "", a, []float32{1, 0.1, 0.1})
	fake := &fakeEmbedder{err: fmt.Errorf("door is down")}

	bare := semQuery(doc, idx, vec, "", fake)
	bare.Text = "wheat"
	if c := retrieveCode(t, bare); c != "backend_error" {
		t.Fatalf("embed failure = %s, want backend_error", c)
	}
	with := bare
	with.FallbackLexical = true
	res := mustRetrieve(t, with)
	if len(res.Hits) != 1 || !res.Fallback || res.Mode != ModeLexical {
		t.Fatalf("error fallback = %+v fallback=%v", res.Hits, res.Fallback)
	}
}

// TestHybridRRFFusion pins the fusion contract: the union of both bounded
// lists in RRF order with deterministic ID tie-breaks, served as hybrid.
func TestHybridRRFFusion(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	lexOnly := addBody(t, doc, "", "wheat wheat wheat wheat", nil)
	semOnly := addBody(t, doc, "", "entirely different words here", nil)
	twinBody := "shared twin wheat note"
	twinA := addBody(t, doc, "", twinBody, nil)
	twinB := addBody(t, doc, "", twinBody, nil)
	mustSync(t, syncQ(doc, idx, ""))
	fake := &fakeEmbedder{table: map[string][]float32{
		"wheat": {0.1, 0.1, 1},
		"dog":   {1, 0.1, 0.1},
	}}
	putVec(t, vec, "", semOnly, []float32{1, 0.1, 0.1})
	putVec(t, vec, "", twinA, []float32{0.2, 0.2, 0.2})
	putVec(t, vec, "", twinB, []float32{0.2, 0.2, 0.2})

	q := semQuery(doc, idx, vec, "", fake)
	q.Mode = ModeHybrid
	q.Text = "dog"
	res := mustRetrieve(t, q)
	if res.Mode != ModeHybrid {
		t.Fatalf("mode = %q, want hybrid", res.Mode)
	}
	// "dog" is lexical nowhere: hybrid serves the whole vector ranking —
	// the exact match first, then the equidistant twins in ID order —
	// labeled hybrid.
	if len(res.Hits) != 3 || res.Hits[0].Ref != refOf(t, doc, semOnly.ID) {
		t.Fatalf("hybrid semantic-only = %+v", res.Hits)
	}
	for _, h := range res.Hits {
		if h.Mode != ModeHybrid {
			t.Fatalf("hit mode = %q, want hybrid", h.Mode)
		}
	}
	first, second := refOf(t, doc, twinA.ID), refOf(t, doc, twinB.ID)
	if strings.Compare(first, second) > 0 {
		first, second = second, first
	}
	if res.Hits[1].Ref != first || res.Hits[2].Ref != second {
		t.Fatalf("hybrid equidistant twins break ID order: %+v", res.Hits)
	}

	q.Text = "wheat"
	res = mustRetrieve(t, q)
	refs := map[string]bool{}
	for _, h := range res.Hits {
		if h.Mode != ModeHybrid {
			t.Fatalf("hit mode = %q, want hybrid", h.Mode)
		}
		refs[h.Ref] = true
	}
	// semOnly has no wheat token but the fake maps wheat far from its
	// vector; it may still rank. The lexical wheat records must all be
	// present: filtering and fusion never drop a live ranked record.
	for _, want := range []string{lexOnly.ID, twinA.ID, twinB.ID} {
		if !refs[refOf(t, doc, want)] {
			t.Fatalf("hybrid wheat missing %s in %+v", want, res.Hits)
		}
	}
	// Scores arrive in non-increasing order, and the identical twins —
	// same lexical score, same vector — break the tie by record ID.
	for i := 1; i < len(res.Hits); i++ {
		a, b := res.Hits[i-1], res.Hits[i]
		if b.Score > a.Score || (b.Score == a.Score && strings.Compare(b.Ref, a.Ref) < 0) {
			t.Fatalf("hybrid order breaks RRF/ID contract at %d: %+v", i, res.Hits)
		}
	}
	ia, ib := -1, -1
	for i, h := range res.Hits {
		if h.Ref == refOf(t, doc, twinA.ID) {
			ia = i
		}
		if h.Ref == refOf(t, doc, twinB.ID) {
			ib = i
		}
	}
	if ia < 0 || ib < 0 || ia-ib < -1 || ia-ib > 1 {
		t.Fatalf("twins are not adjacent: %+v", res.Hits)
	}
	lo, hi := ia, ib
	if lo > hi {
		lo, hi = hi, lo
	}
	if strings.Compare(res.Hits[lo].Ref, res.Hits[hi].Ref) >= 0 {
		t.Fatalf("twins break ID order: %+v", res.Hits)
	}
}

// TestSemanticLifecycleDrops proves consumption-side visibility for vector
// ranking: a forgotten record and a deleted source record are dropped and
// counted apart, never served, even though the vectors still name them.
func TestSemanticLifecycleDrops(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	keep := addBody(t, doc, "", "kept hound note", nil)
	gone := addBody(t, doc, "", "doomed hound note", nil)
	del := addBody(t, doc, "", "deleted hound note", nil)
	mustSync(t, syncQ(doc, idx, ""))
	fake := &fakeEmbedder{table: map[string][]float32{"hound": {1, 0.1, 0.1}}}
	for _, r := range []Record{keep, gone, del} {
		putVec(t, vec, "", r, []float32{1, 0.1, 0.1})
	}
	must(t, Request{Verb: "forget", Target: doc, ID: gone.ID, IfRevision: 1, Reason: "test"})
	docT, err := dblifecycle.ParseTarget(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docOp(context.Background(), Request{}, docT, dbdoc.OpRm,
		dbdoc.Input{Collection: Collection, ID: del.ID}, false); err != nil {
		t.Fatal(err)
	}

	q := semQuery(doc, idx, vec, "", fake)
	q.Text = "hound"
	res := mustRetrieve(t, q)
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, keep.ID) {
		t.Fatalf("recall = %+v, want only the live record", res.Hits)
	}
	if f := res.Freshness; !f.Stale || f.DroppedState != 1 || f.DroppedStale != 1 {
		t.Fatalf("freshness = %+v, want stale with 1 state and 1 stale drop", f)
	}
	for _, h := range res.Hits {
		if h.Ref == refOf(t, doc, gone.ID) || h.Ref == refOf(t, doc, del.ID) {
			t.Fatalf("closed record still served: %+v", h)
		}
	}
}

// TestSemanticFiltersBeforeLimit pins filter-before-limit for vector
// ranking: with limit 1 the top vector hit is filtered out by tags and the
// second one is served, not an empty page.
func TestSemanticFiltersBeforeLimit(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	addBody(t, doc, "", "first hound note", []string{"nope"})
	want := addBody(t, doc, "", "second hound note", []string{"yes"})
	mustSync(t, syncQ(doc, idx, ""))
	fake := &fakeEmbedder{table: map[string][]float32{"hound": {1, 0.1, 0.1}}}
	for _, r := range recordsOf(t, doc, "") {
		putVec(t, vec, "", r, []float32{1, 0.1, 0.1})
	}
	q := semQuery(doc, idx, vec, "", fake)
	q.Text = "hound"
	q.Tags = []string{"yes"}
	q.Limit = 1
	res := mustRetrieve(t, q)
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, want.ID) {
		t.Fatalf("filtered recall = %+v, want the tagged record", res.Hits)
	}

	// Scope and time clauses filter the same way.
	repoQ := semQuery(doc, idx, vec, "", fake)
	repoQ.Text = "hound"
	repoQ.Repos = []string{"no-such-repo"}
	if res := mustRetrieve(t, repoQ); len(res.Hits) != 0 {
		t.Fatalf("repos filter = %+v, want no hits", res.Hits)
	}
}

// TestSemanticModelMismatch pins validation through the vec category: the
// explicit model must match the collection's persisted model.
func TestSemanticModelMismatch(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	fake := &fakeEmbedder{table: map[string][]float32{"wheat": {1, 0.1, 0.1}}}
	q := semQuery(doc, idx, vec, "", fake)
	q.Text = "wheat"
	q.EmbedModel = "another-model"
	if c := retrieveCode(t, q); c != "invalid_input" {
		t.Fatalf("model mismatch = %s, want invalid_input", c)
	}
	if fake.n() != 0 {
		t.Fatalf("embed calls = %d, want 0 (validated before embedding)", fake.n())
	}
}

// TestSemanticDimensionMismatch covers an embedder disagreeing with the
// collection's persisted dimension.
func TestSemanticDimensionMismatch(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	fake := &fakeEmbedder{table: map[string][]float32{"wheat": {1, 2, 3, 4}}}
	q := semQuery(doc, idx, vec, "", fake)
	q.Text = "wheat"
	if c := retrieveCode(t, q); c != "invalid_input" {
		t.Fatalf("dimension mismatch = %s, want invalid_input", c)
	}
}

// TestSemanticMissingCollection pins the no-provisioning boundary on the
// vector side: an absent collection is not_found naming it.
func TestSemanticMissingCollection(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "novec.db"))
	must(t, Request{Verb: "init", Target: doc})
	mkIndex(t, idx, "")
	tt, err := dblifecycle.ParseTarget(vec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := (dblifecycle.Service{}).Create(ctx, tt, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	fake := &fakeEmbedder{table: map[string][]float32{"wheat": {1, 0.1, 0.1}}}
	q := semQuery(doc, idx, vec, "", fake)
	q.Text = "wheat"
	if c := retrieveCode(t, q); c != "not_found" {
		t.Fatalf("missing collection = %s, want not_found", c)
	}
}

// TestSemanticStaleLabel pins per-hit staleness for vector ranking: a
// record revised after ingestion serves the fresh document with Stale set.
func TestSemanticStaleLabel(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	a := addBody(t, doc, "", "stale hound note", nil)
	mustSync(t, syncQ(doc, idx, ""))
	putVec(t, vec, "", a, []float32{1, 0.1, 0.1})
	must(t, Request{Verb: "update", Target: doc, ID: a.ID, IfRevision: 1, Body: str("stale hound note, revised")})
	fake := &fakeEmbedder{table: map[string][]float32{"hound": {1, 0.1, 0.1}}}
	q := semQuery(doc, idx, vec, "", fake)
	q.Text = "hound"
	res := mustRetrieve(t, q)
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, a.ID) {
		t.Fatalf("recall = %+v", res.Hits)
	}
	h := res.Hits[0]
	if h.Revision != 2 || !h.Stale || h.Record.Body != "stale hound note, revised" {
		t.Fatalf("hit = %+v, want rev 2 stale with the fresh body", h)
	}
	if !res.Freshness.Stale {
		t.Fatalf("freshness = %+v, want stale", res.Freshness)
	}
}

// TestLexicalMakesNoEmbedCall pins the no-implicit-network boundary: a
// lexical recall with a vector target and an injected embedder configured
// never embeds.
func TestLexicalMakesNoEmbedCall(t *testing.T) {
	dir := isolate(t)
	doc := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	idx := loc("bleve:file:" + filepath.Join(dir, "mem.bleve"))
	vec := loc("sqlite:file:" + filepath.Join(dir, "mem-vec.db"))
	trio(t, doc, idx, vec, "")
	a := addBody(t, doc, "", "plain wheat note", nil)
	mustSync(t, syncQ(doc, idx, ""))
	fake := &fakeEmbedder{table: map[string][]float32{"wheat": {1, 0.1, 0.1}}}
	q := RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat",
		Vector: vec, EmbedModel: semModel, Embed: fake.fn()}
	res := mustRetrieve(t, q)
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, a.ID) || res.Mode != ModeLexical {
		t.Fatalf("lexical recall = %+v", res.Hits)
	}
	if fake.n() != 0 {
		t.Fatalf("embed calls = %d, want 0", fake.n())
	}
}

// TestRetrieveSchemaHasVectorFlags pins the CLI/MCP parity for the new
// knobs: the projected schema carries every flag the command accepts.
func TestRetrieveSchemaHasVectorFlags(t *testing.T) {
	schema := RetrieveSchema()
	got := map[string]bool{}
	for _, f := range schema.Flags {
		got[f.Name] = true
	}
	for _, want := range []string{"vector", "vector-collection", "embed-model", "fallback-lexical", "mode", "text", "index"} {
		if !got[want] {
			t.Fatalf("retrieve schema misses --%s (has %v)", want, got)
		}
	}
}

// TestEmbedLocalFakeDoor drives EmbedLocal against a fake local door: the
// exact HTTP contract the real door must satisfy — bearer token auth, the
// model passthrough, float embeddings per input — with the token isolated
// under a temp BASHY_HOME and the port pinned by BASHY_LLM_PORT.
func TestEmbedLocalFakeDoor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	var seenAuth, seenModel string
	var seenInputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/embeddings" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var in struct {
			Model string `json:"model"`
			Input any    `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		seenModel = in.Model
		raw, _ := json.Marshal(in.Input)
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			seenInputs = []string{one}
		} else if err := json.Unmarshal(raw, &seenInputs); err != nil {
			http.Error(w, "bad input shape", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(seenInputs))
		for i := range seenInputs {
			data[i] = map[string]any{"embedding": []float32{0.1 + float32(i), 0.2, 0.3}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_LLM_PORT", u.Port())

	vecs, err := EmbedLocal(context.Background(), "fake-model", []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 1 || len(vecs[0]) != semDim {
		t.Fatalf("embeddings = %v, want one %d-dim vector", vecs, semDim)
	}
	token, err := os.ReadFile(filepath.Join(home, "cligw", "token"))
	if err != nil {
		t.Fatal(err)
	}
	if seenAuth != "Bearer "+strings.TrimSpace(string(token)) {
		t.Fatalf("authorization = %q, want the door Bearer [REDACTED]", seenAuth)
	}
	if seenModel != "fake-model" || len(seenInputs) != 1 || seenInputs[0] != "hello" {
		t.Fatalf("model/input = %q/%q, want fake-model/[hello]", seenModel, seenInputs)
	}

	vecs, err = EmbedLocal(context.Background(), "fake-model", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || vecs[0][0] == vecs[1][0] {
		t.Fatalf("batch embeddings = %v, want two distinct vectors", vecs)
	}

	if _, err := EmbedLocal(context.Background(), "", []string{"x"}); err == nil {
		t.Fatal("empty model was accepted")
	}
	if _, err := EmbedLocal(context.Background(), "m", nil); err == nil {
		t.Fatal("empty texts were accepted")
	}
}

// TestEmbedLocalDoorDown stays a backend error, never a silent fallback:
// only an explicit FallbackLexical turns it into labeled lexical results.
func TestEmbedLocalDoorDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_LLM_PORT", strconv.Itoa(1))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := EmbedLocal(ctx, "fake-model", []string{"x"}); err == nil {
		t.Fatal("dead door was accepted")
	}
}
