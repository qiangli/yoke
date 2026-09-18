package kb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

func TestParseLinks(t *testing.T) {
	body := `Start with a bare [[pkill-guard]] and an explicit [[kb:restart-check]].
File against [[todo:a5f5cfc8]] under [[sprint:163]].
Markdown: see [the note](pages/never-pkill.md) and [the task](../todo/deadbeef-fix-it.md).
Ignore [external](https://example.com/x.md) and [anchor](#section) and [non-md](pages/x.txt).`

	got := ParseLinks(body)
	want := map[string]LinkKind{
		"kb:pkill-guard":   LinkKB,
		"kb:restart-check": LinkKB,
		"todo:a5f5cfc8":    LinkTodo,
		"sprint:163":       LinkSprint,
		"kb:never-pkill":   LinkKB,
		"todo:deadbeef":    LinkTodo,
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d links, want %d: %+v", len(got), len(want), got)
	}
	for _, l := range got {
		wk, ok := want[l.Ref()]
		if !ok {
			t.Errorf("unexpected link %q", l.Ref())
			continue
		}
		if l.Kind != wk {
			t.Errorf("%q: kind %q, want %q", l.Ref(), l.Kind, wk)
		}
	}
}

// TestParseLinksEveryKind is the S168.1 gate: the prose parser yields EVERY
// kind in the ref vocabulary, in both spellings, and an unknown scheme is
// classified (LinkUnknown) rather than dropped or filed as a kb slug.
func TestParseLinksEveryKind(t *testing.T) {
	var b strings.Builder
	for _, k := range ref.Kinds() {
		fmt.Fprintf(&b, "see [[%s:x-%s]] and [[urn:dhnt:%s:y-%s]]\n", k, k, k, k)
	}
	got := ParseLinks(b.String())
	byRef := map[string]Link{}
	for _, l := range got {
		byRef[l.Ref()] = l
	}
	for _, k := range ref.Kinds() {
		for _, id := range []string{"x-" + string(k), "y-" + string(k)} {
			l, ok := byRef[string(k)+":"+id]
			if !ok {
				t.Errorf("kind %q: link %s:%s not parsed (have %v)", k, k, id, keys(byRef))
				continue
			}
			if l.Kind != k || l.Target != id {
				t.Errorf("kind %q: got %+v", k, l)
			}
		}
	}
	if len(got) != 2*len(ref.Kinds()) {
		t.Errorf("parsed %d links, want %d", len(got), 2*len(ref.Kinds()))
	}

	// urn:dhnt: is a SPELLING, not a different link: the two dedupe to one.
	same := ParseLinks("[[kb:one]] and [[urn:dhnt:kb:one]]")
	if len(same) != 1 || same[0].Ref() != "kb:one" {
		t.Errorf("urn spelling did not collapse: %+v", same)
	}

	// Unknown schemes: classified, never dropped, never a slug. A tool:model
	// binding written in brackets is the realistic case.
	unk := ParseLinks("[[note: remember this]] [[codex:gpt5.6-sol]] [[urn:dhnt:issue:12]] [[kb:]]")
	if len(unk) != 3 {
		t.Fatalf("unknown schemes: got %d links %+v, want 3 (the empty-id [[kb:]] is not a link)", len(unk), unk)
	}
	for _, l := range unk {
		if l.Kind != LinkUnknown {
			t.Errorf("%+v: want LinkUnknown", l)
		}
		if l.Kind == LinkKB {
			t.Errorf("%+v: an unknown scheme was misfiled as a kb slug", l)
		}
	}
	if unk[0].Target != "note: remember this" || unk[1].Target != "codex:gpt5.6-sol" {
		t.Errorf("unknown targets should keep the inner text: %+v", unk)
	}
	// The # tolerance the parser always had survives the move to pkg/ref.
	if h := ParseLinks("[[todo:#abc123]] [[sprint:#7]]"); len(h) != 2 || h[0].Target != "abc123" || h[1].Target != "7" {
		t.Errorf("# tolerance: %+v", h)
	}
}

func keys(m map[string]Link) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestDoctorUnknownKindIsReportedNotDropped: before the vocabulary, an unknown
// scheme was a dangling kb slug; now it is its own class. The row must MOVE,
// not vanish — and the sprint link stays external (not dangling) as before.
func TestDoctorUnknownKindIsReportedNotDropped(t *testing.T) {
	p := &Page{Slug: "a", Title: "A", Body: "cites [[note: remember]] and [[sprint:9]] and [[kb:missing]]"}
	rep := Doctor([]*Page{p}, nil, nil, false)
	if len(rep.UnknownKind) != 1 || rep.UnknownKind[0].Target != "note: remember" || rep.UnknownKind[0].From != "kb:a" {
		t.Errorf("unknown_kind = %+v", rep.UnknownKind)
	}
	if len(rep.Dangling) != 1 || rep.Dangling[0].Target != "kb:missing" {
		t.Errorf("dangling = %+v (sprint must be external, the unknown scheme must not be here)", rep.Dangling)
	}
	if rep.Clean() {
		t.Error("a report with an unknown-kind link is not clean")
	}
}

