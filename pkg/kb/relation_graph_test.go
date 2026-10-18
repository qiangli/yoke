package kb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRelations(t *testing.T, dir string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RelationPath(dir), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNewCoreRelationsRoundTrip: each new relation type written to graph.jsonl
// reads back intact, with deterministic entity ids, and is core (doctor does
// not report it as open vocabulary).
func TestNewCoreRelationsRoundTrip(t *testing.T) {
	for _, rel := range []string{"supports", "contradicts", "derived-from", "caused-by", "part-of"} {
		t.Run(rel, func(t *testing.T) {
			dir := t.TempDir()
			in := Relation{ID: "r-" + rel, Op: "link", Target: "kb:alpha", Relation: rel, Dst: "kb:beta"}
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			writeRelations(t, dir, string(b))
			live, err := (RelationRing{Dir: dir}).Live()
			if err != nil {
				t.Fatal(err)
			}
			if len(live) != 1 {
				t.Fatalf("want 1 relation, got %+v", live)
			}
			got := live[0]
			if got.ID != in.ID || got.Relation != rel || got.Target != in.Target || got.Dst != in.Dst {
				t.Fatalf("round-trip mismatch: %+v", got)
			}
			if got.TargetID != RelationEntityID("kb", "kb:alpha") || got.DstID != RelationEntityID("kb", "kb:beta") {
				t.Fatalf("entity ids changed: %+v", got)
			}
			if !CoreRelation(rel) {
				t.Fatalf("%s not core", rel)
			}
			if open := DoctorRelations(live); len(open) != 0 {
				t.Fatalf("core relation flagged open-vocabulary: %+v", open)
			}
		})
	}
}

// TestBacklinksShowRelationType: body links report relation "link"; graph
// relations whose dst is the page report their own type — an entity page is
// just the about target.
func TestBacklinksShowRelationType(t *testing.T) {
	isolateRelationStores(t)
	dir := t.TempDir()
	mustRun(t, dir, "add", "--title", "ollama entity", "--slug", "ollama",
		"--description", "WHEN reasoning about the ollama service", "--body", "the entity")
	mustRun(t, dir, "add", "--title", "pin the port", "--slug", "pin-port",
		"--description", "WHEN starting ollama", "--body", "see [[ollama]]")
	writeRelations(t, dir,
		`{"id":"r2","op":"link","target":"kb:pin-port","relation":"about","dst":"kb:ollama"}`,
		`{"id":"r1","op":"link","target":"todo:abc123","relation":"supports","dst":"urn:dhnt:kb:ollama"}`,
		`{"id":"r3","op":"link","target":"kb:pin-port","relation":"about","dst":"kb:other"}`,
	)

	out := mustRun(t, dir, "backlinks", "--json", "ollama")
	var payload struct {
		Backlinks []backlinkJSON `json:"backlinks"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	want := []backlinkJSON{
		{Ref: "kb:pin-port", Kind: "kb", Title: "pin the port", Relation: "link"},
		{Ref: "kb:pin-port", Kind: "kb", Title: "pin the port", Relation: "about"},
		{Ref: "todo:abc123", Kind: "todo", Relation: "supports"},
	}
	if len(payload.Backlinks) != len(want) {
		t.Fatalf("want %d backlinks, got %+v", len(want), payload.Backlinks)
	}
	for i := range want {
		if payload.Backlinks[i] != want[i] {
			t.Fatalf("backlink %d = %+v, want %+v", i, payload.Backlinks[i], want[i])
		}
	}

	text := mustRun(t, dir, "backlinks", "ollama")
	for _, line := range []string{"kb:pin-port\tlink\t", "kb:pin-port\tabout\t", "todo:abc123\tsupports\t"} {
		if !strings.Contains(text, line) {
			t.Fatalf("text backlinks missing %q:\n%s", line, text)
		}
	}
}

// TestDoctorFlagsOpenContradiction: a contradicts relation between two live
// pages is flagged; one whose endpoint was superseded (page status or a
// supersedes relation) is resolved and not flagged; nothing is rewritten.
func TestDoctorFlagsOpenContradiction(t *testing.T) {
	isolateRelationStores(t)
	dir := t.TempDir()
	mustRun(t, dir, "add", "--title", "use port one", "--slug", "port-one",
		"--description", "WHEN choosing the ollama port", "--body", "port 1")
	mustRun(t, dir, "add", "--title", "use port two", "--slug", "port-two",
		"--description", "WHEN picking a listener", "--body", "port 2")
	mustRun(t, dir, "add", "--title", "old advice", "--slug", "old-advice",
		"--description", "WHEN tuning the cache", "--body", "stale")
	mustRun(t, dir, "supersede", "old-advice", "--title", "new advice",
		"--description", "WHEN tuning the cache size today")
	writeRelations(t, dir,
		`{"id":"c-open","op":"link","target":"kb:port-one","relation":"contradicts","dst":"kb:port-two"}`,
		`{"id":"c-page","op":"link","target":"kb:old-advice","relation":"contradicts","dst":"kb:port-one"}`,
		`{"id":"c-rel","op":"link","target":"pkg/a","relation":"contradicts","dst":"pkg/b"}`,
		`{"id":"s1","op":"link","target":"pkg/c","relation":"supersedes","dst":"pkg/b"}`,
	)
	before, err := os.ReadFile(filepath.Join(dir, "pages", "port-one.md"))
	if err != nil {
		t.Fatal(err)
	}

	out := mustRun(t, dir, "doctor", "--json")
	var rep DoctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if len(rep.OpenContradictions) != 1 || rep.OpenContradictions[0].ID != "c-open" {
		t.Fatalf("want only c-open flagged, got %+v", rep.OpenContradictions)
	}
	if len(rep.OpenRelations) != 0 {
		t.Fatalf("contradicts/supersedes are core, got open-vocabulary %+v", rep.OpenRelations)
	}
	text := mustRun(t, dir, "doctor")
	if !strings.Contains(text, "open contradictions (1)") || !strings.Contains(text, "kb:port-one -contradicts-> kb:port-two") {
		t.Fatalf("doctor text missing contradiction:\n%s", text)
	}
	after, err := os.ReadFile(filepath.Join(dir, "pages", "port-one.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("doctor rewrote a page")
	}
}
