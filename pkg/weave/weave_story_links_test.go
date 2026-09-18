// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/kb"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// TestResolveSprintLinks: a card's prose cites kb pages and todos by id and
// they resolve at read time against the tracked story root; a [[sprint:n]]
// citation is external (the sprint store is not a link node); an unknown
// slug is dangling, never an error.
func TestResolveSprintLinks(t *testing.T) {
	root := t.TempDir()
	st := kb.Open(filepath.Join(root, kb.RepoSub))
	if err := st.Write(&kb.Page{Form: kb.FormNote, Type: kb.TypeLesson, Title: "Deploy runbook", Slug: "deploy-runbook", Body: "x", Status: kb.StatusCandidate}, "add"); err != nil {
		t.Fatal(err)
	}
	it, err := todopkg.Add(todopkg.RepoStore(root), "Fix the gate", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{
		ID:         1,
		SpecRef:    "[[kb:deploy-runbook]]",
		Acceptance: "done when [[todo:" + it.ID[:8] + "]] closes; see [[sprint:2]] and [[kb:nope]]",
		StoryRoots: []string{root},
		Goal:       []sprintGoalItem{{ID: "g1", Text: "ship it", Stories: []sprintStoryRef{{Repo: root, ID: it.ID}, {Repo: root, ID: "000000000000"}}}},
		Runs:       []sprintRun{{Repo: root, ID: 7}},
	}
	got := map[string]string{}
	for _, l := range resolveSprintLinks(s) {
		got[l.Field+" "+l.Ref] = l.Status
	}
	want := map[string]string{
		"spec kb:deploy-runbook":                "resolved",
		"acceptance todo:" + it.ID:              "resolved",
		"acceptance sprint:2":                   "external",
		"acceptance kb:nope":                    "dangling",
		"goal:g1 todo:" + it.ID:                 "schema",   // membership, not a citation
		"goal:g1 todo:000000000000":             "dangling", // a story no tracked root holds
		"run run:" + filepath.Base(root) + "-7": "schema",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %q, want %q (all: %v)", k, got[k], w, got)
		}
	}
}
