// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// Abandoning a run must return its linked todo to the backlog, so the list never
// shows a stale "assigned" for work nobody is doing (the no-absence-of-evidence rule
// applied to task status).
func TestWeaveReleaseRegisterRevertsAssigned(t *testing.T) {
	root := t.TempDir()
	reg := todopkg.RepoStore(root)
	td := &issue.Issue{ID: "abc123def456", Kind: issue.KindTask, Title: "x", Status: todopkg.StatusAssigned, Weave: 42, Created: time.Now().UTC()}
	if _, err := reg.Save(td); err != nil {
		t.Fatal(err)
	}
	weaveReleaseRegister(root, &weaveItem{ID: 42, Register: "abc123def456"})
	got, err := reg.Resolve("abc123def456")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != todopkg.StatusTodo {
		t.Fatalf("assigned not reverted to todo: %s", got.Status)
	}
	if got.Weave != 0 {
		t.Fatalf("weave link not cleared: %d", got.Weave)
	}
}

// A DONE todo must not be resurrected by a stray release (only assigned reverts).
func TestWeaveReleaseRegisterLeavesDoneAlone(t *testing.T) {
	root := t.TempDir()
	reg := todopkg.RepoStore(root)
	td := &issue.Issue{ID: "aaaa11112222", Kind: issue.KindTask, Title: "x", Status: todopkg.StatusDone, Created: time.Now().UTC()}
	if _, err := reg.Save(td); err != nil {
		t.Fatal(err)
	}
	weaveReleaseRegister(root, &weaveItem{ID: 7, Register: "aaaa11112222"})
	got, _ := reg.Resolve("aaaa11112222")
	if got.Status != todopkg.StatusDone {
		t.Fatalf("done was disturbed by release: %s", got.Status)
	}
}
