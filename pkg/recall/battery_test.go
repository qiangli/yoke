// Battery acceptance for Sprint 284 story 05c7c8f83654 (M7): one shared
// fixture exercising multi-hop, ask, visibility, validity, consolidate and
// determinism through the public Recall/Context/Consolidate surface.
// TESTS ONLY: no production code.
package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
)

// batteryConsolidateRules is a minimal deterministic rule set: two matchers
// over "subject = value" claims, so the shared fixture yields exactly two
// proposals with stable ids.
const batteryConsolidateRules = `
schema: bashy-kb-consolidate-rules-v1
claim_pattern: '^\s*([^\s=]+)\s*=\s*(\S(?:.*\S)?)\s*$'
unknown_values: [unknown, "?", n/a, none, "null"]
failure:
  kinds: [failure, error]
  outcomes: [fail, failed, error]
rules:
  - id: fact-from-repeated-claim
    version: 1
    match: repeated-claim
    min: 2
    type: fact
    title: "{{.Subject}} = {{.Value}}"
    body: "Claimed by {{.Count}} independent sources."
  - id: contradicts-differing-claim
    version: 1
    match: differing-claim
    type: gotcha
    title: "Contradicts: {{.Subject}} has differing values"
    body: "Flag only — no fact is changed."
`

// batteryFixture is the ONE shared fixture for the whole battery: a repo
// store (multi-hop pages, ask pages, lifecycle pages, consolidate claim
// pages, one contradicts link) plus an agent store (lineage notes for one
// owner, plus a hidden bridge node owned by another principal). Every
// subtest uses a disjoint vocabulary so subtests stay order-independent.
type batteryFixture struct {
	root     string
	repoDir  string
	agentDir string
	store    *kb.Store
	repo     []Reader
}

