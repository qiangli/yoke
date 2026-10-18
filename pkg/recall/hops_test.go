package recall

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/kbmemory"
)

// hopRepo writes pages plus a graph.jsonl of link relations into a repo-ring
// fixture and returns its readers (pages + relations).
func hopRepo(t *testing.T, pages []*kb.Page, rels []kb.Relation) (string, []Reader) {
	t.Helper()
	dir := t.TempDir()
	store := kb.Open(dir)
	for _, p := range pages {
		if err := store.Write(p, "add"); err != nil {
			t.Fatalf("write %s: %v", p.Slug, err)
		}
	}
	if len(rels) > 0 {
		var b strings.Builder
		for _, r := range rels {
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(raw)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(kb.RelationPath(dir), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, []Reader{
		RepoRing{Store: kb.Open(dir), Path: dir},
		RelationRing{RingName: RingRepo, Path: dir},
	}
}

func hopLink(id, target, relation, dst, by string) kb.Relation {
	return kb.Relation{
		ID: id, Op: "link", By: by, At: time.Now().UTC(),
		Target: target, Relation: relation, Dst: dst,
	}
}

// hopCode is the code-form fixture reader: text matching on Recall (a stand-in
// for CodeRing's ranking, which the walk never re-implements) and a static
// code graph behind CodeNodes.
type hopCode struct {
	ring  string
	nodes []Hit
	edges []LabeledEdge
}

func (r *hopCode) Ring() string    { return r.ring }
func (r *hopCode) Forms() []string { return []string{kb.FormCode} }
func (r *hopCode) Recall(q Query) ([]Hit, error) {
	terms := kb.Terms(q.Text)
	var out []Hit
	for _, h := range r.nodes {
		hay := strings.ToLower(h.ID + " " + h.Cue + " " + h.Gist)
		for _, term := range terms {
			if strings.Contains(hay, strings.ToLower(term)) {
				out = append(out, h)
				break
			}
		}
	}
	return out, nil
}
func (r *hopCode) CodeNodes(Query) ([]Hit, []LabeledEdge, error) { return r.nodes, r.edges, nil }

func hopCodeHit(id, cue string) Hit {
	return Hit{
		Ring: RingRepo, Kind: "code", ID: id, Form: kb.FormCode,
		Cue: cue, Gist: "test fixture", Full: cue + " body",
		Source: []SourceRef{{URI: "file://fixture/" + id}},
		Score:  1, Why: []string{"codegraph"},
	}
}

func hitIDs(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}

func hitByID(hits []Hit, id string) *Hit {
	for i := range hits {
		if hits[i].ID == id {
			return &hits[i]
		}
	}
	return nil
}

// assertHopPathsResolvable pins the path contract: every ref in every path
// names a record the same principal can re-open (a visible kb page or a
// known code node), alternating with relation labels, ending at the hit.
func assertHopPathsResolvable(t *testing.T, hits []Hit, store *kb.Store, codeIDs map[string]bool) {
	t.Helper()
	visible := map[string]bool{}
	if store != nil {
		pages, err := store.List()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pages {
			visible["kb:"+p.Slug] = true
		}
	}
	known := map[string]bool{}
	for id := range visible {
		known[id] = true
	}
	for id := range codeIDs {
		known[id] = true
	}
	for _, h := range hits {
		if len(h.Path) == 0 {
			continue
		}
		if h.Path[len(h.Path)-1] != h.ID {
			t.Errorf("path of %s ends at %q", h.ID, h.Path[len(h.Path)-1])
		}
		for i, e := range h.Path {
			if i%2 == 1 {
				continue // relation label
			}
			if !known[e] {
				t.Errorf("path ref %q of %s is not resolvable by this principal", e, h.ID)
			}
		}
	}
}

// A lesson page about an entity reaches the related code symbol and its test
// at --hops 2 with --forms page,relation,code: lesson -about-> entity
// -calls-> symbol and entity -tested-by-> test.
func TestHopsLessonReachesCodeAndTest(t *testing.T) {
	lesson := &kb.Page{Slug: "backoff-lesson", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "allocator retry backoff lesson", Description: "WHEN the widget allocator fails retry with backoff",
		Status: kb.StatusValidated, Body: "Retries need a budget. See [[retry-entity]]."}
	entity := &kb.Page{Slug: "retry-entity", Form: kb.FormPage, Type: kb.TypeFact,
		Title: "retry budget entity", Description: "The retry budget concept behind allocator backoff",
		Status: kb.StatusValidated, Body: "Implemented by the allocator; covered by its test."}
	_, readers := hopRepo(t, []*kb.Page{lesson, entity}, []kb.Relation{
		hopLink("r1", "kb:backoff-lesson", "about", "kb:retry-entity", ""),
		hopLink("r2", "kb:retry-entity", "calls", "code:widget/alloc.go:Allocator", ""),
		hopLink("r3", "kb:retry-entity", "tested-by", "code:widget/alloc_test.go:TestAllocator", ""),
	})
	sym := "code:widget/alloc.go:Allocator"
	tst := "code:widget/alloc_test.go:TestAllocator"
	code := &hopCode{ring: RingRepo,
		nodes: []Hit{hopCodeHit(sym, "code Allocator widget/alloc.go"), hopCodeHit(tst, "code TestAllocator widget/alloc_test.go")},
		edges: []LabeledEdge{{From: "code:file:widget/alloc.go", To: sym, Relation: "contains"}},
	}
	readers = append(readers, code)
	forms := []string{kb.FormPage, kb.FormRelation, kb.FormCode}

	plain := Recall(Query{Text: "backoff retry lesson", K: 8, Forms: forms}, readers...)
	if hitByID(plain.Hits, sym) != nil || hitByID(plain.Hits, tst) != nil {
		t.Fatalf("hops 0 already reaches code: %v", hitIDs(plain.Hits))
	}
	multi := Recall(Query{Text: "backoff retry lesson", K: 8, Forms: forms, Hops: 2}, readers...)
	s := hitByID(multi.Hits, sym)
	te := hitByID(multi.Hits, tst)
	if s == nil || te == nil {
		t.Fatalf("hops 2 misses code: %v", hitIDs(multi.Hits))
	}
	if len(s.Path) == 0 || s.Path[0] == sym {
		t.Fatalf("symbol path is not seed-rooted: %v", s.Path)
	}
	if s.Path[len(s.Path)-1] != sym || te.Path[len(te.Path)-1] != tst {
		t.Fatalf("paths do not end at their hits: %v / %v", s.Path, te.Path)
	}
	seedIDs := map[string]bool{}
	for _, h := range plain.Hits {
		seedIDs[h.ID] = true
	}
	if !seedIDs[s.Path[0]] {
		t.Fatalf("symbol path starts at %q, not a direct hit %v", s.Path[0], hitIDs(plain.Hits))
	}
	assertHopPathsResolvable(t, multi.Hits, nil, map[string]bool{sym: true, tst: true,
		"kb:backoff-lesson": true, "kb:retry-entity": true})
}

// A multi-hop question whose answer page shares no keywords with the query
// is found at --hops 2 and not at --hops 0.
func TestHopsKeywordlessAnswer(t *testing.T) {
	q := &kb.Page{Slug: "moss-queries", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "green moss garden queries", Description: "WHEN choosing moss for shade prefer cushion varieties",
		Status: kb.StatusValidated, Body: "Compare [[stone-answer]] before finalizing the bed."}
	a := &kb.Page{Slug: "stone-answer", Form: kb.FormPage, Type: kb.TypeFact,
		Title: "granite load tables", Description: "Axial capacities for dry stone walls",
		Status: kb.StatusValidated, Body: "Granite carries the load."}
	_, readers := hopRepo(t, []*kb.Page{q, a}, nil)

	plain := Recall(Query{Text: "moss shade garden", K: 3, Forms: []string{kb.FormPage}}, readers...)
	if hitByID(plain.Hits, "kb:stone-answer") != nil {
		t.Fatalf("hops 0 reaches the keywordless answer: %v", hitIDs(plain.Hits))
	}
	multi := Recall(Query{Text: "moss shade garden", K: 3, Forms: []string{kb.FormPage}, Hops: 2}, readers...)
	ans := hitByID(multi.Hits, "kb:stone-answer")
	if ans == nil {
		t.Fatalf("hops 2 misses the answer: %v", hitIDs(multi.Hits))
	}
	want := []string{"kb:moss-queries", "link", "kb:stone-answer"}
	if strings.Join(ans.Path, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("answer path = %v, want %v", ans.Path, want)
	}
	ctx := Context(Query{Text: "moss shade garden", K: 3, Forms: []string{kb.FormPage}, Hops: 2}, readers...)
	found := false
	for _, b := range ctx.Blocks {
		if b.Ref == "kb:stone-answer" {
			found = true
			if len(b.Path) == 0 {
				t.Fatalf("context block carries no path")
			}
		}
	}
	if !found {
		t.Fatalf("context hops 2 misses the answer: %v", blockRefs(ctx.Blocks))
	}
}

// Output is byte-identical across runs and the budgets hold on a high-degree
// hub; relation weights keep their documented shape.
func TestHopsDeterministicAndBudgeted(t *testing.T) {
	pages := []*kb.Page{{
		Slug: "hub-lesson", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "hub lesson on spokes", Description: "WHEN spokes wobble true the hub",
		Status: kb.StatusValidated,
		Body:   "spokes: " + hubLinks(100),
	}}
	for i := 0; i < 100; i++ {
		pages = append(pages, &kb.Page{Slug: spoke(i), Form: kb.FormPage, Type: kb.TypeFact,
			Title: "spoke fact", Description: "a spoke outcome report",
			Status: kb.StatusValidated, Body: "spoke body"})
	}
	_, readers := hopRepo(t, pages, nil)
	q := Query{Text: "hub spokes wobble", K: 5, Forms: []string{kb.FormPage}, Hops: 3}

	first, err := json.Marshal(Recall(q, readers...))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(Recall(q, readers...))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("recall output differs across runs:\n%s\n%s", first, second)
	}
	cq := Query{Text: "hub spokes wobble", K: 5, Forms: []string{kb.FormPage}, Hops: 3, Budget: 700}
	cfirst, err := json.Marshal(Context(cq, readers...))
	if err != nil {
		t.Fatal(err)
	}
	csecond, err := json.Marshal(Context(cq, readers...))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfirst) != string(csecond) {
		t.Fatalf("context output differs across runs")
	}
	tight := Recall(Query{Text: "hub spokes wobble", K: 10, Forms: []string{kb.FormPage}, Hops: 3, Budget: 30}, readers...)
	if tight.Budget.Spent > 30 {
		t.Fatalf("spent %d over budget 30 on a high-degree hub", tight.Budget.Spent)
	}
	if !tight.Budget.Truncated {
		t.Fatalf("expected truncation under a 30-token budget, spent %d", tight.Budget.Spent)
	}
	if got := hopWeight("about"); got != 1.0 {
		t.Fatalf("hopWeight(about) = %v, want 1.0", got)
	}
	if got := hopWeight("unregistered-relation"); got != 0.5 {
		t.Fatalf("hopWeight(open vocabulary) = %v, want prose strength 0.5", got)
	}
	if got := hopMultiplicity(1.0); got != 3 {
		t.Fatalf("hopMultiplicity(1.0) = %v, want 3", got)
	}
}

func hubLinks(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("[[")
		b.WriteString(spoke(i))
		b.WriteString("]] ")
	}
	return b.String()
}

