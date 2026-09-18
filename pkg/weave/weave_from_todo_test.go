// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func TestWeaveAddFromTodoRepoAndHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_TODO_DIR", filepath.Join(home, ".bashy", "todo"))

	dir := weaveTestRepo(t)
	t.Chdir(dir)

	// 1. Create a repo todo item in docs/todo
	repoStore := todopkg.RepoStore(dir)
	repoItem, err := todopkg.Add(repoStore, "Repo task item", "Repo body", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// 2. Create a host user todo item in ~/.bashy/todo/steward
	userStore, err := todopkg.UserStore("")
	if err != nil {
		t.Fatal(err)
	}
	userItem, err := todopkg.Add(userStore, "Host task item", "Host body", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// 3. Seed run from repo todo
	cmd1 := newWeaveAddCmd()
	cmd1.SetArgs([]string{"--from-todo", repoItem.ID[:8]})
	outBuf1 := &bytes.Buffer{}
	cmd1.SetOut(outBuf1)
	if err := cmd1.Execute(); err != nil {
		t.Fatalf("weave add --from-todo repo item failed: %v", err)
	}

	// Verify repo item is assigned and linked to weave #1
	repoLoaded, err := repoStore.Resolve(repoItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repoLoaded.Status != todopkg.StatusAssigned {
		t.Fatalf("expected repo todo status assigned, got %s", repoLoaded.Status)
	}
	if repoLoaded.Weave != 1 {
		t.Fatalf("expected repo todo Weave=1, got %d", repoLoaded.Weave)
	}

	// Verify weave status 1 shows register link
	statusCmd1 := newWeaveStatusCmd()
	statusCmd1.SetArgs([]string{"1"})
	statusBuf1 := &bytes.Buffer{}
	statusCmd1.SetOut(statusBuf1)
	if err := statusCmd1.Execute(); err != nil {
		t.Fatalf("weave status 1 failed: %v", err)
	}
	if !strings.Contains(statusBuf1.String(), repoItem.ID[:8]) {
		t.Fatalf("weave status output missing register link: %s", statusBuf1.String())
	}

	// 4. Seed run from host user todo
	cmd2 := newWeaveAddCmd()
	cmd2.SetArgs([]string{"--from-todo", userItem.ID[:8]})
	outBuf2 := &bytes.Buffer{}
	cmd2.SetOut(outBuf2)
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("weave add --from-todo user item failed: %v", err)
	}

	// Verify user item is assigned and linked to weave #2
	userLoaded, err := userStore.Resolve(userItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if userLoaded.Status != todopkg.StatusAssigned {
		t.Fatalf("expected user todo status assigned, got %s", userLoaded.Status)
	}
	if userLoaded.Weave != 2 {
		t.Fatalf("expected user todo Weave=2, got %d", userLoaded.Weave)
	}

	// Verify todo show for repo todo shows weave #1 link
	todoShowCmd1 := todopkg.NewTodoCmd()
	todoShowBuf1 := &bytes.Buffer{}
	todoShowCmd1.SetOut(todoShowBuf1)
	todoShowCmd1.SetArgs([]string{"--repo", "show", repoItem.ID[:8]})
	if err := todoShowCmd1.Execute(); err != nil {
		t.Fatalf("todo show failed: %v", err)
	}
	if !strings.Contains(todoShowBuf1.String(), "weave     #1") {
		t.Fatalf("todo show output missing weave #1 link: %s", todoShowBuf1.String())
	}

	// 5. Test merge reconciliation: weaveCloseRegisterOnMerge
	root, workspace, sha := setupMergeFixture(t)
	repoStoreMerge := todopkg.RepoStore(root)
	mItem, err := todopkg.Add(repoStoreMerge, "Merge task", "Merge body", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	mItem.Status = todopkg.StatusAssigned
	mItem.Weave = 10
	if _, err := repoStoreMerge.Save(mItem); err != nil {
		t.Fatal(err)
	}

	gitT(t, root, "fetch", "-q", workspace, "agent/weave-issue-1:agent/weave-issue-1")
	gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge issue 1", "agent/weave-issue-1")

	runItem := &weaveItem{ID: 10, Register: mItem.ID, Owner: "agent", State: "done", Head: sha, Workspace: workspace, CommitsAhead: 1}
	weaveCloseRegisterOnMerge(root, "main", runItem)

	mLoaded, err := repoStoreMerge.Resolve(mItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mLoaded.Status != todopkg.StatusDone {
		t.Fatalf("expected merged todo status to be done, got %s", mLoaded.Status)
	}
	if mLoaded.Closed == nil {
		t.Fatal("expected Closed timestamp to be set")
	}
	if mLoaded.Weave != 10 {
		t.Fatalf("expected Weave=10 to be retained on merged todo, got %d", mLoaded.Weave)
	}

	// 6. Test abandon reconciliation: weaveReleaseRegister
	abItem, err := todopkg.Add(repoStoreMerge, "Abandon task", "Abandon body", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	abItem.Status = todopkg.StatusAssigned
	abItem.Weave = 11
	if _, err := repoStoreMerge.Save(abItem); err != nil {
		t.Fatal(err)
	}

	abRun := &weaveItem{ID: 11, Register: abItem.ID}
	weaveReleaseRegister(root, abRun)

	abLoaded, err := repoStoreMerge.Resolve(abItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if abLoaded.Status != todopkg.StatusTodo {
		t.Fatalf("expected abandoned todo status to revert to todo, got %s", abLoaded.Status)
	}
	if abLoaded.Weave != 0 {
		t.Fatalf("expected abandoned todo Weave link to be cleared (0), got %d", abLoaded.Weave)
	}
}