func buildBattery(t *testing.T) *batteryFixture {
	t.Helper()
	fx := &batteryFixture{root: t.TempDir()}
	fx.repoDir = fx.root
	fx.agentDir = filepath.Join(fx.root, "agent")
	fx.store = kb.Open(fx.repoDir)
	mk := func(p *kb.Page) {
		t.Helper()
		if err := fx.store.Write(p, "add"); err != nil {
			t.Fatalf("write %s: %v", p.Slug, err)
		}
	}

	// Multi-hop: the answer page shares no keywords with the query; only a
	// [[link]] connects them.
	mk(&kb.Page{Slug: "moss-queries", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "green moss garden queries", Description: "WHEN choosing moss for shade prefer cushion varieties",
		Status: kb.StatusValidated, Body: "Compare [[stone-answer]] before finalizing the bed."})
	mk(&kb.Page{Slug: "stone-answer", Form: kb.FormPage, Type: kb.TypeFact,
		Title: "granite load tables", Description: "Axial capacities for dry stone walls",
		Status: kb.StatusValidated, Body: "Granite carries the load."})

	// Validity: a superseded pair switching at lifeCut, plus a page that is
	// forgotten up front (recall hides it, the tombstone audit stays).
	old := page("widget-old", "widget retry policy", "WHEN widget calls fail retry 3 times")
	old.Form = kb.FormPage
	old.Status, old.SupersededBy, old.ValidTo = kb.StatusSuperseded, "widget-new", lifeCut.Format(time.RFC3339)
	mk(old)
	neu := page("widget-new", "widget retry policy revised", "WHEN widget calls fail retry with backoff")
	neu.Form = kb.FormPage
	neu.Supersedes, neu.ValidFrom = "widget-old", lifeCut.Format(time.RFC3339)
	mk(neu)
	fg := page("bat-forget-me", "widget token rotation", "WHEN widget auth fails")
	fg.Form = kb.FormPage
	fg.Body = "Rotate the widget token nightly."
	mk(fg)

	// Ask: two evidence pages under one question's vocabulary.
	mk(&kb.Page{Slug: "bat-ask-alpha", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "bataskalpha", Description: "bataskalpha bataskbeta",
		Status: kb.StatusValidated, Body: "bataskalpha bataskbeta evidence for the harbor question."})
	mk(&kb.Page{Slug: "bat-ask-beta", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "bataskbeta", Description: "bataskalpha bataskbeta",
		Status: kb.StatusValidated, Body: "bataskalpha bataskbeta second evidence for the harbor question."})
	contra := `{"id":"bat-contra","op":"link","target":"kb:bat-ask-alpha","relation":"contradicts","dst":"kb:bat-ask-beta"}` + "\n"
	if err := os.WriteFile(kb.RelationPath(fx.repoDir), []byte(contra), 0o644); err != nil {
		t.Fatal(err)
	}

	// Consolidate: one validated fact plus two candidate notes repeating a
	// differing claim. No other page title in this store is a claim.
	mk(&kb.Page{Slug: "bat-fact-turbine", Form: kb.FormPage, Type: kb.TypeFact,
		Title: "turbine.rpm = 4000", Description: "turbine baseline",
		Status: kb.StatusValidated, Body: "Measured baseline."})
	mk(&kb.Page{Slug: "bat-note-turbine-a", Form: kb.FormNote, Type: kb.TypeFact,
		Title: "turbine.rpm = 4600", Description: "turbine gauge reading",
		Status: kb.StatusCandidate, Body: "Saw 4600 on the gauge.",
		Source: &kb.Source{Tool: "codex", Episode: "ep-2"}})
	mk(&kb.Page{Slug: "bat-note-turbine-b", Form: kb.FormNote, Type: kb.TypeFact,
		Title: "turbine.rpm = 4600", Description: "turbine gauge reading again",
		Status: kb.StatusCandidate, Body: "Gauge still reads 4600.",
		Source: &kb.Source{Tool: "claude", Episode: "ep-4"}})

	loaded, err := fx.store.Load("bat-forget-me")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.Forget(loaded, "obsolete", time.Now().UTC().Truncate(time.Second)); err != nil {
		t.Fatal(err)
	}

	// Agent store: lineage notes for "owner" across ep-p → ep-c → ep-g,
	// plus a hidden bridge node owned by "other" between two visible nodes.
	agent := kb.Open(fx.agentDir)
	amk := func(p *kb.Page) {
		t.Helper()
		if err := agent.Write(p, "add"); err != nil {
			t.Fatalf("write agent %s: %v", p.Slug, err)
		}
	}
	anote := func(slug, ep, parent string, export bool) *kb.Page {
		return &kb.Page{Slug: slug, Form: kb.FormNote, Type: kb.TypeLesson,
			Title: "widget " + slug, Description: "widget note " + slug,
			Status: kb.StatusCandidate, Export: export,
			Source: &kb.Source{Tool: "owner", Episode: ep, ParentEpisode: parent}}
	}
	for _, p := range []*kb.Page{
		anote("bl-p-private", "ep-p", "", false),
		anote("bl-p-exported", "ep-p", "", true),
		anote("bl-c-own", "ep-c", "ep-p", false),
		anote("bl-g-own", "ep-g", "ep-c", false),
		anote("bl-x-old", "ep-x", "", false),
		{Slug: "bb-alpha", Form: kb.FormNote, Type: kb.TypeLesson,
			Title: "northwind departure query", Description: "WHEN northwind runs prefer the northern route",
			Status: kb.StatusValidated, Source: &kb.Source{Tool: "owner"}},
		{Slug: "bb-beta", Form: kb.FormNote, Type: kb.TypeLesson,
			Title: "cobalt arrival answer", Description: "Zebra quilt xenon verdict",
			Status: kb.StatusValidated, Source: &kb.Source{Tool: "owner"}},
		{Slug: "bb-bridge", Form: kb.FormNote, Type: kb.TypeLesson,
			Title: "harbormaster ledger", Description: "a foreign relay record",
			Status: kb.StatusValidated, Source: &kb.Source{Tool: "other"}},
	} {
		amk(p)
	}
	var b strings.Builder
	for _, r := range []kb.Relation{
		hopLink("bat-r1", "kb:bb-alpha", "about", "kb:bb-bridge", "other"),
		hopLink("bat-r2", "kb:bb-bridge", "about", "kb:bb-beta", "other"),
	} {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(kb.RelationPath(fx.agentDir), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	fx.repo = []Reader{
		RepoRing{Store: fx.store, Path: fx.repoDir},
		RelationRing{RingName: RingRepo, Path: fx.repoDir},
	}
	return fx
}

func batteryAgentReaders(fx *batteryFixture, owner string) []Reader {
	return []Reader{
		AgentRing{Store: kb.OpenAgentRing(fx.agentDir, owner), Path: fx.agentDir},
		RelationRing{RingName: RingAgent, Path: fx.agentDir, Owner: owner},
	}
}

func batterySortedIDs(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	sort.Strings(out)
	return out
}

func TestMemoryBattery(t *testing.T) {
	isolateRecallStores(t)
	fx := buildBattery(t)
	table := []struct {
		name string
		run  func(*testing.T, *batteryFixture)
	}{
		{"multihop", batteryMultihop},
		{"ask", batteryAsk},
		{"visibility", batteryVisibility},
		{"validity", batteryValidity},
		{"consolidate", batteryConsolidate},
		{"determinism", batteryDeterminism},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, fx) })
	}
}

