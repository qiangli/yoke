// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/websession"
)

// The edit session (S526.B1h): red/green proofs for the step stack (wish,
// undo, redo, a new wish clears the redo list), Save as ONE verified
// version listing every wish, Cancel leaving the active version
// byte-identical, and the draft surviving a console restart.

// switchVerifier refuses while pass is false and passes afterwards,
// recording every call: Save must verify the whole draft exactly once.
type switchVerifier struct {
	mu     sync.Mutex
	pass   bool
	detail string
	calls  []VerifyRequest
}

func (v *switchVerifier) Verify(_ context.Context, req VerifyRequest) (Verdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, req)
	if !v.pass {
		return Verdict{Pass: false, Detail: v.detail}, nil
	}
	return Verdict{Pass: true, Evidence: "evidence-" + req.CandidateRev[:8]}, nil
}

func (v *switchVerifier) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.calls)
}

// draftEnv is a console with the notes app shaped by a fake agent, reached
// as the loopback operator (the OS principal): no pairing needed.
type draftEnv struct {
	t         *testing.T
	h         http.Handler
	opts      Options
	shaper    *fakeShaper
	verifier  *switchVerifier
	activator *fakeActivator
	home      string
}

const draftPeer = "127.0.0.1:5555"

func newDraftEnv(t *testing.T) *draftEnv {
	t.Helper()
	home := t.TempDir()
	source := newNotesSource(t)
	e := &draftEnv{t: t, home: home, shaper: &fakeShaper{}, verifier: &switchVerifier{pass: true},
		activator: &fakeActivator{home: home}}
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	e.opts = Options{Panels: []Panel{notes}, Shape: ShapeOptions{
		Home: home, WorkRoot: filepath.Join(t.TempDir(), "shape-work"), Runner: []string{"/bin/sh"},
		Level: contain.LevelProcess, Delegate: e.shaper, Verifier: e.verifier, Activator: e.activator,
	}}
	e.h = newTestHandler(t, e.opts)
	return e
}

// restart builds a fresh handler over the same home and work root: what a
// console restart leaves the draft with.
func (e *draftEnv) restart() { e.h = newTestHandler(e.t, e.opts) }

func (e *draftEnv) get(path string) (int, string) {
	w := do(e.h, http.MethodGet, path, draftPeer, nil)
	return w.Code, w.Body.String()
}

func (e *draftEnv) post(path, body string) (int, string) {
	hdr := http.Header{}
	if body != "" {
		hdr.Set("Content-Type", "application/json")
	}
	w := send(e.h, http.MethodPost, path, draftPeer, hdr, body)
	return w.Code, w.Body.String()
}

func (e *draftEnv) mustGet(path string, want int) string {
	e.t.Helper()
	code, body := e.get(path)
	if code != want {
		e.t.Fatalf("GET %s = %d, want %d: %s", path, code, want, body)
	}
	return body
}

func (e *draftEnv) mustPost(path, body string, want int) string {
	e.t.Helper()
	code, out := e.post(path, body)
	if code != want {
		e.t.Fatalf("POST %s = %d, want %d: %s", path, code, want, out)
	}
	return out
}

