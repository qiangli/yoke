package recall

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/kbmemory"
)

func stubMemoryCall(hits []kbmemory.Hit, fresh kbmemory.Freshness) func(context.Context, kbmemory.RetrieveQuery) kbmemory.Envelope {
	return func(_ context.Context, q kbmemory.RetrieveQuery) kbmemory.Envelope {
		return kbmemory.Envelope{SchemaVersion: kbmemory.RetrievalSchema, OK: true, Op: kbmemory.RetrievalOp,
			Result: kbmemory.RetrieveResult{Hits: hits, Freshness: fresh, Mode: kbmemory.ModeLexical}}
	}
}

func memoryFixtureHits() []kbmemory.Hit {
	return []kbmemory.Hit{
		{Ref: "mem:alpha", Score: 2.5, Mode: "lexical", Revision: 7,
			Record: kbmemory.Record{ID: "alpha", Form: kb.FormNote, Type: "lesson",
				Title: "alpha widget", Body: "alpha widget body text", Status: "validated",
				Updated: "2026-10-01T00:00:00Z"},
			Why: []string{"title"}},
		{Ref: "mem:beta", Score: 1.5, Mode: "lexical", Revision: 3, IndexRevision: "fp-9", Stale: true,
			Record: kbmemory.Record{ID: "beta", Form: kb.FormPage, Type: "lesson",
				Title: "beta widget", Body: "beta widget body text", Status: "candidate",
				Updated: "2026-10-02T00:00:00Z"},
			Why: []string{"body"}},
	}
}

func memoryFixtureFresh() kbmemory.Freshness {
	return kbmemory.Freshness{DocTarget: "doctarget-1", IndexTarget: "indextarget-1",
		SourceRevision: "fp-a", IndexRevision: "fp-a"}
}

func memorySrc(call func(context.Context, kbmemory.RetrieveQuery) kbmemory.Envelope) MemorySource {
	return MemorySource{Target: "doctarget-1", Index: "indextarget-1", Call: call}
}

func findBlock(blocks []ContextBlock, ref string) *ContextBlock {
	for i := range blocks {
		if blocks[i].Ref == ref {
			return &blocks[i]
		}
	}
	return nil
}

func TestMemoryContextServesProvenanceBlocks(t *testing.T) {
	isolateRecallStores(t)
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}},
		memorySrc(stubMemoryCall(memoryFixtureHits(), memoryFixtureFresh())))
	if len(res.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2: %+v", len(res.Blocks), res)
	}
	a := findBlock(res.Blocks, "mem:alpha")
	if a == nil {
		t.Fatalf("missing mem:alpha in %+v", res.Blocks)
	}
	if len(a.Source) != 1 || a.Source[0].URI != "mem:alpha" || a.Source[0].Region != "" {
		t.Errorf("source = %+v, want verbatim record ID as URI, no region", a.Source)
	}
	if len(a.Why) == 0 || a.Why[0] != "memory" {
		t.Errorf("why = %v, want memory-first retrieval reasons", a.Why)
	}
	if a.Retrieval == nil || a.Retrieval.Mode != "lexical" || a.Retrieval.Score != 2.5 || a.Retrieval.Revision != 7 {
		t.Errorf("retrieval = %+v, want lexical/2.5/rev 7", a.Retrieval)
	}
	if !a.Retrieval.Estimate {
		t.Errorf("retrieval.Estimate = false, want true labelling Tokens as estimate")
	}
	if a.Retrieval.Target != "doctarget-1" {
		t.Errorf("retrieval.Target = %q, want canonical doctarget-1", a.Retrieval.Target)
	}
	if a.Text == "" || a.Tokens <= 0 {
		t.Errorf("block is not prompt-ready: %+v", a)
	}
	b := findBlock(res.Blocks, "mem:beta")
	if b == nil || b.Retrieval == nil || !b.Retrieval.Stale || b.Retrieval.IndexRevision != "fp-9" {
		t.Errorf("stale provenance lost: %+v", b)
	}
	if res.Freshness == nil || res.Freshness.DocTarget != "doctarget-1" || res.Freshness.IndexTarget != "indextarget-1" {
		t.Errorf("freshness = %+v", res.Freshness)
	}
	if res.Truncation != nil {
		t.Errorf("untruncated read carries truncation: %+v", res.Truncation)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"retrieval"`, `"freshness"`, `"estimate":true`, `"target":"doctarget-1"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("JSON envelope missing %s: %s", key, raw)
		}
	}
	if strings.Contains(string(raw), `"truncation"`) {
		t.Errorf("untruncated JSON carries truncation: %s", raw)
	}
	if strings.Contains(string(raw), `"memory_error"`) {
		t.Errorf("successful JSON carries memory_error: %s", raw)
	}
}

