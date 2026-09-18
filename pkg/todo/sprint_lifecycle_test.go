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
