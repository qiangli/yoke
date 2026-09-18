// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func TestWeaveDelegationSetsAssignee(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Setup repo with a docs/todo list
	dir := weaveTestRepo(t)

	// Create an issue in the repo store
	reg := todopkg.RepoStore(dir)
	it := &issue.Issue{
		ID:      "testabc123",
		Kind:    issue.KindTask,
		Title:   "test delegation",
		Status:  todopkg.StatusTodo,
		Created: time.Now(),
	}
	if _, err := reg.Save(it); err != nil {
		t.Fatal(err)
	}

	// Set WEAVE_AGENT so it picks it up as assignee
	t.Setenv("WEAVE_AGENT", "agent-smith")

	// Run weave add --from-todo
	t.Chdir(dir)

	cmd := newWeaveAddCmd()
	cmd.SetArgs([]string{"--from-issue", "testabc123"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	// Verify the register issue got assignee
	loaded, err := reg.Resolve("testabc123")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != todopkg.StatusAssigned {
		t.Fatalf("expected status assigned, got %s", loaded.Status)
	}
	if loaded.Assignee != "agent-smith" {
		t.Fatalf("expected assignee agent-smith, got %s", loaded.Assignee)
	}
	if loaded.Weave == 0 {
		t.Fatalf("expected Weave ID to be set")
	}
}