func TestMemoryContextKeepsMemoryAndLegacyIdentities(t *testing.T) {
	isolateRecallStores(t)
	// A kb-ring hit sharing a memory ref string is an unrelated record
	// from a distinct target: there is no post-assembly (ring, ref)
	// collapse, so both survive. Only pre-budget (target, record ID)
	// dedup collapses, and that covers memory hits alone.
	legacy := staticReader{ring: RingAgent, hits: []Hit{
		{Ring: RingAgent, ID: "mem:alpha", Form: kb.FormNote, Cue: "legacy alpha", Score: 9}}}
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}},
		memorySrc(stubMemoryCall(memoryFixtureHits(), memoryFixtureFresh())), legacy)
	survived := 0
	memoryLabelled := 0
	for _, b := range res.Blocks {
		if b.Ring == RingAgent && b.Ref == "mem:alpha" {
			survived++
			if b.Retrieval != nil {
				memoryLabelled++
			}
		}
	}
	if survived != 2 || memoryLabelled != 1 {
		t.Fatalf("agent/memi:alpha survived %d (memory-labelled %d), want 2/1: %+v",
			survived, memoryLabelled, res.Blocks)
	}
}

func TestMemoryContextDedupesBeforeBudget(t *testing.T) {
	isolateRecallStores(t)
	dupes := append(memoryFixtureHits(), memoryFixtureHits()[0])
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}},
		memorySrc(stubMemoryCall(dupes, memoryFixtureFresh())))
	count := 0
	for _, b := range res.Blocks {
		if b.Ref == "mem:alpha" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate served ref survived %d times: %+v", count, res.Blocks)
	}
	if res.Truncation != nil {
		t.Fatalf("deduped duplicate reported as cut: %+v", res.Truncation)
	}
}

func TestMemoryContextKeepsUnrelatedIdentities(t *testing.T) {
	isolateRecallStores(t)
	// Same ref string under another ring names an unrelated record: the
	// post-assembly identity is (ring, ref), so both survive.
	legacy := staticReader{ring: RingRepo, hits: []Hit{
		{Ring: RingRepo, ID: "mem:alpha", Form: kb.FormPage, Cue: "repo alpha", Score: 9}}}
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}},
		memorySrc(stubMemoryCall(memoryFixtureHits(), memoryFixtureFresh())), legacy)
	seen := map[string]int{}
	for _, b := range res.Blocks {
		seen[b.Ring+"\x00"+b.Ref]++
	}
	if seen[RingAgent+"\x00mem:alpha"] != 1 || seen[RingRepo+"\x00mem:alpha"] != 1 {
		t.Fatalf("unrelated identities collapsed: %+v", res.Blocks)
	}
}

func TestMemoryContextRespectsBudget(t *testing.T) {
	isolateRecallStores(t)
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Budget: 10, Forms: []string{kb.FormNote, kb.FormPage}},
		memorySrc(stubMemoryCall(memoryFixtureHits(), memoryFixtureFresh())))
	if res.Budget.Used > 10 {
		t.Fatalf("used %d over budget 10", res.Budget.Used)
	}
}

func TestMemoryContextReportsTruncation(t *testing.T) {
	isolateRecallStores(t)
	fresh := memoryFixtureFresh()
	fresh.Truncated = true
	fresh.Examined = 42
	fresh.DroppedStale = 3
	fresh.DroppedState = 1
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}},
		memorySrc(stubMemoryCall(memoryFixtureHits(), fresh)))
	if res.Truncation == nil || !res.Truncation.Truncated || res.Truncation.Examined != 42 ||
		res.Truncation.DroppedStale != 3 || res.Truncation.DroppedState != 1 {
		t.Fatalf("truncation = %+v", res.Truncation)
	}
}

func TestMemoryContextReportsAssemblerOmissions(t *testing.T) {
	isolateRecallStores(t)
	// The retrieval was NOT cut, but the assembler budget leaves a served
	// hit out: truncation must report the omission anyway.
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Budget: 10, Forms: []string{kb.FormNote, kb.FormPage}},
		memorySrc(stubMemoryCall(memoryFixtureHits(), memoryFixtureFresh())))
	if res.Truncation == nil || !res.Truncation.Truncated {
		t.Fatalf("budget cut carries no truncation: %+v", res.Truncation)
	}
	if res.Truncation.Omitted < 1 {
		t.Fatalf("truncation omits the assembler omission: %+v", res.Truncation)
	}
	if res.Budget.Used > 10 {
		t.Fatalf("used %d over budget 10", res.Budget.Used)
	}
}

