package kb

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// consolidateFixture writes a store by hand — fixed timestamps, no Store.Write
// — so every digest and proposal id is reproducible: pages (a validated fact,
// claim notes, a note written in a failed episode), journal observe events
// (gate failures, repeated claims, sense readings incl. a failed probe and an
// unknown value, post-action feedback) and one graph note.
func consolidateFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "kb")
	if err := os.MkdirAll(filepath.Join(dir, "pages"), 0o755); err != nil {
		t.Fatal(err)
	}
	pages := []*Page{
		{Slug: "fact-buildbox-cpus", Form: FormPage, Type: TypeFact, Title: "buildbox.cpus = 4",
			Description: "buildbox core count", Status: StatusValidated, Evidence: "nproc",
			Created: "2026-09-01T00:00:00Z", Body: "Measured with nproc."},
		{Slug: "note-region", Form: FormNote, Type: TypeFact, Title: "deploy.region = us-east-1",
			Status: StatusCandidate, Created: "2026-10-02T00:00:00Z", Body: "From the deploy log.",
			Source: &Source{Tool: "codex", Episode: "ep-2"}},
		{Slug: "note-cpus", Form: FormNote, Type: TypeFact, Title: "buildbox.cpus = 8",
			Status: StatusCandidate, Created: "2026-10-03T00:00:00Z", Body: "Saw 8 in top.",
			Source: &Source{Tool: "claude", Episode: "ep-5"}},
		{Slug: "note-dag-timeout", Form: FormNote, Type: TypeLesson, Title: "dag capacity tests need the package alone",
			Status: StatusCandidate, Created: "2026-10-01T11:00:00Z", Body: "Run pkg/dag alone; parallel suites starve the fixture worker.",
			Source: &Source{Tool: "claude", Episode: "ep-1"}},
	}
	for _, p := range pages {
		b, err := p.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pages", p.Slug+".md"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	journal := []string{
		`{"op":"add","slug":"fact-buildbox-cpus","tool":"claude","at":"2026-09-01T00:00:00Z"}`,
		`{"id":"g1","event":"observe","kind":"gate","episode":"ep-1","ref":"gate:dag","summary":"TestCapacity timed out","at":"2026-10-01T10:00:00Z","ran":true,"command":"go test ./pkg/dag","exit_code":1,"tool":"claude"}`,
		`{"id":"g2","event":"observe","kind":"tool-result","episode":"ep-3","ref":"gate:dag","summary":"TestCapacity timed out again","at":"2026-10-04T10:00:00Z","command":"go test ./pkg/dag","exit_code":2,"tool":"codex"}`,
		`{"id":"c1","event":"observe","kind":"note","episode":"ep-2","ref":"log:deploy-1","summary":"deploy.region = us-east-1","at":"2026-10-02T01:00:00Z","tool":"codex"}`,
		`{"id":"c2","event":"observe","kind":"note","episode":"ep-4","ref":"log:deploy-2","summary":"deploy.region = us-east-1","at":"2026-10-05T01:00:00Z","tool":"claude"}`,
		// sense: a contradictory reading, a stale unknown value, a failed probe
		`{"id":"s1","event":"observe","kind":"sense","episode":"ep-5","ref":"sense:cpu-0","summary":"buildbox.cpus = 8","at":"2026-10-10T00:00:00Z","tool":"claude"}`,
		`{"id":"s2","event":"observe","kind":"sense","episode":"ep-6","ref":"sense:cpu-0","summary":"buildbox.cpus = unknown","at":"2026-08-01T00:00:00Z","tool":"claude"}`,
		`{"id":"s3","event":"observe","kind":"sense","episode":"ep-6","ref":"sense:disk-0","summary":"probe failed: disk.free = unknown","at":"2026-10-11T00:00:00Z","exit_code":1,"tool":"claude"}`,
		// post-action feedback agreeing with the validated fact
		`{"id":"f1","event":"observe","kind":"tool-result","episode":"ep-7","ref":"action:resize","summary":"buildbox.cpus = 4","at":"2026-10-12T00:00:00Z","passed":true,"tool":"claude"}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(strings.Join(journal, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	graph := `{"id":"r1","op":"note","by":"codex","at":"2026-10-06T00:00:00Z","target":"deploy","text":"deploy.region = eu-west-1","episode":"ep-8"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, RelationFile), []byte(graph), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func pinConsolidateClock(t *testing.T) {
	t.Helper()
	// Far enough ahead that tombstones and supersedes written at real "now"
	// during the test resolve as in effect.
	consolidateClock = func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { consolidateClock = time.Now })
}

