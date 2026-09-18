// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// sprint show --links: a sprint card's prose (spec, acceptance, continuity,
// goal text, thread) may cite kb pages and todos by id — [[kb:slug]],
// [[todo:id]] — and those citations become real at READ time through the one
// resolver kb owns (pkg/kb/links.go; todo show --links calls the same). The
// graph is the kb pages and todo records of every story root the sprint
// tracks (sprint track), plus the current checkout. sprint reads kb and todo;
// neither can see the sprint store (kb is an import leaf), so this is the
// only place a [[sprint:n]] citation's OWN links are resolved, and inbound
// links to a sprint are the ones todo/kb classify as "external".

type sprintLinkRef struct {
	Ref    string `json:"ref"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"` // schema | resolved | dangling | external | unknown
	Field  string `json:"field,omitempty"`  // spec | acceptance | continuity | goal | thread
}

// sprintLinkFields is the prose of a card, by field, in display order.
func sprintLinkFields(s *weaveStory) [][2]string {
	fields := [][2]string{
		{"spec", s.SpecRef},
		{"acceptance", s.Acceptance},
		{"continuity", s.Continuity},
	}
	var goal []string
	for _, g := range s.Goal {
		goal = append(goal, g.Text)
	}
	fields = append(fields, [2]string{"goal", strings.Join(goal, "\n")})
	var thread []string
	for _, c := range s.Thread {
		thread = append(thread, c.Body)
	}
	fields = append(fields, [2]string{"thread", strings.Join(thread, "\n")})
	return fields
}

// resolveSprintLinks returns everything the card is connected to, in one
// list: the SCHEMA links first (goal → story, linked weave runs; status
// "schema", field "goal" / "run"), then every citation in the card's prose
// resolved against the kb pages + todo records of the sprint's story roots.
// The two are different facts — membership versus mention — and the field
// says which is which.
func resolveSprintLinks(s *weaveStory) []sprintLinkRef {
	var nodes []kb.LinkNode
	for _, root := range sprintStoryRoots(s) {
		if pages, err := kb.Open(filepath.Join(root, kb.RepoSub)).List(); err == nil {
			nodes = append(nodes, kb.KBNodes(pages)...)
		}
		if items, err := todopkg.List(todopkg.RepoStore(root), ""); err == nil {
			for _, it := range items {
				nodes = append(nodes, kb.TodoNode(it.ID, it.Title, it.Body))
			}
		}
	}
	var out []sprintLinkRef
	seen := map[string]bool{}
	// Schema: goal items name stories by id; the story's title comes from the
	// same node graph (a story that no tracked root holds is "dangling", which
	// is exactly what `sprint show`'s coverage line calls it).
	for _, g := range s.Goal {
		for _, ref := range g.Stories {
			key := "goal|todo:" + ref.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			l := kb.Link{Kind: kb.LinkTodo, Target: ref.ID, Raw: "[[todo:" + ref.ID + "]]"}
			if n, ok := kb.ResolveLink(l, nodes); ok {
				out = append(out, sprintLinkRef{Ref: n.Ref(), Title: n.Title, Status: "schema", Field: "goal:" + g.ID})
			} else {
				out = append(out, sprintLinkRef{Ref: l.Ref(), Status: "dangling", Field: "goal:" + g.ID})
			}
		}
	}
	for _, r := range s.Runs {
		key := fmt.Sprintf("run|%s:%d", r.Repo, r.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sprintLinkRef{Ref: fmt.Sprintf("run:%s-%d", filepath.Base(r.Repo), r.ID), Title: r.Repo, Status: "schema", Field: "run"})
	}
	for _, f := range sprintLinkFields(s) {
		for _, l := range kb.ParseLinks(f[1]) {
			key := f[0] + "|" + l.Ref()
			if seen[key] {
				continue
			}
			seen[key] = true
			if n, ok := kb.ResolveLink(l, nodes); ok {
				out = append(out, sprintLinkRef{Ref: n.Ref(), Title: n.Title, Status: "resolved", Field: f[0]})
			} else {
				// external (sprint, run, meet, … — another store's kind),
				// unknown (a scheme outside the vocabulary), or dangling.
				out = append(out, sprintLinkRef{Ref: l.Ref(), Status: l.Status(), Field: f[0]})
			}
		}
	}
	return out
}

func renderSprintLinks(w io.Writer, links []sprintLinkRef) {
	fmt.Fprintf(w, "  ── links (%d; schema = membership, resolved = a citation in the prose) ──\n", len(links))
	if len(links) == 0 {
		fmt.Fprintln(w, "  (none — link a story with `sprint goal add --story`, or cite [[kb:slug]] / [[todo:id]] in the spec, acceptance, continuity, goal or thread)")
		return
	}
	for _, l := range links {
		title := l.Title
		if title == "" {
			title = "-"
		}
		// Truncate for the column, never the JSON: a story title is a sentence.
		if r := []rune(title); len(r) > 44 {
			title = string(r[:43]) + "…"
		}
		fmt.Fprintf(w, "  -> %-22s %-44s %-9s (%s)\n", l.Ref, title, l.Status, l.Field)
	}
}
