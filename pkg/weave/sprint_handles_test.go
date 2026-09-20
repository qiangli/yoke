package weave

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
)

// A card written before uuid/slug existed gets both on load — the same values
// on every load (a read-only verb must print a stable identity without
// writing) — and the next ordinary write persists them (D14).
func TestSprintHandlesMintedOnDemandDeterministicallyAndPersistedByTheNextWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", home)
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/plinth")
	created := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	legacy := `{"next_story_id":3,"stories":[
	  {"id":1,"title":"Team collaboration follow-up","column":"done","created":"` + created.Format(time.RFC3339Nano) + `"},
	  {"id":2,"title":"Team collaboration follow-up","column":"doing","created":"` + created.Add(time.Minute).Format(time.RFC3339Nano) + `"}]}`
	if err := os.WriteFile(filepath.Join(home, "queue.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	q1, err := loadWeaveQueue(home)
	if err != nil {
		t.Fatal(err)
	}
	a, b := q1.Stories[0], q1.Stories[1]
	if a.UUID == "" || b.UUID == "" || a.UUID == b.UUID {
		t.Fatalf("uuids must be minted and distinct: %q %q", a.UUID, b.UUID)
	}
	if ref.ShapeOf(a.UUID) != ref.ShapeUID {
		t.Fatalf("a minted uuid must have uid shape: %q", a.UUID)
	}
	if a.Slug != "team-collaboration-follow-up" || b.Slug != "team-collaboration-follow-up-2" {
		t.Fatalf("slugs must be unique within the store: %q %q", a.Slug, b.Slug)
	}
	// Loading again, nothing was written: SAME handles.
	q2, err := loadWeaveQueue(home)
	if err != nil {
		t.Fatal(err)
	}
	if q2.Stories[0].UUID != a.UUID || q2.Stories[1].Slug != b.Slug {
		t.Fatalf("minting must be deterministic across loads: %q vs %q", q2.Stories[0].UUID, a.UUID)
	}
	if raw, _ := os.ReadFile(filepath.Join(home, "queue.json")); strings.Contains(string(raw), "uuid") {
		t.Fatal("a read must not dirty the store")
	}
	// The next ordinary write persists exactly what was printed.
	if err := withWeaveQueueLock(home, func(q *weaveQueue) error { q.Stories[0].Column = "doing"; return nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "queue.json"))
	if !strings.Contains(string(raw), `"uuid": "`+a.UUID+`"`) || !strings.Contains(string(raw), `"slug": "`+b.Slug+`"`) {
		t.Fatalf("handles not persisted by the write:\n%s", raw)
	}
}

// Any handle opens the card: seq, `#seq`, uuid (full or a prefix), slug, and
// the ancestral path on this host; a path under another host says whose it is.
func TestFindSprintByHandle(t *testing.T) {
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/plinth")
	q := &weaveQueue{Stories: []*weaveStory{
		{ID: 7, Title: "Desktop over p2p", Created: time.Now().UTC()},
		{ID: 8, Title: "Zero-residue closure", Created: time.Now().UTC()},
	}}
	mintSprintHandles(q)
	want := q.Stories[0]
	for _, h := range []string{"7", "#7", want.UUID, want.UUID[:13], strings.ReplaceAll(want.UUID, "-", "")[:12], "desktop-over-p2p", sprintScopePath() + "/7", sprintScopePath() + "/" + want.Slug} {
		got, err := findSprintByHandle(q, h)
		if err != nil || got != want {
			t.Fatalf("handle %q: got %v err=%v", h, got, err)
		}
	}
	if _, err := findSprintByHandle(q, "9"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown seq: %v", err)
	}
	if _, err := findSprintByHandle(q, "bob/otherhost/7"); err == nil || !strings.Contains(err.Error(), "lives under bob/otherhost") || !strings.Contains(err.Error(), "uuid") {
		t.Fatalf("another host's path must be named, not 'not found': %v", err)
	}
}

// `sprint show` accepts every handle and prints all three plus the path;
// --json carries them under "ref".
func TestSprintShowPrintsTheThreeHandlesAndAcceptsAny(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", home)
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/plinth")
	q := &weaveQueue{NextStoryID: 2, Stories: []*weaveStory{{ID: 1, Title: "Fenced Rust through bashy dag", Column: "backlog", Created: time.Now().UTC()}}}
	if err := saveWeaveQueue(home, q); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		cmd := NewSprintCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	text := run("show", "1", "--plain")
	uuidLine := ""
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "ref:") {
			uuidLine = l
		}
	}
	if !strings.Contains(uuidLine, "sprint:1 · ") || !strings.Contains(uuidLine, " · fenced-rust-through-bashy-dag") {
		t.Fatalf("ref line: %q", uuidLine)
	}
	if !strings.Contains(text, "path:       sprint:"+sprintScopePath()+"/1") {
		t.Fatalf("path line missing:\n%s", text)
	}
	var env struct {
		Result struct {
			Ref sprintRef `json:"ref"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(run("show", "1", "--json")), &env); err != nil {
		t.Fatal(err)
	}
	r := env.Result.Ref
	if r.Ref != "sprint:1" || r.UUID == "" || r.Slug != "fenced-rust-through-bashy-dag" || r.Path != "sprint:"+sprintScopePath()+"/1" {
		t.Fatalf("json ref = %+v", r)
	}
	for _, h := range []string{r.Slug, r.UUID, r.UUID[:13], sprintScopePath() + "/1"} {
		if out := run("show", h, "--plain"); !strings.Contains(out, "sprint #1 [backlog]") {
			t.Fatalf("show %q:\n%s", h, out)
		}
	}
}