// batteryMultihop: --hops 2 finds the answer page that shares no keywords
// with the query; --hops 0 does not.
func batteryMultihop(t *testing.T, fx *batteryFixture) {
	t.Helper()
	q := Query{Text: "moss shade garden", K: 3, Forms: []string{kb.FormPage}}
	if hitByID(Recall(q, fx.repo...).Hits, "kb:stone-answer") != nil {
		t.Fatal("hops 0 reaches the keywordless answer")
	}
	q.Hops = 2
	ans := hitByID(Recall(q, fx.repo...).Hits, "kb:stone-answer")
	if ans == nil {
		t.Fatal("hops 2 misses the keywordless answer")
	}
	want := []string{"kb:moss-queries", "link", "kb:stone-answer"}
	if strings.Join(ans.Path, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("answer path = %v, want %v", ans.Path, want)
	}
}

// batteryAsk: the final answer cites evidence; ask abstains on contradictory
// evidence and on missing evidence.
func batteryAsk(t *testing.T, fx *batteryFixture) {
	t.Helper()
	found := func(ref, ring string) kb.ShowEnvelope { return kb.ShowEnvelope{Ref: ref, Outcome: "found"} }

	hits := Recall(Query{Text: "bataskalpha bataskbeta", K: 3, Forms: []string{kb.FormPage}},
		HostRing{Store: fx.store, Path: fx.repoDir}).Hits
	if len(hits) != 2 {
		t.Fatalf("ask retrieval = %v, want both evidence pages", hitIDs(hits))
	}
	st := askState{Question: "bataskalpha bataskbeta", MaxRounds: 4, MinCoverage: .6, Blocking: map[string]string{}}
	for _, h := range hits {
		st.Evidence = append(st.Evidence, AskEvidence{Ref: h.ID, Ring: h.Ring, Text: h.Cue + " " + h.Gist + " " + h.Full, Path: []string{h.ID}})
	}
	final := finishAsk(&st, "battery", found, nil)
	if final.Abstained {
		t.Fatalf("supported answer abstained: %+v", final)
	}
	if len(final.Citations) != 2 {
		t.Fatalf("citations = %+v, want both evidence refs", final.Citations)
	}
	for _, c := range final.Citations {
		if !strings.Contains(final.Answer, "["+c.Ref+"]") {
			t.Fatalf("answer does not cite %s: %q", c.Ref, final.Answer)
		}
	}

	contra := askState{Question: "bataskalpha bataskbeta", MaxRounds: 4, MinCoverage: .6,
		Evidence: []AskEvidence{
			{Ref: "kb:bat-ask-alpha", Ring: RingHost, Text: "bataskalpha bataskbeta"},
			{Ref: "kb:bat-ask-beta", Ring: RingHost, Text: "bataskalpha bataskbeta"},
		}}
	creaders := []Reader{RelationRing{RingName: RingHost, Path: fx.repoDir}}
	got := finishAsk(&contra, "battery", found, creaders)
	if !got.Abstained || !slicesHas(got.Reasons, "contradiction") {
		t.Fatalf("contradictory evidence did not abstain: %+v", got)
	}
	if strings.Join(got.BlockingRefs, ",") != "kb:bat-ask-alpha,kb:bat-ask-beta" {
		t.Fatalf("blocking refs = %v, want both evidence pages", got.BlockingRefs)
	}

	empty := askState{Question: "unanswerable harbor fixture", MaxRounds: 4, MinCoverage: .6}
	neverShow := func(string, string) kb.ShowEnvelope { t.Fatal("empty answer must not cite"); return kb.ShowEnvelope{} }
	missing := finishAsk(&empty, "battery", neverShow, nil)
	if !missing.Abstained || !slicesHas(missing.Reasons, "insufficient-coverage") {
		t.Fatalf("missing evidence did not abstain: %+v", missing)
	}
}

