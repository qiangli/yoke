// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/issue"
)

func ambiguousTodoFleet(t *testing.T) *fleet.Catalog {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `agents:
  - name: duplicate-owner
    tool: codex
    model: one
  - name: DUPLICATE-OWNER
    tool: claude
    model: two
  - name: alpha-owner
    aliases: [shared-owner]
    tool: codex
    model: one
  - name: beta-owner
    aliases: [shared-owner]
    tool: claude
    model: two
`
	if err := os.WriteFile(filepath.Join(dir, "ambiguous.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
}

func TestTodoAssigneeRejectsAmbiguousAgentIdentity(t *testing.T) {
	cat := ambiguousTodoFleet(t)
	previous := todoAgentCatalog
	todoAgentCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { todoAgentCatalog = previous })
	st := &issue.Store{Root: t.TempDir()}

	for _, owner := range []string{"duplicate-owner", "shared-owner"} {
		t.Run(owner, func(t *testing.T) {
			if _, err := Add(st, "ambiguous assignment", "", "", nil, "", owner); err == nil ||
				!strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("ambiguous assignee %q was not rejected: %v", owner, err)
			}
		})
	}
	items, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("rejected ambiguous assignments persisted %d todos", len(items))
	}
}

func pinTodoAgents(t *testing.T, names ...string) {
	t.Helper()
	cat := fleet.New(fleet.WithRoot(t.TempDir()))
	for _, name := range names {
		if err := cat.SaveAgent(fleet.Agent{Name: name, Tool: "codex", Model: "gpt5.6-sol"}); err != nil {
			t.Fatal(err)
		}
	}
	previous := todoAgentCatalog
	todoAgentCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { todoAgentCatalog = previous })
}

