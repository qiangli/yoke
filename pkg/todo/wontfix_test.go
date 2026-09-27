package todo

// Sprint: #301; Story: #959; Story-ID: cb38b7c214d0

import (
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
)

func TestIsClosed(t *testing.T) {
	for status, want := range map[string]bool{
		StatusDone: true, StatusWontfix: true, issue.StatusClosed: true,
		StatusTodo: false, StatusAssigned: false, StatusDoing: false, StatusBlocked: false,
	} {
		if got := IsClosed(status); got != want {
			t.Errorf("IsClosed(%q) = %v, want %v", status, got, want)
		}
	}
}

// wontfix closes an item without a delivery: it stamps Closed like done, a
// sprint story may take it (no acceptance credit is claimed), a recurring
// item does not reopen on it, and it leaves the open list.
func TestWontfixClosesWithoutDelivery(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	st, err := UserStore("steward")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Add(st, "not needed after all", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story, err := Add(st, "a sprint story dropped by decision", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint = 301
	if _, err := st.Save(story); err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(st, story.ID, StatusDone); err == nil {
		t.Fatal("done on a sprint story must still go through sprint accept")
	}
	recurring, err := Add(st, "weekly thing retired", "", "", nil, "weekly", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range []*issue.Issue{plain, story, recurring} {
		got, err := SetStatus(st, it.ID, StatusWontfix)
		if err != nil {
			t.Fatalf("%s: %v", it.Title, err)
		}
		if got.Status != StatusWontfix || got.Closed == nil {
			t.Fatalf("%s: status %q closed %v, want wontfix + a close stamp", it.Title, got.Status, got.Closed)
		}
	}
	if IsOverdue(plain) {
		t.Fatal("a wontfix item is never overdue")
	}
	reopened, err := SetStatus(st, plain.ID, StatusTodo)
	if err != nil || reopened.Closed != nil {
		t.Fatalf("reopen: %v, closed %v", err, reopened.Closed)
	}
}