// batteryVisibility: the child episode sees only exported ancestor notes; a
// second principal never sees the first principal's agent-ring notes; a
// hidden node bridging two visible nodes never appears in a --hops path,
// rank or citation.
func batteryVisibility(t *testing.T, fx *batteryFixture) {
	t.Helper()
	got := agentIDs(t, fx.agentDir, "owner", Lineage{Self: "ep-c", Parent: "ep-p"})
	want := []string{"kb:bl-c-own", "kb:bl-p-exported", "kb:bl-x-old"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("child sees %v, want %v (never a non-exported ancestor note)", got, want)
	}

	mine := Query{Text: "northwind harbormaster", K: 10, Forms: []string{kb.FormNote}}
	ownerHits, err := AgentRing{Store: kb.OpenAgentRing(fx.agentDir, "owner"), Path: fx.agentDir}.Recall(mine)
	if err != nil {
		t.Fatal(err)
	}
	otherHits, err := AgentRing{Store: kb.OpenAgentRing(fx.agentDir, "other"), Path: fx.agentDir}.Recall(mine)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(batterySortedIDs(ownerHits), ",") != "kb:bb-alpha" {
		t.Fatalf("owner sees %v, want only its own bridge endpoint", batterySortedIDs(ownerHits))
	}
	if strings.Join(batterySortedIDs(otherHits), ",") != "kb:bb-bridge" {
		t.Fatalf("other principal sees %v, want only its own bridge node", batterySortedIDs(otherHits))
	}

	owner := batteryAgentReaders(fx, "owner")
	walked := Recall(Query{Text: "northwind departure query", K: 5, Hops: 4}, owner...)
	if hitByID(walked.Hits, "kb:bb-alpha") == nil {
		t.Fatalf("owner misses its own seed: %v", hitIDs(walked.Hits))
	}
	if hitByID(walked.Hits, "kb:bb-beta") != nil {
		t.Fatalf("hidden bridge leaks the answer: %v", hitIDs(walked.Hits))
	}
	for _, h := range walked.Hits {
		if strings.Contains(h.ID, "bb-bridge") {
			t.Fatalf("hidden node ranked: %s", h.ID)
		}
		for _, e := range h.Path {
			if strings.Contains(e, "bb-bridge") {
				t.Fatalf("hidden node in path of %s: %v", h.ID, h.Path)
			}
		}
	}
	ctx := Context(Query{Text: "northwind departure query", K: 5, Forms: []string{kb.FormNote}, Hops: 4}, owner...)
	for _, b := range ctx.Blocks {
		if strings.Contains(b.Ref, "bb-bridge") {
			t.Fatalf("hidden node cited: %s", b.Ref)
		}
		for _, e := range b.Path {
			if strings.Contains(e, "bb-bridge") {
				t.Fatalf("hidden node in citation path of %s: %v", b.Ref, b.Path)
			}
		}
	}
	control := Recall(Query{Text: "harbormaster ledger", K: 5, Hops: 2}, batteryAgentReaders(fx, "other")...)
	if hitByID(control.Hits, "kb:bb-bridge") == nil {
		t.Fatalf("control principal misses its own bridge: %v", hitIDs(control.Hits))
	}
}

// batteryValidity: the superseded page is visible --as-of before the
// supersede and hidden after; the forgotten page is hidden from recall while
// its tombstone audit is kept.
func batteryValidity(t *testing.T, fx *batteryFixture) {
	t.Helper()
	host := HostRing{Store: fx.store}
	before := Recall(Query{Text: "widget retry", K: 5, AsOf: lifeCut.Add(-time.Hour)}, host)
	if got := ids(before.Hits); !strings.Contains(got, "kb:widget-old") || strings.Contains(got, "kb:widget-new") {
		t.Fatalf("as-of before supersede = %q, want the old page visible and the replacement hidden", got)
	}
	after := Recall(Query{Text: "widget retry", K: 5, AsOf: lifeCut.Add(time.Hour)}, host)
	if got := ids(after.Hits); !strings.Contains(got, "kb:widget-new") || strings.Contains(got, "kb:widget-old") {
		t.Fatalf("as-of after supersede = %q, want the replacement visible and the old page hidden", got)
	}

	now := Recall(Query{Text: "widget", K: 5}, host)
	if got := ids(now.Hits); !strings.Contains(got, "kb:widget-new") || strings.Contains(got, "bat-forget-me") {
		t.Fatalf("recall after forget = %q, want the replacement and never the forgotten page", got)
	}
	tombs, err := fx.store.Tombstones()
	if err != nil {
		t.Fatal(err)
	}
	tb, ok := tombs["bat-forget-me"]
	if !ok {
		t.Fatalf("forget audit lost: no tombstone for bat-forget-me (%v)", tombs)
	}
	if tb.Reason != "obsolete" {
		t.Fatalf("tombstone reason = %q, want the recorded forget reason", tb.Reason)
	}
}