// mkRepoKB makes a temp repo with a docs/kb store and a docs/todo dir, returning
// the kb store dir so a CLI run can point --dir at it and the sibling todo dir
// is discoverable by walking up to the .git marker.
func mkRepoKB(t *testing.T) (kbDir, todoDir string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	kbDir = filepath.Join(root, "docs", "kb")
	todoDir = filepath.Join(root, "docs", "todo")
	if err := os.MkdirAll(todoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return kbDir, todoDir
}

func writeTodo(t *testing.T, todoDir, id, title, body string) {
	t.Helper()
	content := "---\nid: " + id + "\ntitle: " + title + "\nstatus: open\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(todoDir, id+"-slug.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBacklinksResolvesTodoToKB is the story gate: a todo whose body cites a kb
// page is reported by `kb backlinks`.
func TestBacklinksResolvesTodoToKB(t *testing.T) {
	kbDir, todoDir := mkRepoKB(t)
	mustRun(t, kbDir, "add", "--title", "pkill guard", "--slug", "pkill-guard",
		"--description", "WHEN tempted to kill a stuck process", "--body", "find the pid first")
	writeTodo(t, todoDir, "a5f5cfc81287", "wire the guard", "Implements [[kb:pkill-guard]] for the mesh.")

	out := mustRun(t, kbDir, "backlinks", "--json", "pkill-guard")
	var payload struct {
		Slug      string `json:"slug"`
		Backlinks []struct {
			Ref   string `json:"ref"`
			Kind  string `json:"kind"`
			Title string `json:"title"`
		} `json:"backlinks"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if len(payload.Backlinks) != 1 {
		t.Fatalf("want 1 backlink, got %d: %+v", len(payload.Backlinks), payload.Backlinks)
	}
	if b := payload.Backlinks[0]; b.Kind != "todo" || b.Ref != "todo:a5f5cfc81287" {
		t.Fatalf("unexpected backlink: %+v", b)
	}
}

// TestDoctorDanglingLeavesBodyByteIdentical is the story gate: a body with a
// dangling [[kb:x]] is reported by doctor AND left byte-identical (doctor flags,
// never fixes).
func TestDoctorDanglingLeavesBodyByteIdentical(t *testing.T) {
	dir := t.TempDir()
	mustRun(t, dir, "add", "--title", "alpha", "--slug", "alpha",
		"--description", "d", "--body", "this cites [[kb:missing]] which does not exist")

	path := filepath.Join(dir, "pages", "alpha.md")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	out := mustRun(t, dir, "doctor", "--json")
	var rep DoctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	found := false
	for _, d := range rep.Dangling {
		if d.From == "kb:alpha" && d.Target == "kb:missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dangling [[kb:missing]] not reported: %+v", rep.Dangling)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("doctor rewrote the body:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDoctorOrphanDuplicateFormDescription(t *testing.T) {
	dir := t.TempDir()
	store := Open(dir)

	// A validated, linked page is neither orphan nor flagged.
	if err := store.Write(&Page{Slug: "anchor", Form: FormPage, Type: TypeLesson,
		Title: "Anchor", Description: "anchored", Status: StatusValidated,
		Body: "links to [[kb:sidecar]]"}, "add"); err != nil {
		t.Fatal(err)
	}
	// sidecar is cited by anchor (inbound) so it is not an orphan.
	if err := store.Write(&Page{Slug: "sidecar", Form: FormPage, Type: TypeLesson,
		Title: "Sidecar", Description: "", Status: StatusCandidate}, "add"); err != nil {
		t.Fatal(err)
	}
	// An orphan: candidate, no links in or out.
	if err := store.Write(&Page{Slug: "lonely", Form: FormPage, Type: TypeLesson,
		Title: "Lonely", Description: "nobody cites me", Status: StatusCandidate}, "add"); err != nil {
		t.Fatal(err)
	}
	// Near-duplicates of each other.
	if err := store.Write(&Page{Slug: "dup-one", Form: FormPage, Type: TypeLesson,
		Title: "stop a stuck process on a paired host", Description: "x", Status: StatusCandidate}, "add"); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(&Page{Slug: "dup-two", Form: FormPage, Type: TypeLesson,
		Title: "stop a stuck process on a paired host", Description: "y", Status: StatusCandidate}, "add"); err != nil {
		t.Fatal(err)
	}
	// A legacy record on disk with no form: key.
	legacy := "---\ntype: lesson\ntitle: Legacy\ndescription: old page\nstatus: candidate\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "pages", "legacy.md"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	pages, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	rep := Doctor(pages, store, nil, false)

	if !slices.Contains(rep.Orphans, "lonely") {
		t.Errorf("lonely should be an orphan: %+v", rep.Orphans)
	}
	if slices.Contains(rep.Orphans, "sidecar") {
		t.Errorf("sidecar has an inbound link, must not be an orphan: %+v", rep.Orphans)
	}
	if slices.Contains(rep.Orphans, "anchor") {
		t.Errorf("anchor is validated, must not be an orphan: %+v", rep.Orphans)
	}
	if !slices.Contains(rep.MissingDescription, "sidecar") {
		t.Errorf("sidecar is missing a description: %+v", rep.MissingDescription)
	}
	if !slices.Contains(rep.MissingForm, "legacy") {
		t.Errorf("legacy has no form: key: %+v", rep.MissingForm)
	}
	if slices.Contains(rep.MissingForm, "anchor") {
		t.Errorf("anchor declares form:, must not be flagged: %+v", rep.MissingForm)
	}
	foundDup := false
	for _, p := range rep.NearDuplicates {
		if p.A == "dup-one" && p.B == "dup-two" {
			foundDup = true
		}
	}
	if !foundDup {
		t.Errorf("dup-one/dup-two near-duplicate pair not reported: %+v", rep.NearDuplicates)
	}
}