func TestMemoryContextPassesBothOwnerModes(t *testing.T) {
	isolateRecallStores(t)
	for _, tc := range []struct {
		name    string
		src     MemorySource
		session string
	}{
		{"persistent-direct", MemorySource{Target: "doctarget-1", Index: "indextarget-1", Dir: "/tmp/kb"}, ""},
		{"explicit-owner", MemorySource{Target: "doctarget-1", Index: "indextarget-1", Session: "owner-7"}, "owner-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got kbmemory.RetrieveQuery
			src := tc.src
			src.Call = func(_ context.Context, q kbmemory.RetrieveQuery) kbmemory.Envelope {
				got = q
				return stubMemoryCall(memoryFixtureHits(), memoryFixtureFresh())(context.Background(), q)
			}
			res := MemoryContext(context.Background(),
				Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}}, src)
			if len(res.Blocks) != 2 {
				t.Fatalf("blocks = %d: %+v", len(res.Blocks), res)
			}
			if got.Session != tc.session || got.Target != "doctarget-1" || got.Index != "indextarget-1" {
				t.Errorf("retrieve query = %+v", got)
			}
			if got.Mode != "" && got.Mode != kbmemory.ModeLexical {
				t.Errorf("mode = %q, want lexical default", got.Mode)
			}
		})
	}
}

func TestMemoryContextExplicitFailureWithholdsLegacy(t *testing.T) {
	isolateRecallStores(t)
	// The collection is explicit, so a retrieval failure fails the packet:
	// no successful legacy-only fallback, and the structured code rides
	// along for the CLI/MCP exit.
	legacy := staticReader{ring: RingRepo, hits: []Hit{
		{Ring: RingRepo, ID: "kb:repo", Form: kb.FormPage, Cue: "repo widget", Score: 1}}}
	src := memorySrc(func(_ context.Context, _ kbmemory.RetrieveQuery) kbmemory.Envelope {
		return kbmemory.Envelope{OK: false, Op: kbmemory.RetrievalOp,
			Error: &kbmemory.ErrorBody{Code: "not_found", Message: "no memory collection"}}
	})
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}}, src, legacy)
	if len(res.Blocks) != 0 {
		t.Fatalf("failed explicit read served blocks: %+v", res.Blocks)
	}
	if res.Freshness != nil || res.Truncation != nil {
		t.Fatalf("failed read reports provenance: %+v", res)
	}
	if res.MemoryError == nil || res.MemoryError.Code != "not_found" {
		t.Fatalf("structured code lost: %+v", res.MemoryError)
	}
	broken := false
	for _, r := range res.Rings {
		if r.Name == "agent" && !r.OK && strings.Contains(r.Error, "not_found") {
			broken = true
		}
	}
	if !broken {
		t.Fatalf("memory ring failure not recorded: %+v", res.Rings)
	}
	// The legacy ring still ran, so its status stays honest.
	foundRepo := false
	for _, r := range res.Rings {
		if r.Name == RingRepo && r.OK {
			foundRepo = true
		}
	}
	if !foundRepo {
		t.Fatalf("legacy ring status lost: %+v", res.Rings)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	// §10.3 admits no top-level error field: the failed packet's envelope
	// stays frozen and the structured code rides the internal state plus
	// the ring error, never a marshalled memory_error member.
	if strings.Contains(string(raw), `"memory_error"`) {
		t.Errorf("failed JSON carries memory_error: %s", raw)
	}
}

func TestMemoryContextMapsForbiddenScopeToStructuredCode(t *testing.T) {
	isolateRecallStores(t)
	for _, tc := range []struct {
		code string
		exit int
	}{
		{"busy", 1},
		{"claim_held", 9},
		{"timeout", 124},
		{"cancelled", 130},
		{"unsupported", 2},
		{"not_found", 1},
	} {
		t.Run(tc.code, func(t *testing.T) {
			src := memorySrc(func(_ context.Context, _ kbmemory.RetrieveQuery) kbmemory.Envelope {
				return kbmemory.Envelope{OK: false, Op: kbmemory.RetrievalOp,
					Error: &kbmemory.ErrorBody{Code: tc.code, Message: "refused " + tc.code}}
			})
			res := MemoryContext(context.Background(),
				Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}}, src)
			if res.MemoryError == nil || res.MemoryError.Code != tc.code {
				t.Fatalf("code = %+v, want %q", res.MemoryError, tc.code)
			}
			if len(res.Blocks) != 0 {
				t.Fatalf("refused read served blocks: %+v", res.Blocks)
			}
			if got := MemoryExitCode(tc.code); got != tc.exit {
				t.Fatalf("exit = %d, want %d for %q", got, tc.exit, tc.code)
			}
		})
	}
}

