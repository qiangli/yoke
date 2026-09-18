package recall

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/craft"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/redact"
)

func isolateRecallStores(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BASHY_KB_DIR", filepath.Join(root, "host-kb"))
	t.Setenv("BASHY_HOME", filepath.Join(root, "bashy-home"))
	t.Setenv("BASHY_SKILLS_DIR", filepath.Join(root, "skills"))
	t.Setenv("YCODE_DATA_DIR", filepath.Join(root, "agent-data"))
	return root
}

// TestContextEnvelopeGolden is deliberately a round-trip through the exported
// envelope type. If a frozen field is renamed, removed, or assigned the wrong
// JSON shape, decoding and re-encoding can no longer reproduce the golden.
func TestContextEnvelopeGolden(t *testing.T) {
	isolateRecallStores(t)
	b, err := os.ReadFile("testdata/kb-context-envelope.json")
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	var envelope ContextResult
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatalf("golden does not fit ContextResult: %v", err)
	}
	gotBytes, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := json.Unmarshal(gotBytes, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ContextResult does not preserve the frozen envelope\n got: %s\nwant: %s", gotBytes, b)
	}
}

func TestContextBudgetAndRingPrecedence(t *testing.T) {
	isolateRecallStores(t)
	long := strings.Repeat("word ", 600)
	readers := []Reader{
		staticReader{ring: RingHost, hits: []Hit{{Ring: RingHost, ID: "kb:host", Form: kb.FormPage, Cue: "host widget", Gist: long, Full: long, Score: 1}}},
		staticReader{ring: RingRepo, hits: []Hit{{Ring: RingRepo, ID: "kb:repo", Form: kb.FormPage, Cue: "repo widget", Gist: long, Full: long, Score: 1}}},
		staticReader{ring: RingAgent, hits: []Hit{{Ring: RingAgent, ID: "kb:agent", Form: kb.FormNote, Cue: "agent widget", Gist: long, Full: long, Score: 1}}},
	}
	res := Context(Query{Text: "widget", Budget: 700, Forms: []string{kb.FormNote, kb.FormPage}}, readers...)
	if res.Budget.Used > 700 {
		t.Fatalf("used %d tokens over limit 700", res.Budget.Used)
	}
	if len(res.Blocks) != 3 {
		t.Fatalf("blocks = %d, want one per ring", len(res.Blocks))
	}
	if got := []string{res.Blocks[0].Ring, res.Blocks[1].Ring, res.Blocks[2].Ring}; !reflect.DeepEqual(got, []string{RingAgent, RingRepo, RingHost}) {
		t.Fatalf("presentation order = %v, want agent > repo > host", got)
	}
	for _, b := range res.Blocks {
		if b.Tokens <= 0 || b.Text == "" || b.Resolution == "" {
			t.Errorf("incomplete block: %+v", b)
		}
	}
}

