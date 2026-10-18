package recall

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/craft"
	"github.com/qiangli/yoke/pkg/kb"
)

// TestMemoryExpansionPinsEveryRow pins the §3 expansion table: each --memory
// TYPE resolves to exactly its facet combination.
func TestMemoryExpansionPinsEveryRow(t *testing.T) {
	isolateRecallStores(t)
	for _, tc := range []struct {
		memory          string
		wantRings       []string
		wantForms       []string
		wantTypes       []string
		wantForeignOnly bool
	}{
		{MemoryWorking, []string{RingAgent}, []string{kb.FormNote}, nil, false},
		{MemoryEpisodic, nil, nil, nil, true},
		{MemorySemantic, []string{RingAgent, RingRepo, RingHost},
			[]string{kb.FormPage, kb.FormNote, kb.FormRelation},
			[]string{kb.TypeFact, kb.TypeDecision}, false},
		{MemoryProcedural, []string{RingAgent, RingRepo, RingHost, RingCapability},
			[]string{kb.FormPage, kb.FormNote},
			[]string{kb.TypeRunbook}, false},
		{MemoryReflective, []string{RingAgent, RingRepo, RingHost},
			[]string{kb.FormPage, kb.FormNote},
			[]string{kb.TypeLesson, kb.TypeGotcha}, false},
		{MemoryProfile, []string{RingHost, RingAgent},
			[]string{kb.FormPage, kb.FormNote},
			[]string{kb.TypeFact, kb.TypeDecision}, false},
		{MemoryCode, nil, []string{kb.FormCode}, nil, false},
	} {
		t.Run(tc.memory, func(t *testing.T) {
			q, err := ApplyMemory(Query{Text: "widget", Memory: tc.memory})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(q.Rings, tc.wantRings) {
				t.Errorf("rings = %v, want %v", q.Rings, tc.wantRings)
			}
			if !reflect.DeepEqual(q.Forms, tc.wantForms) {
				t.Errorf("forms = %v, want %v", q.Forms, tc.wantForms)
			}
			if !reflect.DeepEqual(q.Types, tc.wantTypes) {
				t.Errorf("types = %v, want %v", q.Types, tc.wantTypes)
			}
			if tc.wantForeignOnly {
				res := Context(Query{Text: "widget", Memory: tc.memory},
					staticReader{ring: RingRepo, hits: []Hit{{Ring: RingRepo, ID: "kb:x", Form: kb.FormPage, Cue: "widget", Score: 1}}})
				if len(res.Blocks) != 0 {
					t.Errorf("episodic consulted kb readers: %+v", res.Blocks)
				}
				if res.Memory != tc.memory {
					t.Errorf("episodic envelope memory = %q, want echo %q", res.Memory, tc.memory)
				}
			}
		})
	}
}

// TestMemoryUnknownValueIsAnError pins the unknown-value error: anything
// outside the seven types fails and lists all seven.
func TestMemoryUnknownValueIsAnError(t *testing.T) {
	isolateRecallStores(t)
	if _, err := ApplyMemory(Query{Text: "widget", Memory: "dreams"}); err == nil {
		t.Fatal("unknown memory type did not error")
	} else {
		for _, want := range MemoryTypes() {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not list %q", err, want)
			}
		}
	}
	recallCmd := NewRecallCmd()
	recallCmd.SetArgs([]string{"widget", "--memory", "dreams"})
	if err := recallCmd.Execute(); err == nil {
		t.Error("recall --memory dreams did not error")
	} else {
		for _, want := range MemoryTypes() {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("recall error %q does not list %q", err, want)
			}
		}
	}
	contextCmd := NewContextCmd()
	contextCmd.SetArgs([]string{"--for", "widget", "--memory", "dreams"})
	if err := contextCmd.Execute(); err == nil {
		t.Error("context --memory dreams did not error")
	} else {
		for _, want := range MemoryTypes() {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("context error %q does not list %q", err, want)
			}
		}
	}
}