func TestAssignedStatusRequiresARegisteredAssignee(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	pinTodoAgents(t, "worker")
	st, err := UserStore("steward")
	if err != nil {
		t.Fatal(err)
	}
	it, err := Add(st, "unowned", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(st, it.ID, StatusAssigned); err == nil || !strings.Contains(err.Error(), "requires an owner (assignee)") {
		t.Fatalf("ownerless assigned status error = %v", err)
	}
	owned, err := Add(st, "delegated", "", "", nil, "", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if owned.Status != StatusAssigned || owned.Assignee != "worker" {
		t.Fatalf("delegated todo = status %q assignee %q", owned.Status, owned.Assignee)
	}
}

func TestTodoLifecycle(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	stew, err := UserStore("steward")
	if err != nil {
		t.Fatal(err)
	}
	a, err := Add(stew, "wire the webhook", "details", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusTodo {
		t.Fatalf("new task status = %q, want %q", a.Status, StatusTodo)
	}
	if _, err := Add(stew, "fix CI", "", "p0", nil, "", ""); err != nil {
		t.Fatal(err)
	}
	other, _ := UserStore("fix-42")
	if _, err := Add(other, "someone else's task", "", "", nil, "", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := List(stew, ""); len(got) != 2 {
		t.Fatalf("steward has %d tasks, want 2", len(got))
	}
	if got, _ := List(other, ""); len(got) != 1 {
		t.Fatalf("fix-42 has %d tasks, want 1", len(got))
	}
	if _, err := SetStatus(stew, a.ID[:6], StatusDoing); err != nil {
		t.Fatal(err)
	}
	doing, _ := List(stew, StatusDoing)
	if len(doing) != 1 || doing[0].ID != a.ID {
		t.Fatalf("doing list wrong: %+v", doing)
	}
	done, err := SetStatus(stew, a.ID, StatusDone)
	if err != nil {
		t.Fatal(err)
	}
	if done.Closed == nil {
		t.Fatal("done task must stamp Closed")
	}
	if _, err := Remove(stew, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := List(stew, ""); len(got) != 1 {
		t.Fatalf("after rm, steward has %d tasks, want 1", len(got))
	}
}

func TestTodoCLIAddAndEditSprint(t *testing.T) {
	base := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := NewTodoCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--base-dir", base}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("todo %v: %v\n%s", args, err, out.String())
		}
	}

	run("add", "tracked delivery", "--sprint", "97")
	items, err := List(RepoStore(base), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d, err = %v", len(items), err)
	}
	if items[0].Sprint != 97 {
		t.Fatalf("added sprint = %d, want 97", items[0].Sprint)
	}

	run("edit", "1", "--sprint", "98")
	item, err := ResolveRef(RepoStore(base), "1")
	if err != nil || item.Sprint != 98 {
		t.Fatalf("edited sprint = %d, err = %v", item.Sprint, err)
	}
	run("edit", "1", "--sprint", "0")
	item, err = ResolveRef(RepoStore(base), "1")
	if err != nil || item.Sprint != 0 {
		t.Fatalf("unlinked sprint = %d, err = %v", item.Sprint, err)
	}
}

func TestRepoStoreIsDocsTodo(t *testing.T) {
	rs := RepoStore("/some/repo")
	if rs.Sub != RepoSub || RepoSub != "docs/todo" {
		t.Fatalf("repo sub = %q, want docs/todo", rs.Sub)
	}
	if got := filepath.Join(rs.Root, rs.Sub); got != filepath.Join("/some/repo", "docs/todo") {
		t.Fatalf("repo dir = %q", got)
	}
}

func TestScopeResolution(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())

	// --base-dir targets another project root's docs/todo, without cd.
	base := t.TempDir()
	bst, label, err := ResolveStore("steward", false, false, base)
	if err != nil {
		t.Fatal(err)
	}
	if bst.Root != base || bst.Sub != RepoSub {
		t.Fatalf("base-dir store = %s/%s, want %s/%s", bst.Root, bst.Sub, base, RepoSub)
	}
	if label != "repo "+base {
		t.Fatalf("label %q", label)
	}

	// --user forces the personal list.
	ust, ulabel, err := ResolveStore("steward", false, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if ust.Sub != "steward" {
		t.Fatalf("user sub = %q, want steward", ust.Sub)
	}
	if !strings.HasPrefix(ulabel, "user ") {
		t.Fatalf("user label %q", ulabel)
	}

	// Items in the base-dir store don't leak into the personal list.
	if _, err := Add(bst, "checked-in task", "", "", nil, "", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := List(bst, ""); len(got) != 1 {
		t.Fatalf("base-dir store has %d, want 1", len(got))
	}
	if got, _ := List(ust, ""); len(got) != 0 {
		t.Fatalf("personal store leaked repo items: %d", len(got))
	}
}

func TestBadStatusRejected(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	st, _ := UserStore("steward")
	a, _ := Add(st, "x", "", "", nil, "", "")
	if _, err := SetStatus(st, a.ID, "nope"); err == nil {
		t.Fatal("an unknown status must be rejected")
	}
}

func TestOwnerTraversalIsContained(t *testing.T) {
	if got := SanitizeOwner("../../etc"); got == "../../etc" {
		t.Fatalf("owner traversal not sanitized: %q", got)
	}
}

func TestIsOverdue(t *testing.T) {
	// due yesterday -> overdue
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	it := &issue.Issue{Status: StatusTodo, Due: &yesterday}
	if !IsOverdue(it) {
		t.Fatal("due yesterday should be overdue")
	}

	// due tomorrow -> not overdue
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	it2 := &issue.Issue{Status: StatusTodo, Due: &tomorrow}
	if IsOverdue(it2) {
		t.Fatal("due tomorrow should not be overdue")
	}

	// done + due yesterday -> NOT overdue
	it3 := &issue.Issue{Status: StatusDone, Due: &yesterday}
	if IsOverdue(it3) {
		t.Fatal("done item should never be overdue")
	}

	// nil due -> not overdue
	it4 := &issue.Issue{Status: StatusTodo}
	if IsOverdue(it4) {
		t.Fatal("nil due should not be overdue")
	}
}

func TestListShowsOverdueMarker(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	st, _ := UserStore("steward")

	// Add an overdue item
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	_, err := Add(st, "overdue task", "", "", &yesterday, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Add a non-overdue item
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	_, err = Add(st, "future task", "", "", &tomorrow, "", "")
	if err != nil {
		t.Fatal(err)
	}

	items, err := List(st, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	// Find the overdue item
	var overdueItem *issue.Issue
	var futureItem *issue.Issue
	for _, it := range items {
		if it.Title == "overdue task" {
			overdueItem = it
		}
		if it.Title == "future task" {
			futureItem = it
		}
	}
	if overdueItem == nil || futureItem == nil {
		t.Fatal("items not found")
	}

	if !IsOverdue(overdueItem) {
		t.Fatal("overdue task should be overdue")
	}
	if IsOverdue(futureItem) {
		t.Fatal("future task should not be overdue")
	}

	// Verify the marker actually appears in the RENDERED list output — invoke the
	// real list command, not a re-implementation of its logic. Exactly the one
	// overdue item must be marked (the future item must not).
	var buf bytes.Buffer
	lc := newListCmd(func() (*issue.Store, string, error) { return st, "test", nil })
	lc.SetOut(&buf)
	lc.SetArgs(nil)
	if err := lc.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "(OVERDUE)"); n != 1 {
		t.Fatalf("list output should mark exactly the one overdue item, got %d markers:\n%s", n, out)
	}
}
