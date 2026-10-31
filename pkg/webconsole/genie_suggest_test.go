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
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/websession"
)

// fakeSuggester answers the read-only example-wish call: a fixed output,
// or a hang until the caller's deadline when block is set. It records what
// it was given so the test can check what it was NOT given.
type fakeSuggester struct {
	mu     sync.Mutex
	tasks  []string
	dirs   []string
	output string
	block  bool
}

func (f *fakeSuggester) Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error) {
	f.mu.Lock()
	f.tasks, f.dirs = append(f.tasks, req.Task), append(f.dirs, req.Dir)
	block, output := f.block, f.output
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return DelegateResult{}, ctx.Err()
	}
	return DelegateResult{Agent: "codex:fake-suggester", Output: output}, nil
}

func (f *fakeSuggester) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tasks)
}

func (f *fakeSuggester) lastTask() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tasks) == 0 {
		return ""
	}
	return f.tasks[len(f.tasks)-1]
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

// TestGenieSuggestExamples is the S526.B1g example-wishes proof.
// GET api/shape/<app>/suggest?page= answers the fake delegate's examples,
// generated from a brief that carries the app, the page and its STATIC
// source only — never the principal's data directory, never the principal
// — in an empty scratch directory; the answer is cached per (app, active
// revision, page) and a new version is asked again; a protected page's
// brief asks for request phrasing; a timeout answers [] silently; and an
// app that declares genie.examples in shaped.json is never asked.
func TestGenieSuggestExamples(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source := newNotesSource(t)
	genie := &fakeSuggester{output: "- make the heading bigger\n2. \"add a delete button\"\n\nsort notes by date\n"}
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	workRoot := filepath.Join(t.TempDir(), "shape-work")
	_, h, closer, err := newHandler(Options{
		Ctx: context.Background(), RequireLogin: true,
		Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Panels:   []Panel{notes},
		Shape: ShapeOptions{
			Home: home, WorkRoot: workRoot, Runner: []string{"/bin/sh"},
			Level: contain.LevelProcess, Delegate: genie, SuggestTimeout: 200 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer() })
	cloud := func(email string) http.Header {
		return http.Header{"X-Forwarded-Prefix": {"/cloud/apps"}, "Remote-User": {email}, "Remote-Email": {email}}
	}
	alice := cloud("alice@example.test")
	const alicePrincipal = "cloud:alice@example.test"
	const peer = "127.0.0.1:5555"
	suggest := func(who http.Header, path string) []string {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, who)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", path, cc)
		}
		var body struct {
			Examples []string `json:"examples"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: %v: %s", path, err, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"examples":[`) {
			t.Errorf("GET %s: examples must be a JSON array, got %s", path, w.Body.String())
		}
		return body.Examples
	}

	// Something of alice's that must never reach the brief.
	st, err := shape.OpenAt(home, "notes", alicePrincipal)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "SECRET-NOTE-9f3a"
	if err := st.WriteData("notes.txt", []byte(secret+"\n")); err != nil {
		t.Fatal(err)
	}

	// Admission: the same as every Shape route.
	if w := do(h, http.MethodGet, "/api/shape/nothere/suggest?page=/", peer, alice); w.Code != http.StatusNotFound {
		t.Errorf("unknown app = %d, want 404", w.Code)
	}
	if w := do(h, http.MethodGet, "/api/shape/notes/suggest?page=/", peer, nil); w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized && w.Code != http.StatusSeeOther {
		t.Errorf("no principal = %d, want refused", w.Code)
	}

	// The fake's examples, parsed: bullets, numbering and quotes dropped.
	got := suggest(alice, "/api/shape/notes/suggest?page=/")
	want := []string{"make the heading bigger", "add a delete button", "sort notes by date"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("examples = %q, want %q", got, want)
	}
	task := genie.lastTask()
	for _, s := range []string{`"Notes" (notes)`, "page /", "Title: Notes", "Headings: Notes", "Labels: Add", "Hints: A new note", "read-only", "<<<PAGE"} {
		if !strings.Contains(task, s) {
			t.Errorf("brief lacks %q:\n%s", s, task)
		}
	}
	for _, s := range []string{secret, "alice", "notes.txt", "PROTECTED"} {
		if strings.Contains(task, s) {
			t.Errorf("brief carries %q:\n%s", s, task)
		}
	}
	// The scratch directory was empty, under the work root, and is gone.
	dir := genie.dirs[0]
	if !strings.HasPrefix(dir, workRoot) || strings.HasPrefix(dir, home) {
		t.Errorf("scratch dir %q, want under %q and never under the home", dir, workRoot)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("scratch dir %q still exists", dir)
	}

	// Cached per (app, revision, page): the same page asks nobody again,
	// another page does, and a new active version does.
	suggest(alice, "/api/shape/notes/suggest?page=/")
	suggest(alice, "/api/shape/notes/suggest?page=/?q=1")
	if n := genie.calls(); n != 1 {
		t.Fatalf("calls after a repeat = %d, want 1 (cached)", n)
	}
	suggest(alice, "/api/shape/notes/suggest?page=/index.html")
	if n := genie.calls(); n != 2 {
		t.Fatalf("calls after another page = %d, want 2", n)
	}
	rev := commitFile(t, source, "public/about.html", "<title>About notes</title><h1>About</h1><button>Back</button>")
	if err := st.Write(shape.State{ActiveRevision: rev, LastKnownGood: rev}); err != nil {
		t.Fatal(err)
	}
	suggest(alice, "/api/shape/notes/suggest?page=/")
	if n := genie.calls(); n != 3 {
		t.Fatalf("calls after a new version = %d, want 3", n)
	}
	// The static source is the page's own file in the active revision.
	suggest(alice, "/api/shape/notes/suggest?page=/about.html")
	if task := genie.lastTask(); !strings.Contains(task, "Title: About notes") || !strings.Contains(task, "Labels: Back") {
		t.Errorf("about brief:\n%s", task)
	}
	// Bob is on base, which alice's first call already answered for: the
	// answer carries nothing personal, so one revision's examples are
	// everyone's, and Genie is not asked again.
	if got := suggest(cloud("bob@example.test"), "/api/shape/notes/suggest?page=/"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("bob's examples = %q", got)
	}
	if n := genie.calls(); n != 4 {
		t.Fatalf("calls after bob = %d, want 4 (shared with alice's base answer)", n)
	}

	// A protected page asks for request phrasing; the launcher's own trust
	// page is known protected without being told.
	suggest(alice, "/api/shape/notes/suggest?page=/&protected=1")
	if task := genie.lastTask(); !strings.Contains(task, "PROTECTED") || !strings.Contains(task, "filed as a request") {
		t.Errorf("protected brief:\n%s", task)
	}
	// A page with no static file of its own: the app's pages by name,
	// never a 404.
	if got := suggest(alice, "/api/shape/notes/suggest?page=/nowhere/"); len(got) != 3 {
		t.Errorf("unknown page examples = %q", got)
	}
	if task := genie.lastTask(); !strings.Contains(task, "no static file of its own") || !strings.Contains(task, "public/index.html") {
		t.Errorf("unknown page brief:\n%s", task)
	}

	// Timeout: [] silently, and asked again later (a failure is not pinned).
	genie.mu.Lock()
	genie.block = true
	genie.mu.Unlock()
	before := genie.calls()
	start := time.Now()
	if got := suggest(alice, "/api/shape/notes/suggest?page=/slow.html"); len(got) != 0 {
		t.Errorf("timed-out examples = %q, want none", got)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("timed-out call took %v", took)
	}
	if n := genie.calls(); n != before+1 {
		t.Errorf("calls after timeout = %d, want %d", n, before+1)
	}
	genie.mu.Lock()
	genie.block = false
	genie.mu.Unlock()

	// No delegate configured: [] and no error.
	_, h2, closer2, err := newHandler(Options{
		Ctx: context.Background(), RequireLogin: true,
		Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Panels:   []Panel{notes},
		Shape:    ShapeOptions{Home: home, WorkRoot: workRoot, Runner: []string{"/bin/sh"}, Level: contain.LevelProcess},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer2() })
	if w := do(h2, http.MethodGet, "/api/shape/notes/suggest?page=/", peer, cloud("carol@example.test")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"examples":[]`) {
		t.Errorf("no delegate = %d %s", w.Code, w.Body.String())
	}

	// shaped.json genie.examples: the app's own, and Genie is not asked.
	rev = commitFile(t, source, "shaped.json", `{"genie": {"examples": ["make the ship faster", "add a high-score table"]}}`)
	if err := st.Write(shape.State{ActiveRevision: rev, LastKnownGood: rev}); err != nil {
		t.Fatal(err)
	}
	before = genie.calls()
	got = suggest(alice, "/api/shape/notes/suggest?page=/")
	if strings.Join(got, "|") != "make the ship faster|add a high-score table" {
		t.Errorf("declared examples = %q", got)
	}
	if n := genie.calls(); n != before {
		t.Errorf("declared examples still asked Genie (%d → %d)", before, n)
	}

}

// TestParseExamples: Genie's answer in the shapes it comes — lines, a
// fenced JSON array, prose around it — becomes a bounded list of plain
// wishes.
func TestParseExamples(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"make the ship faster\nadd a high score\n", []string{"make the ship faster", "add a high score"}},
		{"Here are some wishes:\n1. make the ship faster\n2) add lives\n- *bold*\n", []string{"make the ship faster", "add lives"}},
		{"```\n[\"a bigger board\", \"a bigger board\", \"\"]\n```", []string{"a bigger board"}},
		{"[\"one wish\", \"two wish\"]", []string{"one wish", "two wish"}},
		{"\"quoted wish\"\n“smart quotes”\n", []string{"quoted wish", "smart quotes"}},
		{"<script>alert(1)</script>\nok wish\n", []string{"ok wish"}},
		{"a\nb\nc\nd\ne\nf\ng\nh\n", nil},
		{"w1 one\nw2 two\nw3 three\nw4 four\nw5 five\nw6 six\nw7 seven\n", []string{"w1 one", "w2 two", "w3 three", "w4 four", "w5 five", "w6 six"}},
		{"", nil},
	}
	for _, c := range cases {
		got := parseExamples(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("parseExamples(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := parseExamples(strings.Repeat("x", 200) + "\nfine wish\n"); len(got) != 1 || got[0] != "fine wish" {
		t.Errorf("overlong line kept: %q", got)
	}
}

// TestStaticExcerpt: the brief's page excerpt is bounded and carries no
// script or style body.
func TestStaticExcerpt(t *testing.T) {
	source := newNotesSource(t)
	big := "<!doctype html><title>Big</title><style>body{color:red}</style><script>const SECRET_JS = 1;</script><h1>Big page</h1>" + strings.Repeat("<p>lorem ipsum</p>", 2000)
	rev := commitFile(t, source, "public/big.html", big)
	r, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := treeOf(r, rev)
	if err != nil {
		t.Fatal(err)
	}
	got := staticExcerpt(tree, "/big.html", suggestPageFile("notes", "/big.html"))
	if len(got) > suggestExcerptLimit+64 {
		t.Errorf("excerpt is %d bytes, want ≤ %d", len(got), suggestExcerptLimit)
	}
	for _, s := range []string{"Title: Big", "Headings: Big page", "Markup:"} {
		if !strings.Contains(got, s) {
			t.Errorf("excerpt lacks %q:\n%s", s, got)
		}
	}
	for _, s := range []string{"SECRET_JS", "color:red"} {
		if strings.Contains(got, s) {
			t.Errorf("excerpt carries %q", s)
		}
	}
	if f := suggestPageFile(consoleApp, "sprint/"); f != "board.html" {
		t.Errorf("console sprint page file = %q", f)
	}
	if f := suggestPageFile("notes", "/sprint/"); f != "sprint/index.html" {
		t.Errorf("notes sprint page file = %q", f)
	}
	if p := suggestPage("/a/../b?x=1#f"); p != "/b" {
		t.Errorf("suggestPage = %q", p)
	}
}