func spoke(i int) string { return "spoke-page-" + itoa(i+1000) }

// --hops 0 is regression-identical to the historical behaviour.
func TestHopsZeroIdentical(t *testing.T) {
	lesson := &kb.Page{Slug: "backoff-lesson", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "allocator retry backoff lesson", Description: "WHEN the widget allocator fails retry with backoff",
		Status: kb.StatusValidated, Body: "See [[retry-entity]]."}
	entity := &kb.Page{Slug: "retry-entity", Form: kb.FormPage, Type: kb.TypeFact,
		Title: "retry budget entity", Description: "The retry budget concept",
		Status: kb.StatusValidated, Body: "Implemented by the allocator."}
	_, readers := hopRepo(t, []*kb.Page{lesson, entity}, []kb.Relation{
		hopLink("r1", "kb:backoff-lesson", "about", "kb:retry-entity", ""),
	})
	forms := []string{kb.FormPage, kb.FormRelation}
	base := Recall(Query{Text: "backoff retry", K: 5, Forms: forms}, readers...)
	zero := Recall(Query{Text: "backoff retry", K: 5, Forms: forms, Hops: 0}, readers...)
	bb, _ := json.Marshal(base)
	zb, _ := json.Marshal(zero)
	if string(bb) != string(zb) {
		t.Fatalf("hops 0 differs:\n%s\n%s", bb, zb)
	}
	cbase := Context(Query{Text: "backoff retry", K: 5, Forms: forms}, readers...)
	czero := Context(Query{Text: "backoff retry", K: 5, Forms: forms, Hops: 0}, readers...)
	cb, _ := json.Marshal(cbase)
	cz, _ := json.Marshal(czero)
	if string(cb) != string(cz) {
		t.Fatalf("context hops 0 differs:\n%s\n%s", cb, cz)
	}
	for _, h := range zero.Hits {
		if len(h.Path) != 0 {
			t.Fatalf("hops 0 hit %s carries a path", h.ID)
		}
	}
}

