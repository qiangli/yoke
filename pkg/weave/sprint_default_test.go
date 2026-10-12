package weave

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func defaultFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", home)
	board := filepath.Join(home, "sprint")
	t.Setenv("BASHY_SPRINT_DIR", board)
	t.Setenv("BASHY_SPRINT_ANNOUNCE", "0")
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/test")
	return board, mustStoryRoot(t)
}

// seedSprints writes cards whose creation order differs from numeric order:
// #7 is the newest, #30 the highest number.
func seedSprints(t *testing.T, board string) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := &weaveQueue{}
	for _, c := range []struct {
		id    int64
		title string
		age   time.Duration
	}{{30, "old high", 0}, {7, "newest low", 2 * time.Hour}, {12, "middle", time.Hour}} {
		q.Stories = append(q.Stories, &weaveStory{ID: c.id, Title: c.title, Column: "backlog",
			Created: base.Add(c.age), UpdatedAt: base.Add(c.age)})
	}
	mintSprintHandles(q)
	if err := saveWeaveQueue(board, q); err != nil {
		t.Fatal(err)
	}
}

func defaultTodo(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	cmd := todopkg.NewTodoCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--base-dir", root}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func storyByTitle(t *testing.T, root, title string) *issue.Issue {
	t.Helper()
	items, err := todopkg.List(todopkg.RepoStore(root), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Title == title {
			return it
		}
	}
	t.Fatalf("story %q not filed", title)
	return nil
}

func TestTodoAddDefaultsToLatestCreatedSprint(t *testing.T) {
	board, root := defaultFixture(t)
	seedSprints(t, board)
	for _, tc := range []struct {
		name  string
		flags []string
		want  int64
	}{
		{"default", nil, 7},
		{"last", []string{"--sprint", "last"}, 7},
		{"explicit", []string{"--sprint", "30"}, 30},
		{"opt-out", []string{"--no-sprint"}, 0},
		{"ordinary-9999", []string{"--sprint", "9999"}, 9999},
	} {
		if out, err := defaultTodo(t, root, append([]string{"add", tc.name}, tc.flags...)...); err != nil {
			t.Fatalf("%s: %v: %s", tc.name, err, out)
		}
		it := storyByTitle(t, root, tc.name)
		if it.Sprint != tc.want {
			t.Fatalf("%s: sprint %d, want %d", tc.name, it.Sprint, tc.want)
		}
		if tc.want > 0 && tc.want != 9999 && (it.SprintID == "" || it.SprintTitle == "") {
			t.Fatalf("%s: missing uuid/title link: %+v", tc.name, it)
		}
		if tc.want == 9999 && it.SprintID != "" {
			t.Fatalf("9999 with no card must link by number only: %+v", it)
		}
	}
	if out, err := defaultTodo(t, root, "add", "bad", "--no-sprint", "--sprint", "7"); err == nil {
		t.Fatalf("conflicting flags accepted: %s", out)
	}
	q, err := loadWeaveQueue(board)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Stories) != 3 {
		t.Fatalf("todo add changed the board: %d cards", len(q.Stories))
	}
	for _, n := range []int64{7, 30} {
		card := findWeaveStory(q, n)
		if len(card.StoryRoots) != 1 || card.StoryRoots[0] != root {
			t.Fatalf("selected sprint %d does not index its stories: %+v", n, card)
		}
	}
}