// TestMemoryExplicitNarrowsByIntersection pins the intersection rule:
// explicit --rings/--forms narrow the expansion, and an empty intersection
// is the normal empty answer, never an error.
func TestMemoryExplicitNarrowsByIntersection(t *testing.T) {
	isolateRecallStores(t)
	q, err := ApplyMemory(Query{Text: "widget", Memory: MemorySemantic, Rings: []string{RingRepo}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(q.Rings, []string{RingRepo}) {
		t.Fatalf("narrowed rings = %v, want [repo]", q.Rings)
	}
	// Empty intersection: semantic admits page|note|relation, so an explicit
	// code form matches nothing — exit 0 with no hits, not an error. A real
	// store exercises the reader-side form filter on the Recall path.
	empty, err := ApplyMemory(Query{Text: "widget", Memory: MemorySemantic, Forms: []string{kb.FormCode}})
	if err != nil {
		t.Fatal(err)
	}
	store := kbWith(t, t.TempDir(), page("widget", "widget lesson", "a lesson about widgets"))
	res := Recall(empty, HostRing{Store: store})
	if len(res.Hits) != 0 || len(res.Errors) != 0 {
		t.Fatalf("empty intersection = %+v errors=%+v, want no hits and no errors", res.Hits, res.Errors)
	}
	ctx := Context(empty, staticReader{ring: RingAgent, hits: []Hit{
		{Ring: RingAgent, ID: "kb:note", Form: kb.FormNote, Cue: "widget note", Score: 1},
	}})
	if len(ctx.Blocks) != 0 {
		t.Fatalf("empty intersection context blocks = %+v, want none", ctx.Blocks)
	}
	if ctx.Memory != MemorySemantic {
		t.Fatalf("empty intersection context memory = %q, want echo %q", ctx.Memory, MemorySemantic)
	}
}

// TestMemoryWorkingReadsEpisodeCheckpointNotes pins working = agent ring,
// note form, exactly --episode.
func TestMemoryWorkingReadsEpisodeCheckpointNotes(t *testing.T) {
	isolateRecallStores(t)
	hits := []Hit{
		{Ring: RingAgent, ID: "kb:ordinary", Form: kb.FormNote, Cue: "widget ordinary", Score: 2},
		{Ring: RingAgent, ID: "kb:checkpoint", Form: kb.FormNote, Cue: "widget checkpoint", Score: 1, Episode: "ep-7", Checkpoint: true},
		{Ring: RingRepo, ID: "kb:repo", Form: kb.FormPage, Cue: "widget repo", Score: 3},
	}
	res := Context(Query{Text: "widget", Memory: MemoryWorking, Episode: "ep-7"},
		staticReader{ring: RingAgent, hits: hits[:2]},
		staticReader{ring: RingRepo, hits: hits[2:]})
	if len(res.Blocks) != 2 {
		t.Fatalf("working with episode returned %d blocks, want ordinary + checkpoint: %+v", len(res.Blocks), res.Blocks)
	}
	for _, b := range res.Blocks {
		if b.Memory != MemoryWorking {
			t.Errorf("block %s memory = %q, want working", b.Ref, b.Memory)
		}
		if b.Ring != RingAgent || b.Form != kb.FormNote {
			t.Errorf("working block escaped its facets: %+v", b)
		}
	}
}

// TestMemoryTypeFiltersEndToEnd pins the Type half of the table against real
// stores: semantic keeps fact|decision, reflective keeps lesson|gotcha,
// procedural keeps runbooks plus capability folds.
func TestMemoryTypeFiltersEndToEnd(t *testing.T) {
	isolateRecallStores(t)
	dir := t.TempDir()
	store := kb.Open(dir)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, p := range []*kb.Page{
		{Slug: "fact", Type: kb.TypeFact, Title: "widget fact", Description: "a widget fact", Status: kb.StatusValidated, Created: now, Updated: now},
		{Slug: "decision", Type: kb.TypeDecision, Title: "widget decision", Description: "a widget decision", Status: kb.StatusValidated, Created: now, Updated: now},
		{Slug: "lesson", Type: kb.TypeLesson, Title: "widget lesson", Description: "a widget lesson", Status: kb.StatusValidated, Created: now, Updated: now},
		{Slug: "gotcha", Type: kb.TypeGotcha, Title: "widget gotcha", Description: "a widget gotcha", Status: kb.StatusValidated, Created: now, Updated: now},
		{Slug: "runbook", Type: kb.TypeRunbook, Title: "widget runbook", Description: "a widget runbook", Status: kb.StatusValidated, Created: now, Updated: now},
	} {
		if err := store.Write(p, "add"); err != nil {
			t.Fatal(err)
		}
	}
	folds := foldsWith(t, dir, craft.Fold{Coordinate: "c1", Note: "widget restart procedure"})
	recall := func(memory string) []Hit {
		return Recall(Query{Text: "widget", Memory: memory},
			HostRing{Store: store}, CapabilityRing{Folds: folds}).Hits
	}
	ids := func(hits []Hit) map[string]bool {
		out := map[string]bool{}
		for _, h := range hits {
			out[h.ID] = true
		}
		return out
	}
	semantic := ids(recall(MemorySemantic))
	for _, want := range []string{"kb:fact", "kb:decision"} {
		if !semantic[want] {
			t.Errorf("semantic missing %s: %v", want, semantic)
		}
	}
	for _, drop := range []string{"kb:lesson", "kb:gotcha", "kb:runbook"} {
		if semantic[drop] {
			t.Errorf("semantic leaked %s", drop)
		}
	}
	reflective := ids(recall(MemoryReflective))
	for _, want := range []string{"kb:lesson", "kb:gotcha"} {
		if !reflective[want] {
			t.Errorf("reflective missing %s: %v", want, reflective)
		}
	}
	for _, drop := range []string{"kb:fact", "kb:decision", "kb:runbook"} {
		if reflective[drop] {
			t.Errorf("reflective leaked %s", drop)
		}
	}
	procedural := ids(recall(MemoryProcedural))
	if !procedural["kb:runbook"] {
		t.Errorf("procedural missing kb:runbook: %v", procedural)
	}
	foundFold := false
	for id := range procedural {
		foundFold = foundFold || strings.HasPrefix(id, "craft:")
	}
	if !foundFold {
		t.Errorf("procedural missing the capability fold: %v", procedural)
	}
	for _, drop := range []string{"kb:fact", "kb:lesson"} {
		if procedural[drop] {
			t.Errorf("procedural leaked %s", drop)
		}
	}
	for _, hits := range [][]Hit{recall(MemorySemantic), recall(MemoryReflective), recall(MemoryProcedural)} {
		for _, h := range hits {
			if h.Memory == "" {
				t.Errorf("hit %s carries no memory echo", h.ID)
			}
		}
	}
}

// TestMemoryProfileStaysOnHostAndAgent pins profile = host,agent rings with
// fact|decision types: repo records never surface.
func TestMemoryProfileStaysOnHostAndAgent(t *testing.T) {
	isolateRecallStores(t)
	mk := func(ring string) staticReader {
		return staticReader{ring: ring, hits: []Hit{
			{Ring: ring, ID: "kb:" + ring + "-fact", Form: kb.FormPage, Cue: "widget convention", Score: 1},
		}}
	}
	res := Context(Query{Text: "widget", Memory: MemoryProfile},
		mk(RingHost), mk(RingAgent), mk(RingRepo))
	if len(res.Blocks) != 2 {
		t.Fatalf("profile blocks = %+v, want host + agent only", res.Blocks)
	}
	for _, b := range res.Blocks {
		if b.Ring == RingRepo {
			t.Errorf("profile leaked a repo block: %+v", b)
		}
	}
}

// TestMemoryCodeSelectsCodeForm pins code ≡ --forms code through an injected
// reader, and that an explicit page form intersects to the empty answer.
func TestMemoryCodeSelectsCodeForm(t *testing.T) {
	isolateRecallStores(t)
	rd := &capturingReader{ring: RingRepo, forms: []string{kb.FormCode}, hits: []Hit{
		{Ring: RingRepo, Form: kb.FormCode, ID: "code:symbol", Cue: "widget symbol", Score: 1},
	}}
	res := Context(Query{Text: "widget", Memory: MemoryCode}, rd)
	if len(res.Blocks) != 1 || res.Blocks[0].Form != kb.FormCode {
		t.Fatalf("code blocks = %+v, want the injected code hit", res.Blocks)
	}
	if res.Blocks[0].Memory != MemoryCode || res.Memory != MemoryCode {
		t.Fatalf("code echo missing: block %q envelope %q", res.Blocks[0].Memory, res.Memory)
	}
	narrowed, err := ApplyMemory(Query{Text: "widget", Memory: MemoryCode, Forms: []string{kb.FormPage}})
	if err != nil {
		t.Fatal(err)
	}
	if res := Context(narrowed, rd); len(res.Blocks) != 0 {
		t.Fatalf("code ∩ page returned %+v, want the empty answer", res.Blocks)
	}
}

// TestMemoryEnvelopeEcho pins the additive envelope fields: Hit.Memory,
// ContextResult.Memory, ContextBlock memory + observed_at, while valid_to,
// freshness, snapshot and path stay absent until a source carries them.
func TestMemoryEnvelopeEcho(t *testing.T) {
	isolateRecallStores(t)
	hit := Hit{Ring: RingRepo, ID: "kb:widget", Form: kb.FormPage, Cue: "widget cue",
		Gist: "widget gist", Score: 1, ObservedAt: "2026-09-13T08:14:00Z"}
	res := Context(Query{Text: "widget", Memory: MemorySemantic},
		staticReader{ring: RingRepo, hits: []Hit{hit}})
	if res.Memory != MemorySemantic {
		t.Fatalf("envelope memory = %q, want semantic", res.Memory)
	}
	if len(res.Blocks) != 1 {
		t.Fatalf("blocks = %+v", res.Blocks)
	}
	b := res.Blocks[0]
	if b.Memory != MemorySemantic {
		t.Errorf("block memory = %q, want semantic", b.Memory)
	}
	if b.ObservedAt != hit.ObservedAt {
		t.Errorf("block observed_at = %q, want %q", b.ObservedAt, hit.ObservedAt)
	}
	if b.ValidTo != "" || b.Freshness != "" || b.Snapshot != "" || len(b.Path) != 0 {
		t.Errorf("block emits reserved fields: %+v", b)
	}
	var raw map[string]any
	rawBytes, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawBytes, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["memory"] != MemorySemantic {
		t.Errorf("envelope JSON memory = %v", raw["memory"])
	}
	if raw["context_version"] != float64(1) {
		t.Errorf("context_version moved: %v", raw["context_version"])
	}
	// Without --memory the echo stays absent: the golden shape is unchanged.
	plain := Context(Query{Text: "widget"}, staticReader{ring: RingRepo, hits: []Hit{hit}})
	plainBytes, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainBytes), `"memory"`) {
		t.Errorf("plain envelope emits memory: %s", plainBytes)
	}
}
