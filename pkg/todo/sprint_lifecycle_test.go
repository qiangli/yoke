package todo

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
)

func TestSprintStoryCannotCloseThroughGenericTodo(t *testing.T) {
	st := &issue.Store{Root: t.TempDir()}
	it, err := Add(st, "claimed sprint work", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = 42
	it.Assignee = "worker"
	it.Status = StatusAssigned
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(st, it.ID, StatusDone); err == nil || !strings.Contains(err.Error(), "sprint accept 42") {
		t.Fatalf("generic close error = %v", err)
	}
	got, _ := ResolveRef(st, it.ID)
	if got.Status != StatusAssigned || got.Closed != nil {
		t.Fatalf("refusal mutated story: status=%q closed=%v", got.Status, got.Closed)
	}
}

func TestDoneSprintStoryCannotBeRetroactivelyAssigned(t *testing.T) {
	base := t.TempDir()
	st := RepoStore(base)
	now := time.Now().UTC()
	it := &issue.Issue{ID: issue.NewID(), Kind: issue.KindTask, Title: "legacy bad closure", Status: StatusDone, Sprint: 42, Closed: &now, Created: now}
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	cmd := NewTodoCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--base-dir", base, "edit", it.ID, "--owner", "worker"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot retroactively assign") {
		t.Fatalf("retroactive assignment error=%v output=%q", err, out.String())
	}
	got, _ := ResolveRef(st, it.ID)
	if got.Assignee != "" || got.Status != StatusDone || got.Closed == nil {
		t.Fatalf("refusal mutated legacy closure: %+v", got)
	}
}

// A story carries its sprint to any host: seq (the filer's label), uuid (the
// identity) and title — all three when a board answers, the seq alone when
// none is linked in, none on unlink.
func TestLinkSprintWritesAllThreeHandlesWhenTheBoardAnswers(t *testing.T) {
	prev := SprintHandles
	t.Cleanup(func() { SprintHandles = prev })

	SprintHandles = nil
	it := &issue.Issue{}
	LinkSprint(it, 228)
	if it.Sprint != 228 || it.SprintID != "" || it.SprintTitle != "" {
		t.Fatalf("no seam: seq only, got %+v", it)
	}

	SprintHandles = func(seq int64) (string, string, bool) {
		if seq == 228 {
			return "0a478ce2-e8fd-5c6f-8846-056bada92a7c", "Local-only sprint and todo", true
		}
		return "", "", false
	}
	LinkSprint(it, 228)
	if it.SprintID != "0a478ce2-e8fd-5c6f-8846-056bada92a7c" || it.SprintTitle != "Local-only sprint and todo" {
		t.Fatalf("seam answered: want uuid + title, got %+v", it)
	}
	LinkSprint(it, 999)
	if it.Sprint != 999 || it.SprintID != "" || it.SprintTitle != "" {
		t.Fatalf("unknown card: seq only, stale uuid cleared, got %+v", it)
	}
	LinkSprint(it, 228)
	LinkSprint(it, 0)
	if it.Sprint != 0 || it.SprintID != "" || it.SprintTitle != "" {
		t.Fatalf("unlink clears all three, got %+v", it)
	}
}

func TestSprintIDRoundTripsThroughFrontmatter(t *testing.T) {
	st := RepoStore(t.TempDir())
	it, err := Add(st, "carried story", "", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint, it.SprintID, it.SprintTitle = 228, "0a478ce2-e8fd-5c6f-8846-056bada92a7c", "Local-only sprint and todo"
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	back, err := ResolveRef(st, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Sprint != 228 || back.SprintID != it.SprintID || back.SprintTitle != it.SprintTitle {
		t.Fatalf("round trip: %+v", back)
	}
}
