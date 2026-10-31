// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
)

// nextShaper is fakeShaper whose answer ends with the follow-up list
// (NEXT: lines) in next, when set.
type nextShaper struct {
	fakeShaper
	nmu  sync.Mutex
	next string
}

func (n *nextShaper) Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error) {
	res, err := n.fakeShaper.Delegate(ctx, req)
	if err != nil {
		return res, err
	}
	n.nmu.Lock()
	res.Output += n.next
	n.nmu.Unlock()
	return res, nil
}

func (n *nextShaper) setNext(s string) {
	n.nmu.Lock()
	n.next = s
	n.nmu.Unlock()
}

func (n *nextShaper) calls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.tasks)
}

// commitFile adds one file to the source checkout as a new commit and
// returns its hash.
func commitFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit("notes: "+name, &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.test", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	return h.String()
}

// TestGenieSuggestNextSteps is the S526.B1g (revision) example-wishes
// proof: opening the wish panel never calls the delegate. A finished wish
// whose answer lists NEXT: lines stores them with the version — on the
// draft step and as Shape-Next trailers of its commit — and the next panel
// open shows them; a version without NEXT derives follow-ups from its own
// wish; a version with neither (the base) answers the cached copy for this
// app and page, or nothing; shaped.json genie.examples wins over all.
func TestGenieSuggestNextSteps(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source := newNotesSource(t)
	shaper := &nextShaper{next: "NEXT:\n- Add a delete button\n* \"make the heading bigger\"\n- (none)\n"}
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	opts := Options{Panels: []Panel{notes}, Shape: ShapeOptions{
		Home: home, WorkRoot: filepath.Join(t.TempDir(), "shape-work"), Runner: []string{"/bin/sh"},
		Level: contain.LevelProcess, Delegate: shaper, Verifier: &switchVerifier{pass: true}, Activator: &fakeActivator{home: home},
	}}
	h := newTestHandler(t, opts)
	const peer = "127.0.0.1:5555"
	get := func(path string, want int) string {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, nil)
		if w.Code != want {
			t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	post := func(path, body string, want int) string {
		t.Helper()
		hdr := http.Header{}
		if body != "" {
			hdr.Set("Content-Type", "application/json")
		}
		w := send(h, http.MethodPost, path, peer, hdr, body)
		if w.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	suggest := func(path string) []string {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", path, cc)
		}
		if !strings.Contains(w.Body.String(), `"examples":[`) {
			t.Errorf("GET %s: examples must be a JSON array, got %s", path, w.Body.String())
		}
		var body struct {
			Examples []string `json:"examples"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: %v: %s", path, err, w.Body.String())
		}
		return body.Examples
	}
	expect := func(what string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s = %q, want %q", what, got, want)
		}
	}
	noCall := func(what string, want int) {
		t.Helper()
		if n := shaper.calls(); n != want {
			t.Fatalf("delegate calls after %s = %d, want %d (a panel open never calls Genie)", what, n, want)
		}
	}
	settle := func() draftView {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			var v draftView
			if err := json.Unmarshal([]byte(get("/api/shape/notes/draft", http.StatusOK)), &v); err != nil {
				t.Fatal(err)
			}
			if !v.Busy {
				if v.LastError != "" {
					t.Fatalf("the wish made no step: %s", v.LastError)
				}
				return v
			}
			if time.Now().After(deadline) {
				t.Fatalf("the wish never settled: %+v", v)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	var state struct {
		Principal string `json:"principal"`
	}
	if err := json.Unmarshal([]byte(get("/api/shape/notes", http.StatusOK)), &state); err != nil || state.Principal == "" {
		t.Fatalf("principal: %v %q", err, state.Principal)
	}
	st, err := shape.OpenAt(home, "notes", state.Principal)
	if err != nil {
		t.Fatal(err)
	}
	activeMessage := func() string {
		t.Helper()
		s, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		r, err := gogit.PlainOpen(source)
		if err != nil {
			t.Fatal(err)
		}
		c, err := r.CommitObject(plumbing.NewHash(s.ActiveRevision))
		if err != nil {
			t.Fatalf("active %s: %v", s.ActiveRevision, err)
		}
		return c.Message
	}

	// Admission: the same as every Shape route.
	get("/api/shape/nothere/suggest?page=/", http.StatusNotFound)

	// 4. The base: nothing stored, nothing cached — the neutral placeholder
	// — and Genie was not asked.
	expect("base", suggest("/api/shape/notes/suggest?page=/"))
	noCall("the first open", 0)

	// 1. A wish in the edit session: Genie's NEXT: lines land on the step
	// (cleaned: markers, quotes, "(none)" dropped) and the draft's panel
	// shows them; the active version is still the base.
	post("/api/shape/notes/draft", "", http.StatusCreated)
	post("/api/shape/notes/draft/wishes", `{"request":"show newest notes first","page":"/","element":""}`, http.StatusAccepted)
	v := settle()
	noCall("one wish", 1)
	expect("draft panel", suggest("/api/shape/notes/suggest?page=/&draft=1"), "Add a delete button", "make the heading bigger")
	noCall("the draft open", 1)
	if len(v.Steps) != 1 || strings.Join(v.Steps[0].Next, "|") != "Add a delete button|make the heading bigger" {
		t.Errorf("draft step next = %+v", v.Steps)
	}

	// Saved: the version's commit carries the follow-ups as trailers and
	// every page of the app shows them on open, without Genie.
	post("/api/shape/notes/draft/save", "", http.StatusOK)
	msg := activeMessage()
	if !strings.Contains(msg, "\nShape-Next: Add a delete button\nShape-Next: make the heading bigger\n") {
		t.Errorf("active commit message lacks the Shape-Next trailers:\n%s", msg)
	}
	expect("saved version", suggest("/api/shape/notes/suggest?page=/"), "Add a delete button", "make the heading bigger")
	expect("saved version, another page", suggest("/api/shape/notes/suggest?page=/about.html"), "Add a delete button", "make the heading bigger")
	expect("saved version, draft flag with no draft", suggest("/api/shape/notes/suggest?page=/&draft=1"), "Add a delete button", "make the heading bigger")
	noCall("the saved version's opens", 1)

	// 2. A version without NEXT (an older Genie, a terse answer): simple
	// follow-ups from its own wish, no model call.
	shaper.setNext("")
	post("/api/shape/notes/draft", "", http.StatusCreated)
	post("/api/shape/notes/draft/wishes", `{"request":"Add a delete button to each note.","page":"/","element":""}`, http.StatusAccepted)
	settle()
	noCall("two wishes", 2)
	expect("derived on the draft", suggest("/api/shape/notes/suggest?page=/&draft=1"), "Change the delete button to each note", "Undo add a delete button to each note")
	post("/api/shape/notes/draft/save", "", http.StatusOK)
	if msg := activeMessage(); strings.Contains(msg, "Shape-Next:") {
		t.Errorf("a version without NEXT carries Shape-Next:\n%s", msg)
	}
	expect("derived on the version", suggest("/api/shape/notes/suggest?page=/"), "Change the delete button to each note", "Undo add a delete button to each note")
	noCall("the derived opens", 2)

	// 3. Back on the base (a restore): the cached copy for this app and
	// page; a page never opened has none.
	r, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	baseRev := head.Hash().String()
	if err := st.Write(shape.State{ActiveRevision: baseRev, LastKnownGood: baseRev}); err != nil {
		t.Fatal(err)
	}
	expect("cached copy", suggest("/api/shape/notes/suggest?page=/"), "Change the delete button to each note", "Undo add a delete button to each note")
	expect("cached copy, unseen page", suggest("/api/shape/notes/suggest?page=/never.html"))
	noCall("the cached opens", 2)

	// After a restart the cache is gone: the base answers nothing — the
	// generic placeholder — and still no call.
	h2 := newTestHandler(t, opts)
	if w := do(h2, http.MethodGet, "/api/shape/notes/suggest?page=/", peer, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"examples":[]`) {
		t.Errorf("after restart = %d %s", w.Code, w.Body.String())
	}
	noCall("the restart open", 2)

	// shaped.json genie.examples: the app's own, before every other source.
	rev := commitFile(t, source, "shaped.json", `{"genie": {"examples": ["make the ship faster", "add a high-score table"]}}`)
	if err := st.Write(shape.State{ActiveRevision: rev, LastKnownGood: rev}); err != nil {
		t.Fatal(err)
	}
	expect("declared examples", suggest("/api/shape/notes/suggest?page=/"), "make the ship faster", "add a high-score table")
	noCall("the declared open", 2)

	// No delegate configured at all: still a plain answer.
	h3 := newTestHandler(t, Options{Panels: []Panel{notes}, Shape: ShapeOptions{
		Home: home, WorkRoot: filepath.Join(t.TempDir(), "shape-work"), Runner: []string{"/bin/sh"}, Level: contain.LevelProcess}})
	if w := do(h3, http.MethodGet, "/api/shape/notes/suggest?page=/", peer, nil); w.Code != http.StatusOK {
		t.Errorf("no delegate = %d %s", w.Code, w.Body.String())
	}
}

// TestSuggestCachePerPrincipal: the cached copy (source 3) is one
// principal's — a derived follow-up repeats their wish — and a non-empty
// answer replaces it.
func TestSuggestCachePerPrincipal(t *testing.T) {
	s := &server{}
	alice := suggestKey{app: "notes", principal: "device:alice", page: "/"}
	bob := suggestKey{app: "notes", principal: "device:bob", page: "/"}
	if got := s.rememberSuggestions(alice, nil); got == nil || len(got) != 0 {
		t.Errorf("empty cache = %#v, want []", got)
	}
	s.rememberSuggestions(alice, []string{"Undo add a secret page"})
	if got := s.rememberSuggestions(bob, nil); len(got) != 0 {
		t.Errorf("bob reads alice's copy: %q", got)
	}
	if got := s.rememberSuggestions(alice, nil); strings.Join(got, "|") != "Undo add a secret page" {
		t.Errorf("alice's copy = %q", got)
	}
	s.rememberSuggestions(alice, []string{"newer"})
	if got := s.rememberSuggestions(alice, nil); strings.Join(got, "|") != "newer" {
		t.Errorf("alice's replaced copy = %q", got)
	}
}

// TestParseNextSteps: Genie's follow-up list in the shapes it comes —
// after the coverage list, with markers, quotes, "(none)" and prose —
// becomes a bounded list of plain wishes, and the coverage list before it
// is still read whole.
func TestParseNextSteps(t *testing.T) {
	answer := "Changed the list.\n\nCOVERED:\n- newest first\nNOT COVERED:\n- export remains to do\nNEXT:\n- add a delete button\n- \"show a count of notes\"\n- (none)\n"
	if got := parseNextSteps(answer); strings.Join(got, "|") != "add a delete button|show a count of notes" {
		t.Errorf("parseNextSteps = %q", got)
	}
	cov := parseCoverageSummary(answer)
	if cov == nil || strings.Join(cov.NotCovered, "|") != "export remains to do" || strings.Join(cov.Covered, "|") != "newest first" {
		t.Errorf("coverage with NEXT after it = %+v", cov)
	}
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"COVERED:\n- a\nNOT COVERED:\n- (none)\n", nil},
		{"**NEXT:**\n* make the ship faster\n* add lives\n\nsome prose\n- not a wish\n", []string{"make the ship faster", "add lives"}},
		{"NEXT:\n- first list\nNEXT:\n- the last list wins\n", []string{"the last list wins"}},
		{"NEXT:\n- <script>alert(1)</script>\n- ok wish\n- ok wish\n", []string{"ok wish"}},
		{"NEXT:\n- w1 one\n- w2 two\n- w3 three\n- w4 four\n- w5 five\n- w6 six\n- w7 seven\n", []string{"w1 one", "w2 two", "w3 three", "w4 four", "w5 five", "w6 six"}},
		{"NEXT:\n- " + strings.Repeat("x", 200) + "\n- fine wish\n", []string{"fine wish"}},
	}
	for _, c := range cases {
		if got := parseNextSteps(c.in); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("parseNextSteps(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDeriveFollowUps: the follow-ups built from a wish, and the trailer
// reader they rest on.
func TestDeriveFollowUps(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"add a delete button", []string{"Change the delete button", "Undo add a delete button"}},
		{"Make the ship faster.", []string{"Change the ship faster", "Undo make the ship faster"}},
		{"show   newest\nnotes first", []string{"Change newest notes first", "Undo show newest notes first"}},
		{"please add an export link", []string{"Change the export link", "Undo please add an export link"}},
		{"dark mode", []string{"Change dark mode", "Undo dark mode"}},
		{"CSV export", []string{"Change CSV export", "Undo CSV export"}},
		{"   ", nil},
	}
	for _, c := range cases {
		if got := deriveFollowUps(c.in); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("deriveFollowUps(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	msg := "shape: add a delete button\n\nShape-Request: add a delete button\nShape-Principal: device:x\nShape-Agent: codex\nShape-Next: show a count\nShape-Next: undo the button\n"
	c := &object.Commit{Message: msg}
	if got := versionSuggestions(c); strings.Join(got, "|") != "show a count|undo the button" {
		t.Errorf("versionSuggestions(next) = %q", got)
	}
	c.Message = "shape: add a delete button\n\nShape-Request: add a delete button\nShape-Principal: device:x\n"
	if got := versionSuggestions(c); strings.Join(got, "|") != "Change the delete button|Undo add a delete button" {
		t.Errorf("versionSuggestions(derived) = %q", got)
	}
	c.Message = "notes: base\n"
	if got := versionSuggestions(c); got != nil {
		t.Errorf("versionSuggestions(base) = %q", got)
	}
	// Trailers are read from the last paragraph only.
	if got := trailerValues("Shape-Next: in the body\n\nShape-Request: x\n", "Shape-Next:"); got != nil {
		t.Errorf("body line read as a trailer: %q", got)
	}
	if p := suggestPage("/a/../b?x=1#f"); p != "/b" {
		t.Errorf("suggestPage = %q", p)
	}
}
