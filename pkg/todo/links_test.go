// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

import (
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/kb"
)

// TestResolveLinksBothDirections checks that `todo show --links` resolves an
// item's outbound citations and its inbound backlinks through kb's one resolver
// (pkg/todo calls it read-only). A todo that cites a kb page, and a kb page that
// cites the todo back, must each see the other.
func TestResolveLinksBothDirections(t *testing.T) {
	root := t.TempDir()
	st := &issue.Store{Root: root}

	it, err := Add(st, "wire the guard", "Implements [[kb:pkill-guard]] for the mesh.", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// A kb page at the repo's docs/kb that links back to the todo.
	kbStore := kb.Open(filepath.Join(root, kb.RepoSub))
	if err := kbStore.Write(&kb.Page{
		Slug: "pkill-guard", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "pkill guard", Description: "WHEN killing a stuck process",
		Status: kb.StatusValidated, Body: "tracked by [[todo:" + it.ID + "]]",
	}, "add"); err != nil {
		t.Fatal(err)
	}

	out, in := resolveLinks(st, it)

	if len(out) != 1 || out[0].Ref != "kb:pkill-guard" || out[0].Status != "resolved" || out[0].Type != kb.TypeLesson {
		t.Fatalf("outbound: want one resolved kb:pkill-guard of type lesson, got %+v", out)
	}
	if len(in) != 1 || in[0].Ref != "kb:pkill-guard" {
		t.Fatalf("inbound: want kb:pkill-guard citing the todo, got %+v", in)
	}
}

// TestResolveLinksDanglingAndSprint marks an unresolved kb link dangling and a
// sprint link external (kb cannot see the sprint store from a leaf).
func TestResolveLinksDanglingAndSprint(t *testing.T) {
	root := t.TempDir()
	st := &issue.Store{Root: root}
	it, err := Add(st, "t", "see [[kb:nope]] and [[sprint:163]]", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := resolveLinks(st, it)
	byRef := map[string]string{}
	for _, l := range out {
		byRef[l.Ref] = l.Status
	}
	if byRef["kb:nope"] != "dangling" {
		t.Errorf("kb:nope should be dangling: %+v", out)
	}
	if byRef["sprint:163"] != "external" {
		t.Errorf("sprint:163 should be external: %+v", out)
	}
}