// batteryConsolidate: proposals match the expected ids; rerunning yields the
// same ids with no new candidates; changing a source makes the proposal
// stale with an inspectable reason.
func batteryConsolidate(t *testing.T, fx *batteryFixture) {
	t.Helper()
	rules, err := kb.ParseConsolidateRules([]byte(batteryConsolidateRules))
	if err != nil {
		t.Fatal(err)
	}
	conIDs := func(rep kb.ConsolidateReport) []string {
		out := make([]string, 0, len(rep.Proposals))
		for _, it := range rep.Proposals {
			out = append(out, it.ID)
		}
		return out
	}
	countPages := func() int {
		pages, err := fx.store.List()
		if err != nil {
			t.Fatal(err)
		}
		return len(pages)
	}

	rep1, err := fx.store.Consolidate(rules, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep1.Proposals) != 2 {
		t.Fatalf("proposals = %v, want exactly the repeated-claim and differing-claim proposals", conIDs(rep1))
	}
	byRule := map[string]string{}
	for _, it := range rep1.Proposals {
		byRule[it.Rule] = it.ID
		if it.Status != "new" {
			t.Fatalf("fresh proposal %s status %q, want new", it.ID, it.Status)
		}
	}
	wantIDs := map[string]string{
		"fact-from-repeated-claim":    "2f1e9c083ea06a0c",
		"contradicts-differing-claim": "d3f27411136a675a",
	}
	for rule, want := range wantIDs {
		if byRule[rule] != want {
			t.Fatalf("rule %s proposal id = %q, want %q (all: %v)", rule, byRule[rule], want, conIDs(rep1))
		}
	}
	if len(rep1.Stale) != 0 {
		t.Fatalf("fresh store has stale proposals: %+v", rep1.Stale)
	}
	t.Logf("battery consolidate proposals=%v", conIDs(rep1))

	// Apply-equivalent: persist each proposal as a candidate carrying its
	// audit block, exactly like kb consolidate --apply does.
	for i := range rep1.Proposals {
		it := &rep1.Proposals[i]
		pr := it.Proposal
		cp := &kb.Page{Slug: it.Slug, Form: kb.FormNote, Type: it.Type, Title: it.Title,
			Description: "battery consolidate candidate (" + pr.Rule + ")",
			Status:      kb.StatusCandidate, Body: it.Body, Proposal: &pr}
		if err := fx.store.Write(cp, "consolidate"); err != nil {
			t.Fatal(err)
		}
	}

	// Rerun: same ids, all already on disk, nothing new written.
	n := countPages()
	rep2, err := fx.store.Consolidate(rules, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(conIDs(rep2), ",") != strings.Join(conIDs(rep1), ",") {
		t.Fatalf("rerun ids differ:\n%v\n%v", conIDs(rep2), conIDs(rep1))
	}
	for _, it := range rep2.Proposals {
		if it.Status != "exists" {
			t.Fatalf("rerun proposal %s status %q, want exists", it.ID, it.Status)
		}
	}
	if got := countPages(); got != n {
		t.Fatalf("rerun wrote %d new pages", got-n)
	}

	// Changing a source makes every proposal that read it stale, with an
	// inspectable reason naming the source and what changed.
	notePath := filepath.Join(fx.repoDir, "pages", "bat-note-turbine-a.md")
	raw, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), "Saw 4600 on the gauge.", "Saw 4600 on the panel meter.", 1)
	if changed == string(raw) {
		t.Fatal("source body to mutate not found")
	}
	if err := os.WriteFile(notePath, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	rep3, err := fx.store.Consolidate(rules, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	stale := map[string][]string{}
	for _, s := range rep3.Stale {
		stale[s.Slug] = s.Reasons
	}
	const wantReason = "kb:bat-note-turbine-a: changed since proposed"
	for _, it := range rep1.Proposals {
		rs, ok := stale[it.Slug]
		if !ok {
			t.Fatalf("proposal %s (%s) not stale after its source changed", it.ID, it.Rule)
		}
		if !slicesHas(rs, wantReason) {
			t.Fatalf("proposal %s stale reasons %q, want %q", it.ID, rs, wantReason)
		}
	}
}

// batteryDeterminism: recall --hops 2 and kb context are byte-identical
// across runs.
func batteryDeterminism(t *testing.T, fx *batteryFixture) {
	t.Helper()
	q := Query{Text: "moss shade garden", K: 3, Forms: []string{kb.FormPage}, Hops: 2}
	first, err := json.Marshal(Recall(q, fx.repo...))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(Recall(q, fx.repo...))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("recall output differs across runs:\n%s\n%s", first, second)
	}
	cfirst, err := json.Marshal(Context(q, fx.repo...))
	if err != nil {
		t.Fatal(err)
	}
	csecond, err := json.Marshal(Context(q, fx.repo...))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfirst) != string(csecond) {
		t.Fatal("context output differs across runs")
	}
}