func (e *draftEnv) draft() draftView {
	e.t.Helper()
	var v draftView
	if err := json.Unmarshal([]byte(e.mustGet("/api/shape/notes/draft", http.StatusOK)), &v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// wish adds a step and waits for the agent.
func (e *draftEnv) wish(text string) draftView {
	e.t.Helper()
	e.mustPost("/api/shape/notes/draft/wishes", `{"request":`+jsonQuote(text)+`,"page":"/","element":""}`, http.StatusAccepted)
	return e.settle()
}

func (e *draftEnv) settle() draftView {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		v := e.draft()
		if !v.Busy {
			if v.LastError != "" {
				e.t.Fatalf("the wish made no step: %s", v.LastError)
			}
			return v
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the wish never settled: %+v", v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *draftEnv) tasks() int {
	e.shaper.mu.Lock()
	defer e.shaper.mu.Unlock()
	return len(e.shaper.tasks)
}

func (e *draftEnv) principal() string {
	e.t.Helper()
	var st struct {
		Principal string `json:"principal"`
	}
	if err := json.Unmarshal([]byte(e.mustGet("/api/shape/notes", http.StatusOK)), &st); err != nil {
		e.t.Fatal(err)
	}
	if st.Principal == "" {
		e.t.Fatal("no principal for the loopback operator")
	}
	return st.Principal
}

func (e *draftEnv) activeRevision() string {
	e.t.Helper()
	st, err := shape.OpenAt(e.home, "notes", e.principal())
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := st.Read()
	if err != nil {
		e.t.Fatal(err)
	}
	return s.ActiveRevision
}

func requestsOf(v draftView) []string {
	out := []string{}
	for _, s := range v.Steps {
		out = append(out, s.Request)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDraftStepStack: wish, undo, redo, a new wish clears the redo list;
// undo and redo never re-run the agent; the live app never changes.
func TestDraftStepStack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture units run under /bin/sh")
	}
	e := newDraftEnv(t)
	const live, draftPage = "/notes/", "/shape/notes/draft/"
	liveBefore := e.mustGet(live, http.StatusOK)

	// No draft yet: the page and the API say so; the Shape state has none.
	e.mustGet("/api/shape/notes/draft", http.StatusNotFound)
	e.mustGet(draftPage, http.StatusNotFound)
	if body := e.mustGet("/api/shape/notes", http.StatusOK); !strings.Contains(body, `"draft":null`) {
		t.Fatalf("shape state without a draft: %s", body)
	}

	// Start: a draft of the active version, with nothing applied yet.
	var v draftView
	if err := json.Unmarshal([]byte(e.mustPost("/api/shape/notes/draft", "", http.StatusCreated)), &v); err != nil {
		t.Fatal(err)
	}
	if v.Wishes != 0 || v.Total != 0 || v.CanUndo || v.CanRedo || v.BaseRev == "" || v.Rev != v.BaseRev {
		t.Fatalf("fresh draft = %+v", v)
	}
	// Starting again resumes the same draft.
	e.mustPost("/api/shape/notes/draft", "", http.StatusOK)
	if !strings.Contains(e.mustGet(draftPage, http.StatusOK), "<h1>Notes</h1>") {
		t.Fatal("the fresh draft does not show the active version")
	}
	if got := e.mustGet(draftPage, http.StatusOK); !strings.Contains(got, `data-mode="draft"`) {
		t.Fatal("the draft page does not carry the component in draft mode")
	}

	// Two wishes: two steps, both applied, the draft shows the second.
	v = e.wish("show the newest note first")
	if v.Wishes != 1 || v.Total != 1 || !v.CanUndo || v.CanRedo || v.Steps[0].Rev == "" || v.Steps[0].Status != stepReady || v.Steps[0].Parent != v.BaseRev {
		t.Fatalf("after one wish = %+v", v)
	}
	v = e.wish("tint the notes blue")
	if v.Wishes != 2 || v.Total != 2 || v.Steps[1].Parent != v.Steps[0].Rev || v.Rev != v.Steps[1].Rev {
		t.Fatalf("after two wishes = %+v", v)
	}
	one, two := v.Steps[0].Rev, v.Steps[1].Rev
	if page := e.mustGet(draftPage, http.StatusOK); !strings.Contains(page, "<!-- shaped 2 -->") || !strings.Contains(page, "newest first") {
		t.Fatalf("the draft does not show both wishes: %s", page)
	}
	if e.tasks() != 2 {
		t.Fatalf("agent ran %d times, want 2", e.tasks())
	}

	// Undo: the cursor steps back, the page shows the first wish only, and
	// the agent did not run.
	if err := json.Unmarshal([]byte(e.mustPost("/api/shape/notes/draft/undo", "", http.StatusOK)), &v); err != nil {
		t.Fatal(err)
	}
	if v.Wishes != 1 || v.Total != 2 || !v.CanRedo || !v.CanUndo || v.Rev != one {
		t.Fatalf("after undo = %+v", v)
	}
	if page := e.mustGet(draftPage, http.StatusOK); strings.Contains(page, "<!-- shaped 2 -->") || !strings.Contains(page, "<!-- shaped 1 -->") {
		t.Fatal("the draft page after undo does not show the first wish only")
	}
	// Redo brings it back; still no agent run.
	if err := json.Unmarshal([]byte(e.mustPost("/api/shape/notes/draft/redo", "", http.StatusOK)), &v); err != nil {
		t.Fatal(err)
	}
	if v.Wishes != 2 || v.CanRedo || v.Rev != two {
		t.Fatalf("after redo = %+v", v)
	}
	if page := e.mustGet(draftPage, http.StatusOK); !strings.Contains(page, "<!-- shaped 2 -->") {
		t.Fatal("the draft page after redo does not show the second wish")
	}
	if e.tasks() != 2 {
		t.Fatalf("undo/redo re-ran the agent: %d runs", e.tasks())
	}
	// Past the ends: refused, nothing moves.
	e.mustPost("/api/shape/notes/draft/redo", "", http.StatusConflict)
	e.mustPost("/api/shape/notes/draft/undo", "", http.StatusOK)
	e.mustPost("/api/shape/notes/draft/undo", "", http.StatusOK)
	if code, body := e.post("/api/shape/notes/draft/undo", ""); code != http.StatusConflict || !strings.Contains(body, "nothing to undo") {
		t.Fatalf("undo at the bottom = %d %s", code, body)
	}
	if v = e.draft(); v.Wishes != 0 || v.Total != 2 || v.Rev != v.BaseRev {
		t.Fatalf("at the bottom = %+v", v)
	}
	e.mustPost("/api/shape/notes/draft/redo", "", http.StatusOK) // back to one applied

	// A new wish clears the redo list: the second wish is dropped (kept
	// for the record as undone), the third sits on the first.
	v = e.wish("add an export button")
	if !sameStrings(requestsOf(v), []string{"show the newest note first", "add an export button"}) || v.Wishes != 2 || v.CanRedo {
		t.Fatalf("after a new wish over an undone one = %+v", v)
	}
	if v.Steps[1].Parent != one {
		t.Fatalf("the new wish was not applied on the draft so far: parent %s, want %s", v.Steps[1].Parent, one)
	}
	if len(v.Dropped) != 1 || v.Dropped[0].Request != "tint the notes blue" {
		t.Fatalf("dropped = %+v, want the undone wish", v.Dropped)
	}
	e.mustPost("/api/shape/notes/draft/redo", "", http.StatusConflict)
	if e.tasks() != 3 {
		t.Fatalf("agent ran %d times, want 3", e.tasks())
	}

	// A wish from a protected page is a request, never a step.
	if body := e.mustPost("/api/shape/notes/draft/wishes", `{"request":"a back button on the Shape page","page":"/shape/notes/","protected":true}`, http.StatusAccepted); !strings.Contains(body, `"status":"requested"`) {
		t.Fatalf("protected wish = %s", body)
	}
	if v = e.draft(); v.Total != 2 || v.Wishes != 2 {
		t.Fatalf("a protected wish changed the draft: %+v", v)
	}
	if body := e.mustGet("/api/shape/notes", http.StatusOK); !strings.Contains(body, `"kind":"request"`) || !strings.Contains(body, `"draft":{`) {
		t.Fatalf("shape state lacks the filed request or the draft: %s", body)
	}

	// Through all of it the live app is the active version, byte for byte.
	if got := e.mustGet(live, http.StatusOK); got != liveBefore {
		t.Fatal("the live app changed during the edit session")
	}
	if e.activeRevision() != "" {
		t.Fatal("an edit session activated something")
	}
}

// TestDraftSaveOneVerifiedVersion: Save verifies the whole draft exactly
// once (base..tip) and activates it as one version whose request lists
// every wish, the undone one marked; a refusal keeps the draft open with
// the failing part named; an all-undone draft has nothing to save.
func TestDraftSaveOneVerifiedVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture units run under /bin/sh")
	}
	e := newDraftEnv(t)
	const live = "/notes/"
	liveBefore := e.mustGet(live, http.StatusOK)

	e.mustPost("/api/shape/notes/draft", "", http.StatusCreated)
	e.wish("show the newest note first")
	e.wish("tint the notes blue")
	v := e.wish("add an export button")
	e.mustPost("/api/shape/notes/draft/undo", "", http.StatusOK) // the export button is undone
	tip := v.Steps[1].Rev

	// Refused: the verifier names the failing part; the draft stays open,
	// nothing was activated, the live app is unchanged.
	e.verifier.mu.Lock()
	e.verifier.pass, e.verifier.detail = false, "smoke check failed: the heading must keep its text"
	e.verifier.mu.Unlock()
	code, body := e.post("/api/shape/notes/draft/save", "")
	if code != http.StatusConflict || !strings.Contains(body, "the heading must keep its text") {
		t.Fatalf("refused save = %d %s", code, body)
	}
	if v = e.draft(); v.Verdict == nil || v.Verdict.Pass || v.Wishes != 2 || v.Total != 3 {
		t.Fatalf("draft after a refused save = %+v", v)
	}
	if len(e.activator.calls) != 0 || e.activeRevision() != "" {
		t.Fatal("a refused save activated something")
	}
	if e.mustGet(live, http.StatusOK) != liveBefore {
		t.Fatal("a refused save changed the live app")
	}

	// Passed: one verification over base..tip, one activation listing every
	// wish, the draft gone, the live app now the saved version.
	e.verifier.mu.Lock()
	e.verifier.pass = true
	e.verifier.mu.Unlock()
	before := e.verifier.count()
	var saved draftSaved
	if err := json.Unmarshal([]byte(e.mustPost("/api/shape/notes/draft/save", "", http.StatusOK)), &saved); err != nil {
		t.Fatal(err)
	}
	if e.verifier.count() != before+1 {
		t.Fatalf("Save verified %d times, want once", e.verifier.count()-before)
	}
	call := e.verifier.calls[len(e.verifier.calls)-1]
	if call.BaseRev != v.BaseRev || call.CandidateRev != tip || call.App != "notes" {
		t.Fatalf("verified %+v, want base %s..tip %s", call, v.BaseRev, tip)
	}
	if len(e.activator.calls) != 1 {
		t.Fatalf("activations = %d, want 1", len(e.activator.calls))
	}
	act := e.activator.calls[0]
	if act.CandidateRev != tip || act.BaseRev != v.BaseRev || act.Evidence == "" {
		t.Fatalf("activated %+v", act)
	}
	for _, want := range []string{"Edit session, 2 wishes", "1. show the newest note first", "2. tint the notes blue", "(undone) add an export button"} {
		if !strings.Contains(act.Request, want) {
			t.Fatalf("version request %q lacks %q", act.Request, want)
		}
	}
	if saved.Request != act.Request || saved.Wishes != 2 || saved.Undone != 1 || saved.Revision != tip {
		t.Fatalf("save response = %+v", saved)
	}
	if e.tasks() != 3 {
		t.Fatalf("Save re-ran the agent: %d runs", e.tasks())
	}
	e.mustGet("/api/shape/notes/draft", http.StatusNotFound)
	e.mustGet("/shape/notes/draft/", http.StatusNotFound)
	if e.activeRevision() != tip {
		t.Fatalf("active revision = %s, want the saved tip %s", e.activeRevision(), tip)
	}
	if page := e.mustGet(live, http.StatusOK); !strings.Contains(page, "<!-- shaped 2 -->") || strings.Contains(page, "<!-- shaped 3 -->") {
		t.Fatalf("the live app is not the saved draft: %s", page)
	}

	// Nothing applied: nothing to save.
	e.mustPost("/api/shape/notes/draft", "", http.StatusCreated)
	e.wish("one more")
	e.mustPost("/api/shape/notes/draft/undo", "", http.StatusOK)
	if code, body := e.post("/api/shape/notes/draft/save", ""); code != http.StatusConflict || !strings.Contains(body, "nothing to save") {
		t.Fatalf("save with nothing applied = %d %s", code, body)
	}
	if e.verifier.count() != before+1 {
		t.Fatal("an empty save ran the verifier")
	}
}

// TestDraftCancelByteIdentical: Cancel discards the draft and the active
// version — its revision and its bytes — is exactly what it was.
func TestDraftCancelByteIdentical(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture units run under /bin/sh")
	}
	e := newDraftEnv(t)
	const live = "/notes/"

	// An active version first, through the ordinary candidate flow, so the
	// draft starts from a shaped version and not the base.
	var c Candidate
	if err := json.Unmarshal([]byte(e.mustPost("/api/shape/notes/candidates", `{"request":"show the newest note first"}`, http.StatusAccepted)), &c); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for c.Status == candRunning && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		_ = json.Unmarshal([]byte(e.mustGet("/api/shape/notes/candidates/"+c.ID, http.StatusOK)), &c)
	}
	if c.Status != candReady {
		t.Fatalf("candidate = %+v", c)
	}
	e.mustPost("/api/shape/notes/candidates/"+c.ID+"/approve", "", http.StatusOK)
	activeBefore := e.activeRevision()
	liveBefore := e.mustGet(live, http.StatusOK)
	if activeBefore == "" || !strings.Contains(liveBefore, "<!-- shaped 1 -->") {
		t.Fatalf("no active version to edit: %q", activeBefore)
	}

	var v draftView
	if err := json.Unmarshal([]byte(e.mustPost("/api/shape/notes/draft", "", http.StatusCreated)), &v); err != nil {
		t.Fatal(err)
	}
	if v.BaseRev != activeBefore {
		t.Fatalf("draft base = %s, want the active version %s", v.BaseRev, activeBefore)
	}
	e.wish("tint the notes blue")
	e.wish("add an export button")
	if page := e.mustGet("/shape/notes/draft/", http.StatusOK); !strings.Contains(page, "<!-- shaped 3 -->") {
		t.Fatal("the draft does not show its wishes")
	}
	body := e.mustPost("/api/shape/notes/draft/cancel", "", http.StatusOK)
	if !strings.Contains(body, `"cancelled":true`) {
		t.Fatalf("cancel = %s", body)
	}
	e.mustGet("/api/shape/notes/draft", http.StatusNotFound)
	e.mustGet("/shape/notes/draft/", http.StatusNotFound)
	e.mustPost("/api/shape/notes/draft/cancel", "", http.StatusNotFound)
	if e.activeRevision() != activeBefore {
		t.Fatalf("active revision after cancel = %s, want %s", e.activeRevision(), activeBefore)
	}
	if got := e.mustGet(live, http.StatusOK); got != liveBefore {
		t.Fatal("the live app is not byte-identical after cancel")
	}
	if len(e.activator.calls) != 1 || e.verifier.count() != 1 {
		t.Fatalf("cancel verified or activated: %d verifications, %d activations", e.verifier.count(), len(e.activator.calls))
	}
	// The workspace is gone too.
	dir, _ := e.activeDraftDir()
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the draft workspace survived cancel")
	}
}

