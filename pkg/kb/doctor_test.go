// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package kb

import (
	"os"
	"testing"
)

// TestNoteFormNeedsNoDescription: a note is the memo shape — `kb add --form
// note --title --body` is one command, and doctor does not call the missing
// description a hygiene problem. A page still needs its routing surface.
func TestNoteFormNeedsNoDescription(t *testing.T) {
	dir := t.TempDir()
	st := Open(dir)
	note := &Page{Form: FormNote, Type: TypeLesson, Title: "Memo", Slug: "memo", Body: "just a memo", Status: StatusCandidate}
	if err := st.Write(note, "add"); err != nil {
		t.Fatalf("write note: %v", err)
	}
	page := &Page{Form: FormPage, Type: TypeLesson, Title: "Page", Slug: "page", Body: "x", Status: StatusCandidate}
	if err := st.Write(page, "add"); err != nil {
		t.Fatalf("write page: %v", err)
	}
	pages, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	rep := Doctor(pages, st, nil, false)
	for _, slug := range rep.MissingDescription {
		if slug == "memo" {
			t.Errorf("doctor flagged the note for a missing description")
		}
	}
	found := false
	for _, slug := range rep.MissingDescription {
		if slug == "page" {
			found = true
		}
	}
	if !found {
		t.Errorf("doctor must still flag a page with no description")
	}
	// The CLI path: buildPage refuses a page without --description, accepts a note.
	f := &pageFlags{form: FormPage, typ: TypeLesson, title: "P", body: "x"}
	if _, err := f.buildPage(nil); err == nil {
		t.Errorf("page without description must be refused")
	}
	f = &pageFlags{form: FormNote, typ: TypeLesson, title: "N", body: "x"}
	if _, err := f.buildPage(nil); err != nil {
		t.Errorf("note without description must be accepted: %v", err)
	}
}

func TestDoctorReportsMissingIDAndDuplicateSeq(t *testing.T) {
	dir := t.TempDir()
	st := Open(dir)
	// A legacy page: written before id/seq existed (bytes, not Write).
	if err := os.MkdirAll(st.pagesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "---\nform: page\ntype: fact\ntitle: Old\ndescription: d\nstatus: validated\n---\n\nbody\n"
	if err := os.WriteFile(st.PagePath("old"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two pages that collided on seq 1 across a merge.
	writePage(t, dir, &Page{Slug: "a", Seq: 1, Type: TypeFact, Title: "A", Description: "d", Status: StatusValidated})
	writePage(t, dir, &Page{Slug: "b", Seq: 1, Type: TypeFact, Title: "B", Description: "d", Status: StatusValidated})
	pages, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	rep := Doctor(pages, st, nil, false)
	if len(rep.MissingID) != 1 || rep.MissingID[0] != "old" {
		t.Errorf("missing id = %v, want [old]", rep.MissingID)
	}
	if len(rep.DuplicateSeq) != 1 || rep.DuplicateSeq[0].Seq != 1 || len(rep.DuplicateSeq[0].Slugs) != 2 {
		t.Errorf("duplicate seq = %+v", rep.DuplicateSeq)
	}
	// doctor only READ: the legacy bytes are untouched.
	b, _ := os.ReadFile(st.PagePath("old"))
	if string(b) != legacy {
		t.Errorf("doctor rewrote a page")
	}
	// The explicit repair path: a write mints both handles, once.
	p, _ := st.Load("old")
	if err := st.Write(p, "update"); err != nil {
		t.Fatal(err)
	}
	p, _ = st.Load("old")
	if p.ID == "" || p.Seq != 2 {
		t.Errorf("update did not stamp: id=%q seq=%d", p.ID, p.Seq)
	}
}