// Two principals sharing one agent ring: the other principal's hidden node
// never connects, scores, or appears — in hits or in paths.
func TestHopsHiddenBridgeAcrossPrincipals(t *testing.T) {
	dir := t.TempDir()
	mk := func(slug, title, desc, owner string) *kb.Page {
		return &kb.Page{Slug: slug, Form: kb.FormNote, Type: kb.TypeLesson,
			Title: title, Description: desc, Status: kb.StatusValidated,
			Source: &kb.Source{Tool: owner}}
	}
	store := kb.Open(dir)
	for _, p := range []*kb.Page{
		mk("alpha-note", "alpha query", "WHEN alpha runs prefer the northern route", "owner"),
		mk("beta-note", "beta answer", "Zebra quilt xenon verdict", "owner"),
		mk("bridge-note", "bridge observation", "a foreign bridge record", "other"),
	} {
		if err := store.Write(p, "add"); err != nil {
			t.Fatal(err)
		}
	}
	var b strings.Builder
	for _, r := range []kb.Relation{
		hopLink("r1", "kb:alpha-note", "about", "kb:bridge-note", "other"),
		hopLink("r2", "kb:bridge-note", "about", "kb:beta-note", "other"),
	} {
		raw, _ := json.Marshal(r)
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(kb.RelationPath(dir), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	owner := []Reader{
		AgentRing{Store: kb.OpenAgentRing(dir, "owner"), Path: dir},
		RelationRing{RingName: RingAgent, Path: dir, Owner: "owner"},
	}
	// The answer shares no keywords with the query: only the hidden bridge
	// connects them, so the owner must not reach it at any depth.
	got := Recall(Query{Text: "alpha query", K: 5, Hops: 4}, owner...)
	if hitByID(got.Hits, "kb:beta-note") != nil {
		t.Fatalf("hidden bridge leaks the answer: %v", hitIDs(got.Hits))
	}
	for _, h := range got.Hits {
		for _, e := range h.Path {
			if strings.Contains(e, "bridge-note") {
				t.Fatalf("hidden node in path of %s: %v", h.ID, h.Path)
			}
		}
		if strings.Contains(h.ID, "bridge-note") {
			t.Fatalf("hidden node emitted: %s", h.ID)
		}
	}
	// Control: the same walk through a visible bridge does reach the answer.
	other := []Reader{
		AgentRing{Store: kb.OpenAgentRing(dir, "other"), Path: dir},
		RelationRing{RingName: RingAgent, Path: dir, Owner: "other"},
	}
	seen := Recall(Query{Text: "bridge observation", K: 5, Hops: 2}, other...)
	if hitByID(seen.Hits, "kb:bridge-note") == nil {
		t.Fatalf("control principal misses its own bridge: %v", hitIDs(seen.Hits))
	}
}

// The adapter contract: --as-of travels into RetrieveQuery.AsOf, Hit.Ref is
// the record ID verbatim, and the graph node ref is built from
// (DocTarget, Ref) without rewriting it.
func TestMemorySeedsContract(t *testing.T) {
	var captured kbmemory.RetrieveQuery
	stub := func(_ context.Context, q kbmemory.RetrieveQuery) kbmemory.Envelope {
		captured = q
		return kbmemory.Envelope{OK: true, Op: kbmemory.RetrievalOp, Result: kbmemory.RetrieveResult{
			Hits: []kbmemory.Hit{{
				Ref: "rec-123", Score: 2.5,
				Record: kbmemory.Record{Title: "memory title", Body: "memory body words"},
			}},
			Freshness: kbmemory.Freshness{DocTarget: "doctarget-1"},
		}}
	}
	asOf := time.Date(2026, 4, 2, 3, 4, 5, 0, time.UTC)
	src := MemorySeeds{Ring: RingRepo, Target: "t", Index: "i", Call: stub}.Source
	hits, err := src(Query{Text: "memory body", K: 3, AsOf: asOf})
	if err != nil {
		t.Fatal(err)
	}
	if captured.AsOf != asOf.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("RetrieveQuery.AsOf = %q, want the recall --as-of", captured.AsOf)
	}
	if len(hits) != 1 || hits[0].ID != "rec-123" {
		t.Fatalf("adapter rewrote Hit.Ref: %+v", hits)
	}
	if hits[0].Score != 2.5 {
		t.Fatalf("adapter re-ranked: score = %v", hits[0].Score)
	}
	if got := MemoryNodeRef("doctarget-1", "rec-123"); got != "memory:doctarget-1#rec-123" {
		t.Fatalf("MemoryNodeRef = %q", got)
	}
	if key, qualified := memoryNodeOf(hits[0]); !qualified || key != "memory:doctarget-1#rec-123" {
		t.Fatalf("memoryNodeOf = %q, %v", key, qualified)
	}
	if _, err := src(Query{Text: "x", AsOf: asOf}); err != nil {
		t.Fatal(err)
	}
	// Zero as-of means now on both sides: empty string, never year 1.
	zeroSrc := MemorySeeds{Call: stub}.Source
	if _, err := zeroSrc(Query{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if captured.AsOf != "" {
		t.Fatalf("zero as-of travelled as %q, want empty (kbmemory fixes now)", captured.AsOf)
	}
	// Backend failures surface; the adapter never substitutes another mode.
	fail := func(_ context.Context, _ kbmemory.RetrieveQuery) kbmemory.Envelope {
		return kbmemory.Envelope{Error: &kbmemory.ErrorBody{Code: "unsupported", Message: "no backend"}}
	}
	failSrc := MemorySeeds{Call: fail}.Source
	if _, err := failSrc(Query{Text: "x"}); err == nil {
		t.Fatalf("backend failure swallowed")
	}
}

// Adapter seeds emit verbatim into their ring and condition the walk without
// colliding across collections.
func TestMemorySeedsRingEmission(t *testing.T) {
	stub := func(_ context.Context, _ kbmemory.RetrieveQuery) kbmemory.Envelope {
		return kbmemory.Envelope{OK: true, Op: kbmemory.RetrievalOp, Result: kbmemory.RetrieveResult{
			Hits: []kbmemory.Hit{
				{Ref: "mem-shared", Score: 3, Record: kbmemory.Record{Title: "shared a", Body: "alpha words"}},
				{Ref: "mem-other", Score: 1, Record: kbmemory.Record{Title: "other b", Body: "beta words"}},
			},
			Freshness: kbmemory.Freshness{DocTarget: "target-a"},
		}}
	}
	_, readers := hopRepo(t, []*kb.Page{page("alpha-page", "alpha words", "alpha words")}, nil)
	q := Query{Text: "alpha words", K: 5, Hops: 1,
		Seeds: MemorySeeds{Ring: RingRepo, Target: "t", Index: "i", Call: stub}.Source}
	got := Recall(q, readers...)
	if hitByID(got.Hits, "mem-shared") == nil {
		t.Fatalf("adapter seeds not emitted: %v", hitIDs(got.Hits))
	}
	again, _ := json.Marshal(Recall(q, readers...))
	first, _ := json.Marshal(got)
	if string(first) != string(again) {
		t.Fatalf("adapter-seeded output differs across runs")
	}
}

// The --hops flag is registered on both commands.
func TestHopsFlagRegistered(t *testing.T) {
	if NewRecallCmd().Flags().Lookup("hops") == nil {
		t.Fatalf("recall has no --hops")
	}
	if NewContextCmd().Flags().Lookup("hops") == nil {
		t.Fatalf("context has no --hops")
	}
}