func TestTodoAddPersonalStaysStandalone(t *testing.T) {
	board, _ := defaultFixture(t)
	seedSprints(t, board)
	cmd := todopkg.NewTodoCmd()
	cmd.SetArgs([]string{"--user", "add", "personal"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	personal, err := todopkg.UserStore(todopkg.DefaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	items, err := todopkg.List(personal, "")
	if err != nil || len(items) != 1 || items[0].Sprint != 0 {
		t.Fatalf("personal default: %v %+v", err, items)
	}
}

func TestEmptyBoardFailsAndCreatesNothing(t *testing.T) {
	board, root := defaultFixture(t)
	out, err := defaultTodo(t, root, "add", "orphan")
	if err == nil || !strings.Contains(err.Error(), "no sprint exists") {
		t.Fatalf("empty board default: %v %s", err, out)
	}
	if items, _ := todopkg.List(todopkg.RepoStore(root), ""); len(items) != 0 {
		t.Fatal("failed add left a story behind")
	}
	if out, err := defaultTodo(t, root, "add", "standalone", "--no-sprint"); err != nil {
		t.Fatalf("--no-sprint on empty board: %v %s", err, out)
	}
	if out, err := runSprintErr(t, "show"); err == nil || !strings.Contains(err.Error()+out, "no sprint exists") {
		t.Fatalf("omitted operand on empty board: %v %s", err, out)
	}
	if q, err := loadWeaveQueue(board); err != nil || len(q.Stories) != 0 {
		t.Fatalf("a card was created implicitly: %v %+v", err, q)
	}
}

func TestNextSprintIDAllocation(t *testing.T) {
	q := &weaveQueue{NextStoryID: 9998}
	for _, want := range []int64{9998, 9999, 10000} {
		if got := nextSprintID(q); got != want {
			t.Fatalf("got %d want %d", got, want)
		}
	}
	q = &weaveQueue{Stories: []*weaveStory{{ID: 1}, {ID: 2}}}
	if got := nextSprintID(q); got != 3 {
		t.Fatalf("occupied numbers skipped wrong: %d", got)
	}
}

func TestOmittedOperandsReachHostAdaptersAsLatest(t *testing.T) {
	board, _ := defaultFixture(t)
	seedSprints(t, board)
	var received []string
	cmd := NewSprintCmd(func(root *cobra.Command) {
		var replace func(*cobra.Command)
		replace = func(c *cobra.Command) {
			for _, child := range c.Commands() {
				replace(child)
			}
			if c.RunE != nil {
				c.RunE = func(_ *cobra.Command, args []string) error { received = append([]string(nil), args...); return nil }
			}
		}
		replace(root)
	})
	for _, tc := range []struct {
		path []string
		args []string
		want string
	}{
		{[]string{"show"}, nil, "7"},
		{[]string{"show"}, []string{"last"}, "7"},
		{[]string{"claim"}, []string{"last", "story-id"}, "7,story-id"},
		{[]string{"show"}, []string{"30"}, "30"},
		{[]string{"claim"}, []string{"story-id"}, "7,story-id"},
		{[]string{"claim"}, []string{"30", "story-id"}, "30,story-id"},
		{[]string{"move"}, []string{"doing"}, "7,doing"},
		{[]string{"goal", "link"}, []string{"goal-id"}, "7,goal-id"},
		{[]string{"arena", "status"}, nil, "7"},
		{[]string{"grade"}, nil, "7"},
		{[]string{"start"}, nil, "7"},
		{[]string{"stop"}, nil, "7"},
		{[]string{"end"}, nil, "7"},
		{[]string{"advance"}, nil, "7"},
		{[]string{"comment"}, nil, "7"},
		{[]string{"add"}, []string{"new sprint title"}, "new sprint title"},
		{[]string{"board"}, nil, ""},
	} {
		c, _, err := cmd.Find(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.ValidateArgs(tc.args); err != nil {
			t.Fatalf("%v args: %v", tc.path, err)
		}
		if err := c.RunE(c, tc.args); err != nil {
			t.Fatalf("%v run: %v", tc.path, err)
		}
		if got := strings.Join(received, ","); got != tc.want {
			t.Fatalf("%v: got %q want %q", tc.path, got, tc.want)
		}
	}
	if out, err := runSprintErr(t, "show"); err != nil || !strings.Contains(out, "sprint #7 ") {
		t.Fatalf("real show of latest: %v %s", err, out)
	}
}

// A historical maintenance uuid is just a uuid: it hydrates under its filed
// number when free, otherwise under the next free number, keeping the uuid.
func TestHistoricalUUIDHydratesLikeAnyOther(t *testing.T) {
	board, root := defaultFixture(t)
	historical := uuid.NewSHA1(sprintUUIDNamespace, []byte("bashy/standing-maintenance/v1")).String()
	st := todopkg.RepoStore(root)
	it, err := todopkg.Add(st, "historical fix", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint, it.SprintID, it.SprintTitle = 9999, historical, "Maintenance"
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	s, created, err := ensureSprintFromRepo(board, historical)
	if err != nil || !created || s.ID != 9999 || s.UUID != historical || s.Title != "Maintenance" || s.Epic != "" || s.SpecRef != "" {
		t.Fatalf("hydration: %v %v %+v", err, created, s)
	}
	got := storyByTitle(t, root, "historical fix")
	if got.Sprint != 9999 || got.SprintID != historical {
		t.Fatalf("story history rewritten: %+v", got)
	}
	// Number 9999 occupied by another card: the historical uuid gets a free number.
	other := "11111111-2222-5333-8444-555555555555"
	it.SprintID, it.SprintTitle = other, "Other"
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	s, _, err = ensureSprintFromRepo(board, other)
	if err != nil || s.ID != 10000 || s.UUID != other {
		t.Fatalf("collision: %v %+v", err, s)
	}
}

func TestSprintSelectionFlag(t *testing.T) {
	board, _ := defaultFixture(t)
	seedSprints(t, board)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"show", "--sprint", "last"}, "sprint #7 "},
		{[]string{"show", "--sprint", "30"}, "sprint #30 "},
	} {
		out, err := runSprintErr(t, tc.args...)
		if err != nil || !strings.Contains(out, tc.want) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
	}
	if out, err := runSprintErr(t, "show", "30", "--sprint", "last"); err == nil {
		t.Fatalf("ambiguous selection accepted: %s", out)
	}
}

func TestTodoEditLastAndUnlink(t *testing.T) {
	board, root := defaultFixture(t)
	seedSprints(t, board)
	if out, err := defaultTodo(t, root, "add", "move me", "--no-sprint"); err != nil {
		t.Fatalf("add: %v %s", err, out)
	}
	it := storyByTitle(t, root, "move me")
	if out, err := defaultTodo(t, root, "edit", it.ID, "--sprint", "last"); err != nil {
		t.Fatalf("edit last: %v %s", err, out)
	}
	it = storyByTitle(t, root, "move me")
	if it.Sprint != 7 || it.SprintID == "" {
		t.Fatalf("last not linked: %+v", it)
	}
	if out, err := defaultTodo(t, root, "edit", it.ID, "--sprint", "0"); err != nil {
		t.Fatalf("unlink: %v %s", err, out)
	}
	it = storyByTitle(t, root, "move me")
	if it.Sprint != 0 || it.SprintID != "" || it.SprintTitle != "" {
		t.Fatalf("unlink left sprint fields: %+v", it)
	}
}
