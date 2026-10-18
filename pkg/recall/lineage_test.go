package recall

import (
	"reflect"
	"sort"
	"testing"

	"github.com/qiangli/yoke/pkg/kb"
)

// lineageRing writes one owner's agent-ring notes across a three-generation
// episode chain ep-p → ep-c → ep-g, plus an unrelated earlier session ep-x.
func lineageRing(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store := kb.Open(dir)
	note := func(slug, ep, parent string, export bool) *kb.Page {
		return &kb.Page{Slug: slug, Form: kb.FormNote, Type: kb.TypeLesson,
			Title: "widget " + slug, Description: "widget note " + slug,
			Status: kb.StatusCandidate, Export: export,
			Source: &kb.Source{Tool: "owner", Episode: ep, ParentEpisode: parent}}
	}
	for _, p := range []*kb.Page{
		note("p-private", "ep-p", "", false),
		note("p-exported", "ep-p", "", true),
		note("c-own", "ep-c", "ep-p", false),
		note("g-own", "ep-g", "ep-c", false),
		note("x-old", "ep-x", "", false),
	} {
		if err := store.Write(p, "add"); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func agentIDs(t *testing.T, dir, owner string, l Lineage) []string {
	t.Helper()
	hits, err := (AgentRing{Store: kb.OpenAgentRing(dir, owner), Path: dir}).Recall(
		Query{Text: "widget", K: 10, Forms: []string{kb.FormNote}, Lineage: l})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestLineageChildSeesExportedAncestorsOnly(t *testing.T) {
	isolateRecallStores(t)
	dir := lineageRing(t)
	got := agentIDs(t, dir, "owner", Lineage{Self: "ep-c", Parent: "ep-p"})
	want := []string{"kb:c-own", "kb:p-exported", "kb:x-old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child sees %v, want %v (never a non-exported ancestor note, no descendants)", got, want)
	}
	// The grandparent edge comes from ep-c's recorded parent_episode.
	got = agentIDs(t, dir, "owner", Lineage{Self: "ep-g", Parent: "ep-c"})
	want = []string{"kb:g-own", "kb:p-exported", "kb:x-old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grandchild sees %v, want %v", got, want)
	}
}

func TestLineageParentReachesDescendantsOnlyWithLineage(t *testing.T) {
	isolateRecallStores(t)
	dir := lineageRing(t)
	got := agentIDs(t, dir, "owner", Lineage{Self: "ep-p"})
	want := []string{"kb:p-exported", "kb:p-private", "kb:x-old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parent without --lineage sees %v, want %v", got, want)
	}
	got = agentIDs(t, dir, "owner", Lineage{Self: "ep-p", Descendants: true})
	want = []string{"kb:c-own", "kb:g-own", "kb:p-exported", "kb:p-private", "kb:x-old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parent with --lineage sees %v, want %v", got, want)
	}
}

func TestLineageOtherPrincipalSeesNothing(t *testing.T) {
	isolateRecallStores(t)
	dir := lineageRing(t)
	for _, l := range []Lineage{{Self: "ep-c", Parent: "ep-p"}, {Self: "ep-p", Descendants: true}} {
		if got := agentIDs(t, dir, "other", l); len(got) != 0 {
			t.Fatalf("other principal (%+v) saw the agent ring: %v", l, got)
		}
	}
}

// TestLineageCheckpointGate: an exported ancestor checkpoint is working memory
// for the child; a non-exported one stays hidden.
func TestLineageCheckpointGate(t *testing.T) {
	isolateRecallStores(t)
	dir := t.TempDir()
	store := kb.Open(dir)
	for _, p := range []*kb.Page{
		{Slug: "cp-shared", Form: kb.FormNote, Type: kb.TypeLesson, Title: "widget cp shared", Tags: []string{"checkpoint"}, Export: true,
			Source: &kb.Source{Tool: "owner", Episode: "ep-p"}},
		{Slug: "cp-private", Form: kb.FormNote, Type: kb.TypeLesson, Title: "widget cp private", Tags: []string{"checkpoint"},
			Source: &kb.Source{Tool: "owner", Episode: "ep-p"}},
	} {
		if err := store.Write(p, "add"); err != nil {
			t.Fatal(err)
		}
	}
	got := agentIDs(t, dir, "owner", Lineage{Self: "ep-c", Parent: "ep-p"})
	if !reflect.DeepEqual(got, []string{"kb:cp-shared"}) {
		t.Fatalf("child checkpoint view %v, want only the exported one", got)
	}
}