func snapshotPages(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(dir, "pages"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, "pages", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func consolidateJSON(t *testing.T, dir string, args ...string) (ConsolidateReport, string) {
	t.Helper()
	out := mustRun(t, dir, append([]string{"consolidate", "--json"}, args...)...)
	var rep ConsolidateReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("consolidate --json: %v\n%s", err, out)
	}
	return rep, out
}

func proposalIDs(rep ConsolidateReport) []string {
	var ids []string
	for _, p := range rep.Proposals {
		ids = append(ids, p.ID)
	}
	return ids
}

func TestConsolidateGoldenAndIdempotent(t *testing.T) {
	scratchStageEnv(t)
	pinConsolidateClock(t)
	dir := consolidateFixture(t)
	before := snapshotPages(t, dir)

	rep, out := consolidateJSON(t, dir)
	golden := filepath.Join("testdata", "kb-consolidate.json")
	if os.Getenv("KB_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("consolidate dry run differs from %s:\n%s", golden, out)
	}
	if _, again := consolidateJSON(t, dir); again != out {
		t.Fatal("dry run is not byte-stable across runs")
	}

	// The expected candidate set, by rule.
	kinds := map[string]int{}
	for _, p := range rep.Proposals {
		kinds[p.Rule]++
		if p.Status != "new" {
			t.Fatalf("fresh store: proposal %s status %q, want new", p.ID, p.Status)
		}
	}
	for rule, n := range map[string]int{
		"lesson-from-failure":         2, // go test ./pkg/dag (x2); the sense:disk-0 failed probe
		"fact-from-repeated-claim":    1, // deploy.region = us-east-1 (note + 2 events)
		"relation-supports":           1, // post-action feedback agrees with the validated fact
		"relation-caused-by":          1, // note-dag-timeout written in failed episode ep-1
		"contradicts-differing-claim": 2, // buildbox.cpus 4 vs 8; deploy.region us-east-1 vs eu-west-1
	} {
		if kinds[rule] != n {
			t.Fatalf("rule %s: %d proposals, want %d (all: %v)", rule, kinds[rule], n, kinds)
		}
	}
	if len(rep.Stale) != 0 {
		t.Fatalf("fresh store has stale proposals: %+v", rep.Stale)
	}
	if after := snapshotPages(t, dir); len(after) != len(before) {
		t.Fatal("dry run wrote pages")
	}

	// --apply writes every proposal as a candidate, touching no existing page.
	applied, _ := consolidateJSON(t, dir, "--apply")
	after := snapshotPages(t, dir)
	for name, b := range before {
		if after[name] != b {
			t.Fatalf("consolidate --apply changed %s:\n%s", name, after[name])
		}
	}
	if len(after) != len(before)+len(applied.Proposals) {
		t.Fatalf("apply wrote %d pages, want %d", len(after)-len(before), len(applied.Proposals))
	}
	store := Open(dir)
	for _, it := range applied.Proposals {
		p, err := store.Load(it.Slug)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != StatusCandidate || p.Proposal == nil || p.Proposal.ID != it.ID || p.Form != FormNote {
			t.Fatalf("%s: want a candidate note carrying proposal %s, got status=%s proposal=%+v", it.Slug, it.ID, p.Status, p.Proposal)
		}
	}

	// Rerun: same ids, all exist, nothing new written.
	rerun, _ := consolidateJSON(t, dir, "--apply")
	if strings.Join(proposalIDs(rerun), ",") != strings.Join(proposalIDs(rep), ",") {
		t.Fatalf("rerun ids differ:\n%v\n%v", proposalIDs(rerun), proposalIDs(rep))
	}
	for _, p := range rerun.Proposals {
		if p.Status != "exists" {
			t.Fatalf("rerun proposal %s status %q, want exists", p.ID, p.Status)
		}
	}
	if n := len(snapshotPages(t, dir)); n != len(after) {
		t.Fatalf("rerun wrote %d new pages", n-len(after))
	}
}

func TestConsolidateStaleSources(t *testing.T) {
	scratchStageEnv(t)
	pinConsolidateClock(t)
	dir := consolidateFixture(t)
	applied, _ := consolidateJSON(t, dir, "--apply")
	factBefore, _ := os.ReadFile(filepath.Join(dir, "pages", "fact-buildbox-cpus.md"))

	bySource := func(ref string) []ConsolidateItem {
		var out []ConsolidateItem
		for _, it := range applied.Proposals {
			for _, s := range it.Sources {
				if s.Ref == ref {
					out = append(out, it)
					break
				}
			}
		}
		return out
	}

	// changed: rewrite a source note's body in place.
	path := filepath.Join(dir, "pages", "note-cpus.md")
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(b, []byte("Saw 8 in top."), []byte("Saw 8 in htop."), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	// superseded and forgotten through the ordinary verbs.
	mustRun(t, dir, "supersede", "note-region", "--title", "deploy region moved", "--type", "fact", "--description", "region change")
	mustRun(t, dir, "forget", "note-dag-timeout", "--reason", "obsolete after dag rewrite")

	rep, _ := consolidateJSON(t, dir)
	stale := map[string][]string{}
	for _, s := range rep.Stale {
		stale[s.Slug] = s.Reasons
	}
	expect := map[string]string{
		"kb:note-cpus":        "kb:note-cpus: changed since proposed",
		"kb:note-region":      "kb:note-region: superseded (was valid)",
		"kb:note-dag-timeout": "kb:note-dag-timeout: forgotten (was valid): obsolete after dag rewrite",
	}
	for ref, reason := range expect {
		items := bySource(ref)
		if len(items) == 0 {
			t.Fatalf("fixture has no proposal sourced from %s", ref)
		}
		for _, it := range items {
			if !strings.Contains(strings.Join(stale[it.Slug], "\n"), reason) {
				t.Fatalf("%s (%s): stale reasons %q, want %q", it.Slug, it.Rule, stale[it.Slug], reason)
			}
		}
	}

	// No silent promotion: validate refuses a stale proposal, show explains it.
	victim := bySource("kb:note-dag-timeout")[0].Slug
	if _, err := run(t, dir, "", "validate", victim, "--evidence", "looked fine"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("validate of stale proposal: err=%v, want stale refusal", err)
	}
	if p, _ := Open(dir).Load(victim); p.Status != StatusCandidate {
		t.Fatalf("stale proposal status %s, want candidate", p.Status)
	}
	var env ShowEnvelope
	if err := json.Unmarshal([]byte(mustRun(t, dir, "show", victim, "--json")), &env); err != nil {
		t.Fatal(err)
	}
	if env.Proposal == nil || env.Proposal.State != "stale" || len(env.Proposal.Reasons) == 0 {
		t.Fatalf("kb show proposal = %+v, want stale with reasons", env.Proposal)
	}

	// The older validated fact is untouched by every contradicting reading.
	if factAfter, _ := os.ReadFile(filepath.Join(dir, "pages", "fact-buildbox-cpus.md")); !bytes.Equal(factBefore, factAfter) {
		t.Fatalf("validated fact changed:\n%s", factAfter)
	}
	// A still-fresh proposal stays pending and validates normally.
	var fresh string
	for _, it := range applied.Proposals {
		if len(stale[it.Slug]) == 0 {
			fresh = it.Slug
			break
		}
	}
	if fresh == "" {
		t.Fatal("expected at least one non-stale proposal")
	}
	if err := json.Unmarshal([]byte(mustRun(t, dir, "show", fresh, "--json")), &env); err != nil {
		t.Fatal(err)
	}
	if env.Proposal == nil || env.Proposal.State != "pending" {
		t.Fatalf("fresh proposal show = %+v, want pending", env.Proposal)
	}
}

func TestConsolidateSenseIsObservationOnly(t *testing.T) {
	scratchStageEnv(t)
	pinConsolidateClock(t)
	dir := consolidateFixture(t)
	rep, _ := consolidateJSON(t, dir)
	var cpuFlag *ConsolidateItem
	for i, it := range rep.Proposals {
		if it.Kind == "contradicts" && it.Subject == "buildbox.cpus" {
			cpuFlag = &rep.Proposals[i]
		}
		for _, s := range it.Sources {
			// The unknown-value reading is an observation of absence: no
			// claim, no flag, no fact.
			if s.Ref == "observe:s2" {
				t.Fatalf("unknown sense value produced proposal %s (%s)", it.ID, it.Rule)
			}
		}
	}
	if cpuFlag == nil {
		t.Fatal("contradictory sense reading raised no contradicts flag")
	}
	var cites bool
	for _, s := range cpuFlag.Sources {
		if s.Ref == "observe:s1" && s.ObservedAt == "2026-10-10T00:00:00Z" && s.Episode == "ep-5" {
			cites = true
		}
	}
	if !cites {
		t.Fatalf("flag does not cite the sense reading + observed_at: %+v", cpuFlag.Sources)
	}
}

func TestConsolidateRulesAreData(t *testing.T) {
	scratchStageEnv(t)
	pinConsolidateClock(t)
	dir := consolidateFixture(t)
	rules := `schema: bashy-kb-consolidate-rules-v1
claim_pattern: '^\s*([^\s=]+)\s*=\s*(\S(?:.*\S)?)\s*$'
rules:
  - id: only-failures
    version: 7
    match: failure
    min: 2
    type: gotcha
    title: "Twice failed: {{.Subject}}"
    body: "{{.Count}}"
`
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, _ := consolidateJSON(t, dir, "--rules", path)
	if len(rep.Proposals) != 1 || rep.Proposals[0].Title != "Twice failed: go test ./pkg/dag" || rep.Proposals[0].RuleVersion != 7 {
		t.Fatalf("custom rules: %+v", rep.Proposals)
	}
	// --since narrows the sources read: only the 2026-10-04 failure is recent.
	rep, _ = consolidateJSON(t, dir, "--rules", path, "--since", "2026-10-03")
	if len(rep.Proposals) != 0 {
		t.Fatalf("--since kept old failures: %+v", rep.Proposals)
	}
	if _, err := ParseConsolidateRules([]byte("schema: bashy-kb-consolidate-rules-v1\nclaim_pattern: '(a)=(b)'\nrules:\n  - {id: x, version: 1, match: nope, type: fact}\n")); err == nil {
		t.Fatal("unknown matcher accepted")
	}
}