func TestContextRelationFormReadsRelationRing(t *testing.T) {
	root := isolateRecallStores(t)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repoKB := filepath.Join(repo, kb.RepoSub)
	if err := os.MkdirAll(repoKB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kb.RelationPath(repoKB), []byte(`{"id":"r1","op":"link","target":"kb:alpha","relation":"about","dst":"todo:123"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	res := Context(Query{Text: "alpha", Rings: []string{RingRepo}, Forms: []string{kb.FormRelation}, Budget: 100}, openContextRings()...)
	if len(res.Blocks) != 1 {
		t.Fatalf("relation context blocks = %d: %+v", len(res.Blocks), res)
	}
	if res.Blocks[0].Form != kb.FormRelation || res.Blocks[0].Ref != "graph:r1" {
		t.Fatalf("wrong relation block: %+v", res.Blocks[0])
	}
}

func TestContextAbstainsWithEmptyBlocks(t *testing.T) {
	isolateRecallStores(t)
	res := Context(Query{Text: "unknown", MinCoverage: 0.9, Forms: []string{kb.FormPage}},
		staticReader{ring: RingRepo})
	if !res.Abstained || len(res.Blocks) != 0 {
		t.Fatalf("abstention = %v blocks=%d, want true and empty", res.Abstained, len(res.Blocks))
	}
}

func TestContextEpisodeOnlyAdmitsNamedAgentCheckpoint(t *testing.T) {
	isolateRecallStores(t)
	hits := []Hit{
		{Ring: RingAgent, ID: "kb:ordinary", Form: kb.FormNote, Cue: "widget ordinary", Score: 2},
		{Ring: RingAgent, ID: "kb:checkpoint", Form: kb.FormNote, Cue: "widget checkpoint", Score: 1, Episode: "ep-7", Checkpoint: true},
	}
	without := Context(Query{Text: "widget", Forms: []string{kb.FormNote}}, staticReader{ring: RingAgent, hits: hits})
	if len(without.Blocks) != 1 || without.Blocks[0].Ref != "kb:ordinary" {
		t.Fatalf("unnamed episode exposed checkpoint: %+v", without.Blocks)
	}
	with := Context(Query{Text: "widget", Episode: "ep-7", Forms: []string{kb.FormNote}}, staticReader{ring: RingAgent, hits: hits})
	if len(with.Blocks) != 2 {
		t.Fatalf("named episode returned %d blocks, want ordinary + checkpoint", len(with.Blocks))
	}
}

func TestContextCommandMissingRingNamesOpenedPath(t *testing.T) {
	root := isolateRecallStores(t)
	missing := filepath.Join(root, "absent-host-ring")
	t.Setenv("BASHY_KB_DIR", missing)
	cmd := NewContextCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--for", "widget", "--rings", "host", "--forms", "page", "--json"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("missing requested ring exited successfully")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not name opened path %q", err, missing)
	}
	var got ContextResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not the context envelope: %v\n%s", err, stdout.String())
	}
	if len(got.Rings) != 1 || got.Rings[0].OK || !strings.Contains(got.Rings[0].Error, missing) {
		t.Fatalf("ring status = %+v", got.Rings)
	}
}

func TestContextCommandRefusesUnavailableForms(t *testing.T) {
	isolateRecallStores(t)
	// relation became available when C5 landed RelationRing; code stays
	// refused until the CodeRing reader is injected at bashy's mount (C6).
	for _, form := range []string{kb.FormCode, "bogus"} {
		cmd := NewContextCmd()
		cmd.SetArgs([]string{"--for", "widget", "--rings", "repo", "--forms", form})
		err := cmd.Execute()
		if err == nil || err.Error() != `kb: invalid form "`+form+`" (note|page|relation|code)` {
			t.Errorf("--forms %s error = %v", form, err)
		}
	}
}

func TestOpenRingsReadsFoldsFromSkillsStore(t *testing.T) {
	isolateRecallStores(t)
	dir := os.Getenv("BASHY_SKILLS_DIR")
	folds := craft.OpenFolds(dir, redact.New())
	now := time.Now().UTC()
	if err := folds.Record(craft.Fold{Coordinate: "test-coordinate", Note: "widget restart procedure", Evidence: "gate passed", ObservedAt: now, ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	var capability Reader
	for _, rd := range openRings("") {
		if rd.Ring() == RingCapability {
			capability = rd
		}
	}
	if capability == nil {
		t.Fatal("openRings did not open folds written in BASHY_SKILLS_DIR")
	}
	hits, err := capability.Recall(Query{Text: "widget restart"})
	if err != nil || len(hits) == 0 {
		t.Fatalf("capability recall: hits=%d err=%v", len(hits), err)
	}
}

func TestRepoAndAgentRingsReadTheirStoresAndAgentIsOwnerOnly(t *testing.T) {
	root := isolateRecallStores(t)
	repoDir := filepath.Join(root, "repo", kb.RepoSub)
	agentDir := filepath.Join(os.Getenv("YCODE_DATA_DIR"), "kb")
	for _, tc := range []struct {
		dir  string
		page *kb.Page
	}{
		{repoDir, &kb.Page{Slug: "repo-widget", Form: kb.FormPage, Type: kb.TypeLesson, Title: "repo widget", Description: "repo widget procedure", Status: kb.StatusValidated}},
		{agentDir, &kb.Page{Slug: "agent-widget", Form: kb.FormNote, Type: kb.TypeLesson, Title: "agent widget", Description: "agent widget note", Status: kb.StatusCandidate, Source: &kb.Source{Tool: "owner"}}},
	} {
		if err := kb.Open(tc.dir).Write(tc.page, "add"); err != nil {
			t.Fatal(err)
		}
	}
	repoHits, err := (RepoRing{Store: kb.Open(repoDir), Path: repoDir}).Recall(Query{Text: "widget", Forms: []string{kb.FormPage}})
	if err != nil || len(repoHits) != 1 || repoHits[0].Ring != RingRepo {
		t.Fatalf("repo hits=%+v err=%v", repoHits, err)
	}
	ownerHits, err := (AgentRing{Store: kb.OpenAgentRing(agentDir, "owner"), Path: agentDir}).Recall(Query{Text: "widget", Forms: []string{kb.FormNote}})
	if err != nil || len(ownerHits) != 1 {
		t.Fatalf("owner agent hits=%+v err=%v", ownerHits, err)
	}
	otherHits, err := (AgentRing{Store: kb.OpenAgentRing(agentDir, "other"), Path: agentDir}).Recall(Query{Text: "widget", Forms: []string{kb.FormNote}})
	if err != nil || len(otherHits) != 0 {
		t.Fatalf("other principal saw owner records: hits=%+v err=%v", otherHits, err)
	}
}

func TestContextChoosesLargestResolutionThatFits(t *testing.T) {
	isolateRecallStores(t)
	h := Hit{Ring: RingRepo, ID: "kb:widget", Form: kb.FormPage, Cue: "widget cue", Gist: strings.Repeat("line ", 20), Full: strings.Repeat("body ", 200), Score: 1}
	line := blockAt(h, "line")
	res := Context(Query{Text: "widget", Forms: []string{kb.FormPage}, Budget: line.Tokens}, staticReader{ring: RingRepo, hits: []Hit{h}})
	if len(res.Blocks) != 1 || res.Blocks[0].Resolution != "line" {
		t.Fatalf("resolution under line-sized budget = %+v", res.Blocks)
	}
	full := Context(Query{Text: "widget", Forms: []string{kb.FormPage}}, staticReader{ring: RingRepo, hits: []Hit{h}})
	if len(full.Blocks) != 1 || full.Blocks[0].Resolution != "full" {
		t.Fatalf("unlimited resolution = %+v", full.Blocks)
	}
}

func TestContextKCapsAllReadersInOneRing(t *testing.T) {
	isolateRecallStores(t)
	res := Context(Query{Text: "widget", K: 2, Forms: []string{kb.FormNote, kb.FormPage}},
		staticReader{ring: RingRepo, hits: []Hit{
			{Ring: RingRepo, Form: kb.FormPage, ID: "kb:page-a", Cue: "widget page a", Score: 4},
			{Ring: RingRepo, Form: kb.FormPage, ID: "kb:page-b", Cue: "widget page b", Score: 3},
		}},
		formStaticReader{ring: RingRepo, forms: []string{kb.FormNote}, hits: []Hit{
			{Ring: RingRepo, Form: kb.FormNote, ID: "kb:note-a", Cue: "widget note a", Score: 2},
			{Ring: RingRepo, Form: kb.FormNote, ID: "kb:note-b", Cue: "widget note b", Score: 1},
		}})
	if len(res.Blocks) != 2 {
		t.Fatalf("two readers in one ring returned %d blocks with k=2", len(res.Blocks))
	}
}

func TestAgentRingCheckpointRequiresMatchingEpisode(t *testing.T) {
	isolateRecallStores(t)
	dir := filepath.Join(os.Getenv("YCODE_DATA_DIR"), "kb")
	store := kb.Open(dir)
	for _, p := range []*kb.Page{
		{Slug: "ordinary", Form: kb.FormNote, Type: kb.TypeLesson, Title: "widget ordinary", Status: kb.StatusCandidate, Source: &kb.Source{Tool: "owner"}},
		{Slug: "checkpoint", Form: kb.FormNote, Type: kb.TypeLesson, Title: "widget checkpoint", Tags: []string{"checkpoint"}, Status: kb.StatusCandidate, Source: &kb.Source{Tool: "owner", Episode: "ep-7"}},
	} {
		if err := store.Write(p, "add"); err != nil {
			t.Fatal(err)
		}
	}
	ring := AgentRing{Store: kb.OpenAgentRing(dir, "owner"), Path: dir}
	without, err := ring.Recall(Query{Text: "widget", Forms: []string{kb.FormNote}})
	if err != nil || len(without) != 1 || without[0].ID != "kb:ordinary" {
		t.Fatalf("unnamed episode hits=%+v err=%v", without, err)
	}
	with, err := ring.Recall(Query{Text: "widget", Episode: "ep-7", Forms: []string{kb.FormNote}})
	if err != nil || len(with) != 2 {
		t.Fatalf("named episode hits=%+v err=%v", with, err)
	}
}

func TestContextCommandInjectsReaderAndPassesFiles(t *testing.T) {
	isolateRecallStores(t)
	rd := &capturingReader{ring: RingRepo, forms: []string{kb.FormCode}, hits: []Hit{{Ring: RingRepo, Form: kb.FormCode, ID: "code:symbol", Cue: "symbol", Score: 1}}}
	cmd := NewContextCmd(rd)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--for", "widget", "--rings", "repo", "--forms", "code", "--files", "a.go,b.go", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rd.query.Files, []string{"a.go", "b.go"}) {
		t.Fatalf("injected reader files = %v", rd.query.Files)
	}
	var got ContextResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Blocks) != 1 || got.Blocks[0].Form != kb.FormCode || got.Blocks[0].Ref != "code:symbol" {
		t.Fatalf("injected block = %+v", got.Blocks)
	}
}

func TestContextTextRendersBulletsAndBrokenReaderStatus(t *testing.T) {
	isolateRecallStores(t)
	res := Context(Query{Text: "widget", Rings: []string{RingRepo, RingHost}, Forms: []string{kb.FormPage}},
		staticReader{ring: RingRepo, hits: []Hit{{Ring: RingRepo, Form: kb.FormPage, ID: "kb:widget", Cue: "widget", Score: 1}}},
		staticReader{ring: RingHost, err: errors.New("kb: open scratch/host: denied")})
	var out bytes.Buffer
	renderContext(&out, res)
	if !strings.Contains(out.String(), "- [repo/page]") || !strings.Contains(out.String(), "kb:widget") {
		t.Fatalf("text output is not citation bullets: %q", out.String())
	}
	if len(res.Rings) != 2 || res.Rings[1].OK || !strings.Contains(res.Rings[1].Error, "scratch/host") {
		t.Fatalf("rings = %+v", res.Rings)
	}
}

type staticReader struct {
	ring string
	hits []Hit
	err  error
}

func (r staticReader) Ring() string    { return r.ring }
func (r staticReader) Forms() []string { return []string{kb.FormNote, kb.FormPage} }
func (r staticReader) Recall(Query) ([]Hit, error) {
	if r.err != nil {
		return nil, r.err
	}
	return append([]Hit(nil), r.hits...), nil
}

type capturingReader struct {
	ring  string
	forms []string
	hits  []Hit
	query Query
}

type formStaticReader struct {
	ring  string
	forms []string
	hits  []Hit
}

func (r formStaticReader) Ring() string    { return r.ring }
func (r formStaticReader) Forms() []string { return r.forms }
func (r formStaticReader) Recall(Query) ([]Hit, error) {
	return append([]Hit(nil), r.hits...), nil
}

func (r *capturingReader) Ring() string    { return r.ring }
func (r *capturingReader) Forms() []string { return r.forms }
func (r *capturingReader) Recall(q Query) ([]Hit, error) {
	r.query = q
	return append([]Hit(nil), r.hits...), nil
}