func (e *draftEnv) activeDraftDir() (string, error) {
	root, _ := e.opts.Shape.workRoot()
	return filepath.Join(root, "notes", principalKey(e.principal()), draftDirName), nil
}

// TestDraftPersistence: the draft — its steps, its cursor, its workspace —
// survives a console restart (a reload and a closed tab are the same
// thing to the server), and editing continues where it stopped.
func TestDraftPersistence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture units run under /bin/sh")
	}
	e := newDraftEnv(t)
	e.mustPost("/api/shape/notes/draft", "", http.StatusCreated)
	e.wish("show the newest note first")
	before := e.wish("tint the notes blue")
	e.mustPost("/api/shape/notes/draft/undo", "", http.StatusOK)

	e.restart()
	after := e.draft()
	if after.BaseRev != before.BaseRev || after.Cursor != 1 || !sameStrings(requestsOf(after), requestsOf(before)) ||
		after.Steps[0].Rev != before.Steps[0].Rev || after.Steps[1].Rev != before.Steps[1].Rev || !after.CanRedo {
		t.Fatalf("draft after restart = %+v, want the same stack with one undone (before %+v)", after, before)
	}
	if page := e.mustGet("/shape/notes/draft/", http.StatusOK); !strings.Contains(page, "<!-- shaped 1 -->") || strings.Contains(page, "<!-- shaped 2 -->") {
		t.Fatal("the draft page after restart is not the tip")
	}
	// Editing continues: redo, then a third wish on top, then Save.
	e.mustPost("/api/shape/notes/draft/redo", "", http.StatusOK)
	v := e.wish("add an export button")
	if v.Wishes != 3 || v.Steps[2].Parent != before.Steps[1].Rev {
		t.Fatalf("after restart and a wish = %+v", v)
	}
	e.mustPost("/api/shape/notes/draft/save", "", http.StatusOK)
	e.mustGet("/api/shape/notes/draft", http.StatusNotFound)
	if len(e.activator.calls) != 1 || !strings.Contains(e.activator.calls[0].Request, "Edit session, 3 wishes") {
		t.Fatalf("activations after restart = %+v", e.activator.calls)
	}
}