// TestContextRecitesWorkingNotesLast pins the recency rule: the episode's
// working notes are emitted after every other block, while ring precedence
// still orders everything else and selection is unchanged.
func TestContextRecitesWorkingNotesLast(t *testing.T) {
	isolateRecallStores(t)
	dir := lineageRing(t)
	q := Query{Text: "widget", K: 10, Rings: []string{RingAgent, RingRepo}, Forms: []string{kb.FormNote, kb.FormPage},
		Lineage: Lineage{Self: "ep-c", Parent: "ep-p"}}
	res := Context(q,
		AgentRing{Store: kb.OpenAgentRing(dir, "owner"), Path: dir},
		staticReader{ring: RingRepo, hits: []Hit{{Ring: RingRepo, ID: "kb:repo", Form: kb.FormPage, Cue: "widget repo", Score: 9}}})
	var refs []string
	for _, b := range res.Blocks {
		refs = append(refs, b.Ref)
	}
	if len(refs) != 4 {
		t.Fatalf("blocks %v, want 4", refs)
	}
	// agent (non-working) > repo, then the working notes last.
	if refs[0] != "kb:x-old" || refs[1] != "kb:repo" {
		t.Fatalf("precedence broken: %v", refs)
	}
	last := []string{refs[2], refs[3]}
	sort.Strings(last)
	if !reflect.DeepEqual(last, []string{"kb:c-own", "kb:p-exported"}) {
		t.Fatalf("working notes not last: %v", refs)
	}
	// Without a lineage nothing is working: historical order, nothing moves.
	q.Lineage = Lineage{}
	plain := Context(q, AgentRing{Store: kb.OpenAgentRing(dir, "owner"), Path: dir},
		staticReader{ring: RingRepo, hits: []Hit{{Ring: RingRepo, ID: "kb:repo", Form: kb.FormPage, Cue: "widget repo", Score: 9}}})
	if n := len(plain.Blocks); n == 0 || plain.Blocks[n-1].Ref != "kb:repo" {
		t.Fatalf("no-lineage order changed: %+v", plain.Blocks)
	}
}

// TestMemoryWorkingExtendsToExportedAncestors: the --memory working row reads
// the reader's episode plus its ancestors' exported notes, per the gate.
func TestMemoryWorkingExtendsToExportedAncestors(t *testing.T) {
	isolateRecallStores(t)
	dir := lineageRing(t)
	res := Context(Query{Text: "widget", K: 10, Memory: MemoryWorking, Lineage: Lineage{Self: "ep-c", Parent: "ep-p"}},
		AgentRing{Store: kb.OpenAgentRing(dir, "owner"), Path: dir})
	seen := map[string]bool{}
	for _, b := range res.Blocks {
		seen[b.Ref] = true
	}
	if !seen["kb:c-own"] || !seen["kb:p-exported"] || seen["kb:p-private"] || seen["kb:g-own"] {
		t.Fatalf("working view %v", seen)
	}
}

func TestReaderLineageFromEnv(t *testing.T) {
	t.Setenv("BASHY_EPISODE", "ep-c")
	t.Setenv("BASHY_PARENT_EPISODE", "ep-p")
	if got := readerLineage("", true); got != (Lineage{Self: "ep-c", Parent: "ep-p", Descendants: true}) {
		t.Fatalf("ambient lineage = %+v", got)
	}
	// An explicit foreign --episode does not inherit this process's parent.
	if got := readerLineage("ep-other", false); got != (Lineage{Self: "ep-other"}) {
		t.Fatalf("explicit lineage = %+v", got)
	}
}