func TestMemoryContextRefusesUnselectedRing(t *testing.T) {
	isolateRecallStores(t)
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Rings: []string{RingRepo}, Budget: 500,
			Forms: []string{kb.FormNote, kb.FormPage}},
		memorySrc(stubMemoryCall(memoryFixtureHits(), memoryFixtureFresh())))
	if len(res.Blocks) != 0 {
		t.Fatalf("unselected ring served blocks: %+v", res.Blocks)
	}
	found := false
	for _, r := range res.Rings {
		if !r.OK && strings.Contains(r.Error, "not selected") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ring refusal not recorded: %+v", res.Rings)
	}
}

func TestMemoryContextPropagatesCancellation(t *testing.T) {
	isolateRecallStores(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := memorySrc(func(ctx context.Context, _ kbmemory.RetrieveQuery) kbmemory.Envelope {
		if err := ctx.Err(); err != nil {
			return kbmemory.Envelope{OK: false, Op: kbmemory.RetrievalOp,
				Error: &kbmemory.ErrorBody{Code: "cancelled", Message: err.Error()}}
		}
		return stubMemoryCall(memoryFixtureHits(), memoryFixtureFresh())(ctx, kbmemory.RetrieveQuery{})
	})
	res := MemoryContext(ctx,
		Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}}, src)
	if len(res.Blocks) != 0 || res.Freshness != nil {
		t.Fatalf("cancelled read served data: %+v", res)
	}
	if res.MemoryError == nil || res.MemoryError.Code != "cancelled" {
		t.Fatalf("cancellation code lost: %+v", res.MemoryError)
	}
	if got := MemoryExitCode(res.MemoryError.Code); got != 130 {
		t.Fatalf("cancelled exit = %d, want 130", got)
	}
	found := false
	for _, r := range res.Rings {
		if !r.OK && strings.Contains(r.Error, "cancelled") {
			found = true
		}
	}
	if !found {
		t.Fatalf("cancellation not recorded: %+v", res.Rings)
	}
}

func TestMemoryContextLeavesLegacyEnvelopeUnchanged(t *testing.T) {
	isolateRecallStores(t)
	res := Context(Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}},
		staticReader{ring: RingRepo, hits: []Hit{
			{Ring: RingRepo, ID: "kb:repo", Form: kb.FormPage, Cue: "repo widget", Score: 1}}})
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"retrieval"`, `"freshness"`, `"truncation"`, `"memory_error"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("legacy envelope carries %s: %s", key, raw)
		}
	}
	if res.Freshness != nil || res.Truncation != nil || res.MemoryError != nil {
		t.Fatalf("legacy result has additive fields: %+v", res)
	}
}

func TestMemoryContextLabelsEstimateInPromptText(t *testing.T) {
	isolateRecallStores(t)
	res := MemoryContext(context.Background(),
		Query{Text: "widget", Budget: 500, Forms: []string{kb.FormNote, kb.FormPage}},
		memorySrc(stubMemoryCall(memoryFixtureHits(), memoryFixtureFresh())))
	var out strings.Builder
	renderContext(&out, res)
	for _, ref := range []string{"mem:alpha", "mem:beta"} {
		if !strings.Contains(out.String(), ref) {
			t.Fatalf("prompt text hides %s:\n%s", ref, out.String())
		}
	}
	if !strings.Contains(out.String(), "tokens estimate") {
		t.Fatalf("new-path blocks lack the estimate label:\n%s", out.String())
	}
	// Legacy lines carry no estimate label.
	legacy := Context(Query{Text: "widget", Budget: 500, Forms: []string{kb.FormPage}},
		staticReader{ring: RingRepo, hits: []Hit{
			{Ring: RingRepo, ID: "kb:repo", Form: kb.FormPage, Cue: "repo widget", Score: 1}}})
	var lout strings.Builder
	renderContext(&lout, legacy)
	if strings.Contains(lout.String(), "estimate") {
		t.Fatalf("legacy text carries estimate label:\n%s", lout.String())
	}
}

func TestContextCommandRegistersMemoryCollectionFlags(t *testing.T) {
	cmd := NewContextCmd()
	for _, name := range []string{"memory-target", "memory-index", "memory-session", "memory-dir",
		"memory-mode", "memory-tags", "memory-repos", "memory-os", "memory-since",
		"memory-until", "memory-wait", "memory-timeout"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("kb context lacks --%s", name)
		}
	}
	// Target without index is a caller error: indexes are never provisioned.
	isolateRecallStores(t)
	cmd = NewContextCmd()
	cmd.SetArgs([]string{"--for", "widget", "--memory-target", "doctarget-1"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--memory-index") {
		t.Fatalf("target-without-index error = %v", err)
	}
}