// TestDraftSaveRecordsHistory is the Save proof through the production
// verifier and activator: three wishes, one undone, one Save, and the
// version history shows ONE new version whose request lists every wish.
func TestDraftSaveRecordsHistory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture units run under /bin/sh")
	}
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("PATH", t.TempDir())
	source, _ := productionNotesSource(t, home)

	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner = home, filepath.Join(t.TempDir(), "shape-work"), []string{"/bin/sh"}
	opts.Delegate = &staticShaper{}
	opts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
	opts.Activator.(*productionActivator).home = home
	panel := Panel{Name: "notes", Label: "Notes", Path: "/notes/", LaunchPath: "/shape/notes/",
		ShapedSource: source, Auth: AuthSystem, Source: "registered", Available: true}
	pairPath := filepath.Join(home, "pairing.json")
	h := newTestHandler(t, Options{
		RequireLogin: true, Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Pairing: true, PairStorePath: pairPath, Panels: []Panel{panel}, Shape: opts,
	})
	alice := pairedPrincipal(t, h, newPairStore(pairPath), "alice")
	const peer = "192.0.2.20:5555"
	post := func(path, body string, want int) string {
		t.Helper()
		hdr := alice.Clone()
		hdr.Set("Content-Type", "application/json")
		w := send(h, http.MethodPost, path, peer, hdr, body)
		if w.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	get := func(path string, want int) string {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, alice)
		if w.Code != want {
			t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	wish := func(text string) draftView {
		t.Helper()
		post("/api/shape/notes/draft/wishes", `{"request":`+jsonQuote(text)+`}`, http.StatusAccepted)
		deadline := time.Now().Add(30 * time.Second)
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
			time.Sleep(50 * time.Millisecond)
		}
	}

	post("/api/shape/notes/draft", "", http.StatusCreated)
	wish("show the newest note first")
	wish("tint the notes blue")
	v := wish("add an export button")
	post("/api/shape/notes/draft/undo", "", http.StatusOK)
	post("/api/shape/notes/draft/redo", "", http.StatusOK)
	post("/api/shape/notes/draft/undo", "", http.StatusOK)
	tip := v.Steps[1].Rev

	var saved draftSaved
	if err := json.Unmarshal([]byte(post("/api/shape/notes/draft/save", "", http.StatusOK)), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != tip || saved.Wishes != 2 || saved.Undone != 1 {
		t.Fatalf("saved = %+v", saved)
	}
	var hv approveHistoryView
	if err := json.Unmarshal([]byte(get("/api/shape/notes/history", http.StatusOK)), &hv); err != nil {
		t.Fatal(err)
	}
	if len(hv.Versions) != 1 {
		t.Fatalf("versions after one Save = %d, want 1", len(hv.Versions))
	}
	ver := hv.Versions[0]
	if ver.Revision != tip || hv.Active != tip {
		t.Fatalf("version = %+v (active %s), want the saved tip %s, active", ver, hv.Active, tip)
	}
	for _, want := range []string{"Edit session, 2 wishes", "1. show the newest note first", "2. tint the notes blue", "(undone) add an export button"} {
		if !strings.Contains(ver.Request, want) {
			t.Fatalf("version request %q lacks %q", ver.Request, want)
		}
	}
	if page := get("/notes/", http.StatusOK); !strings.Contains(page, "shaped a10 2") || strings.Contains(page, "shaped a10 3") {
		t.Fatalf("the live app is not the saved draft: %s", page)
	}
	get("/api/shape/notes/draft", http.StatusNotFound)
}
