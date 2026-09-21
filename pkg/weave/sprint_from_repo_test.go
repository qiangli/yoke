package weave

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
)

// A checkout with two stories of one sprint (filed as #228 on another host)
// and one legacy story with a bare seq. The board is empty.
func repoWithForeignSprint(t *testing.T) (home, repo string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/plinth")
	repo = t.TempDir()
	if real, err := filepath.EvalSymlinks(repo); err == nil {
		repo = real // macOS: /var → /private/var, and the store records the real path
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if raw, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
	}
	git("init", "-q")
	if err := os.MkdirAll(filepath.Join(repo, "docs", "todo"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "docs", "todo", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("aaaaaaaaaaa1-s1.md", "---\nid: aaaaaaaaaaa1\nseq: 611\ntitle: S1 frontmatter\nstatus: todo\npriority: p0\nsprint: 228\nsprint_id: 0a478ce2-e8fd-5c6f-8846-056bada92a7c\nsprint_title: Local-only sprint and todo\n---\n\nbody\n")
	write("aaaaaaaaaaa2-s2.md", "---\nid: aaaaaaaaaaa2\nseq: 612\ntitle: S2 materialize\nstatus: todo\npriority: p1\nsprint: 228\nsprint_id: 0a478ce2-e8fd-5c6f-8846-056bada92a7c\nsprint_title: Local-only sprint and todo\n---\n\nbody\n")
	write("bbbbbbbbbbb1-legacy.md", "---\nid: bbbbbbbbbbb1\nseq: 5\ntitle: Legacy story\nstatus: todo\nsprint: 3\n---\n\nbody\n")
	if err := os.WriteFile(filepath.Join(repo, "docs", "sprint-228-master-execution-plan.md"), []byte("# Sprint 228\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return home, repo
}

func runSprintErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewSprintCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestStoryBelongsToSprintByUUIDFirstThenSeq(t *testing.T) {
	card := &weaveStory{ID: 7, UUID: "0a478ce2-e8fd-5c6f-8846-056bada92a7c"}
	cases := []struct {
		name string
		it   issue.Issue
		want bool
	}{
		{"uuid matches, seq differs (another host's number)", issue.Issue{Sprint: 228, SprintID: "0a478ce2-e8fd-5c6f-8846-056bada92a7c"}, true},
		{"uuid matches case-insensitively", issue.Issue{Sprint: 1, SprintID: "0A478CE2-E8FD-5C6F-8846-056BADA92A7C"}, true},
		{"legacy story, seq matches", issue.Issue{Sprint: 7}, true},
		{"legacy story, seq differs", issue.Issue{Sprint: 8}, false},
		{"foreign uuid, same seq — NOT a member", issue.Issue{Sprint: 7, SprintID: "11111111-2222-5333-8444-555555555555"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := storyBelongsToSprint(&tc.it, card); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	if storyBelongsToSprint(&issue.Issue{Sprint: 7, SprintID: "0a478ce2-e8fd-5c6f-8846-056bada92a7c"}, &weaveStory{ID: 7}) {
		t.Fatal("a card without a uuid cannot claim a story that names one")
	}
}

func TestSprintShowCreatesTheCardFromTheCheckout(t *testing.T) {
	home, repo := repoWithForeignSprint(t)
	// Advance this host's counter so the foreign number 228 is free but the
	// next local number is not 228 — proves the frontmatter seq is reused.
	if err := os.MkdirAll(filepath.Join(home, "sprint"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sprint", "queue.json"), []byte(`{"next_story_id":4,"stories":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The board names what the checkout carries before any card exists.
	out, err := runSprintErr(t, "board")
	if err != nil {
		t.Fatal(err, out)
	}
	if !strings.Contains(out, "in this checkout, not on this host (1)") || !strings.Contains(out, "0a478ce2-e8fd-5c6f-8846-056bada92a7c") {
		t.Fatalf("board must list the checkout's sprint:\n%s", out)
	}

	out, err = runSprintErr(t, "show", "0a478ce2")
	if err != nil {
		t.Fatal(err, out)
	}
	if !strings.Contains(out, "sprint: created #228 on this host from the stories in "+repo) {
		t.Fatalf("want the one-line notice:\n%s", out)
	}
	if !strings.Contains(out, "sprint #228 [backlog] — Local-only sprint and todo") || !strings.Contains(out, "0a478ce2-e8fd-5c6f-8846-056bada92a7c") {
		t.Fatalf("card must carry the frontmatter's number, title and uuid:\n%s", out)
	}
	if !strings.Contains(out, "spec:       docs/sprint-228-master-execution-plan.md") {
		t.Fatalf("the plan doc named by the foreign number is the spec:\n%s", out)
	}
	if !strings.Contains(out, "S1 frontmatter") || !strings.Contains(out, "S2 materialize") || strings.Contains(out, "Legacy story") {
		t.Fatalf("exactly the stories that carry the uuid:\n%s", out)
	}

	// Idempotent: the same handle — by number this time — finds the card.
	out, err = runSprintErr(t, "show", "228")
	if err != nil {
		t.Fatal(err, out)
	}
	if strings.Contains(out, "created #") {
		t.Fatalf("second verb must find, not create:\n%s", out)
	}
	q, err := loadWeaveQueue(filepath.Join(home, "sprint"))
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Stories) != 1 || q.Stories[0].ID != 228 || q.NextStoryID != 229 || q.Stories[0].StoryRoots[0] != repo {
		t.Fatalf("one card, number 228, counter past it, checkout tracked: %+v next=%d", q.Stories[0], q.NextStoryID)
	}
	out, err = runSprintErr(t, "board")
	if err != nil || strings.Contains(out, "in this checkout, not on this host") {
		t.Fatalf("once created it is no longer 'not on this host': %v\n%s", err, out)
	}
}

func TestSprintFromRepoTakesTheNextNumberWhenTheForeignOneIsTaken(t *testing.T) {
	home, _ := repoWithForeignSprint(t)
	if err := os.MkdirAll(filepath.Join(home, "sprint"), 0o755); err != nil {
		t.Fatal(err)
	}
	board := `{"next_story_id":229,"stories":[{"id":228,"title":"An unrelated local sprint","column":"doing","created":"2026-09-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(home, "sprint", "queue.json"), []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}
	// By number: 228 IS a local card, so the local card wins — the label
	// collides by design and the uuid is how the other sprint is named.
	out, err := runSprintErr(t, "show", "228")
	if err != nil || !strings.Contains(out, "An unrelated local sprint") {
		t.Fatalf("local number must win: %v\n%s", err, out)
	}
	out, err = runSprintErr(t, "show", "0a478ce2-e8fd-5c6f-8846-056bada92a7c")
	if err != nil {
		t.Fatal(err, out)
	}
	if !strings.Contains(out, "sprint: created #229 on this host") || !strings.Contains(out, "sprint #229 [backlog] — Local-only sprint and todo") {
		t.Fatalf("want the next local number:\n%s", out)
	}
	// The stories still list under it — matched by uuid, not by their label 228.
	if !strings.Contains(out, "S1 frontmatter") {
		t.Fatalf("stories match by uuid:\n%s", out)
	}
}

func TestSprintFromRepoRefusesWhatNoStoryNames(t *testing.T) {
	repoWithForeignSprint(t)
	out, err := runSprintErr(t, "show", "11111111-2222-5333-8444-555555555555")
	if err == nil || !strings.Contains(out, "no story in") {
		t.Fatalf("want a refusal naming the checkout: %v\n%s", err, out)
	}
	out, err = runSprintErr(t, "show", "3")
	if err == nil || !strings.Contains(out, "no story in") {
		t.Fatalf("a legacy story without sprint_id cannot create a card: %v\n%s", err, out)
	}
}

func TestCommitMsgMatchesStoriesByUUIDAcrossNumbers(t *testing.T) {
	home, _ := repoWithForeignSprint(t)
	if err := os.MkdirAll(filepath.Join(home, "sprint"), 0o755); err != nil {
		t.Fatal(err)
	}
	// This host already uses 228 for something else; the team sprint lands as #229.
	board := `{"next_story_id":229,"stories":[{"id":228,"title":"An unrelated local sprint","column":"doing","created":"2026-09-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(home, "sprint", "queue.json"), []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runSprintErr(t, "show", "0a478ce2"); err != nil || !strings.Contains(out, "created #229") {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	write := func(body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The committer names ITS number; the stories say 228 in their label and
	// the uuid in sprint_id — accepted, because membership is by uuid.
	out, err := runSprintErr(t, "commit-msg", write("x\n\nSprint: #229\nStory: #611\nStory-ID: aaaaaaaaaaa1\n"))
	if err != nil || !strings.Contains(out, "Sprint #229, 1 story reference(s) verified") {
		t.Fatalf("uuid membership across numbers: %v\n%s", err, out)
	}
	// With the optional Sprint-ID, right and wrong.
	out, err = runSprintErr(t, "commit-msg", write("x\n\nSprint: #229\nSprint-ID: 0a478ce2-e8fd-5c6f-8846-056bada92a7c\nStory: #611\nStory-ID: aaaaaaaaaaa1\n"))
	if err != nil {
		t.Fatalf("matching Sprint-ID must pass: %v\n%s", err, out)
	}
	out, err = runSprintErr(t, "commit-msg", write("x\n\nSprint: #229\nSprint-ID: 11111111-2222-5333-8444-555555555555\nStory: #611\nStory-ID: aaaaaaaaaaa1\n"))
	if err == nil || !strings.Contains(out, "is not sprint #229 on this host") {
		t.Fatalf("wrong Sprint-ID must fail by name: %v\n%s", err, out)
	}
	// The unrelated local #228 does NOT own the story just because its label says 228.
	out, err = runSprintErr(t, "commit-msg", write("x\n\nSprint: #228\nStory: #611\nStory-ID: aaaaaaaaaaa1\n"))
	if err == nil || !strings.Contains(out, "not linked to Sprint: #228") {
		t.Fatalf("a colliding number must not claim a uuid-bearing story: %v\n%s", err, out)
	}
}

func TestCommitMsgSprintIDAgainstCommittedStoriesWhenNoCard(t *testing.T) {
	repoWithForeignSprint(t) // empty board
	write := func(body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	out, err := runSprintErr(t, "commit-msg", write("x\n\nSprint: #228\nSprint-ID: 0a478ce2-e8fd-5c6f-8846-056bada92a7c\nStory: #611\nStory-ID: aaaaaaaaaaa1\n"))
	if err != nil {
		t.Fatalf("no card, stories carry the uuid: %v\n%s", err, out)
	}
	out, err = runSprintErr(t, "commit-msg", write("x\n\nSprint: #228\nSprint-ID: 11111111-2222-5333-8444-555555555555\nStory: #611\nStory-ID: aaaaaaaaaaa1\n"))
	if err == nil || !strings.Contains(out, "does not match the committed stories") {
		t.Fatalf("wrong uuid against committed stories: %v\n%s", err, out)
	}
}

func TestParseCommitTraceOptionalSprintID(t *testing.T) {
	trace, err := parseCommitTrace("x\n\nSprint: #87\nSprint-ID: 0A478CE2-E8FD-5C6F-8846-056BADA92A7C\nStory: #110\nStory-ID: d1e86f29d7a7\n")
	if err != nil || trace.SprintID != "0a478ce2-e8fd-5c6f-8846-056bada92a7c" {
		t.Fatalf("trace=%+v err=%v", trace, err)
	}
	if _, err := parseCommitTrace("x\n\nSprint: #87\nSprint-ID: 0a478ce2\nStory: #110\nStory-ID: d1e86f29d7a7\n"); err == nil || !strings.Contains(err.Error(), "full uuid") {
		t.Fatalf("a prefix is not a Sprint-ID: %v", err)
	}
	if _, err := parseCommitTrace("x\n\nSprint: #87\nSprint-ID: 0a478ce2-e8fd-5c6f-8846-056bada92a7c\nSprint-ID: 0a478ce2-e8fd-5c6f-8846-056bada92a7c\nStory: #110\nStory-ID: d1e86f29d7a7\n"); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("two Sprint-IDs: %v", err)
	}
}

func TestSprintAddMintsTheUUIDAndTodoCarriesIt(t *testing.T) {
	home, _ := repoWithForeignSprint(t)
	out, err := runSprintErr(t, "add", "Fresh local sprint")
	if err != nil {
		t.Fatal(err, out)
	}
	q, err := loadWeaveQueue(filepath.Join(home, "sprint"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "sprint", "queue.json"))
	if q.Stories[0].UUID == "" || !strings.Contains(string(raw), q.Stories[0].UUID) {
		t.Fatalf("sprint add must persist the uuid at once:\n%s", raw)
	}
	if !strings.Contains(out, q.Stories[0].UUID) {
		t.Fatalf("sprint add prints the uuid:\n%s", out)
	}
	// The pkg/todo seam answers from this board.
	uuid, title, ok := sprintHandlesForTodo(q.Stories[0].ID)
	if !ok || uuid != q.Stories[0].UUID || title != "Fresh local sprint" {
		t.Fatalf("seam: %q %q %v", uuid, title, ok)
	}
	if _, _, ok := sprintHandlesForTodo(999); ok {
		t.Fatal("seam must answer ok=false for a card that does not exist")
	}
}
