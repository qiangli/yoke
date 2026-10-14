package board

import (
	"testing"
	"time"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// A wontfix story is closed work, exactly like done: the sprint card must
// not count it as open. The rule lives in todo.IsClosed (what `bashy sprint
// show` uses); the board shares it instead of restating it.
func TestFinalizeWontfixCountsAsClosed(t *testing.T) {
	statuses := []struct {
		status string
		open   bool
	}{
		{"done", false},
		{"wontfix", false},
		{"todo", true},
		{"blocked", true},
		{"assigned", true},
	}
	b := &Board{Sprints: []Sprint{{ID: 7, Title: "guard", Column: "doing"}}}
	for i, st := range statuses {
		// The board's open/closed buckets must agree with the shared
		// predicate for every status in this story set.
		if todopkg.IsClosed(st.status) == st.open {
			t.Fatalf("shared predicate disagrees with test expectation for %q", st.status)
		}
		b.Todos = append(b.Todos, Todo{ID: string(rune('a' + i)), Title: st.status, Status: st.status, SprintID: 7})
	}
	// A story on another sprint must not leak into this card's counts.
	b.Sprints = append(b.Sprints, Sprint{ID: 8, Title: "other", Column: "doing"})
	b.Todos = append(b.Todos, Todo{ID: "z", Title: "elsewhere", Status: "todo", SprintID: 8})

	b.finalize(time.Now())

	sp := b.Sprints[0]
	if sp.StoryOpen != 3 || sp.StoryClosed != 2 || sp.StoryTotal != 5 {
		t.Fatalf("wrong sprint counts: open=%d closed=%d total=%d, want 3/2/5",
			sp.StoryOpen, sp.StoryClosed, sp.StoryTotal)
	}
}
