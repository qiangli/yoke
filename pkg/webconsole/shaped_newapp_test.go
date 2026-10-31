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

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/shape/contain"
)

// S526.B1i: Genie infers "make a new app" from the one Genie form. The
// delegate answers `NEW-APP: <name> | <label>` with no change; the server
// makes the blank app, opens the principal's draft on it, re-runs the wish
// there as the first step, and the record says new_app. A normal answer is
// still a candidate on the current app; NEW-APP together with a change is
// refused as ambiguous.

// intentShaper is a fake Genie with three answers: NEW-APP with no change
// (the default), the same plus a change (ambiguous), or an ordinary change
// (plain). On the new app's first wish — the brief says the app was just
// created for it — it builds the sign-up form, whatever the mode.
type intentShaper struct {
	mu        sync.Mutex
	tasks     []string
	ambiguous bool
	plain     bool
}

func (f *intentShaper) Delegate(_ context.Context, req DelegateRequest) (DelegateResult, error) {
	f.mu.Lock()
	f.tasks = append(f.tasks, req.Task)
	ambiguous, plain := f.ambiguous, f.plain
	f.mu.Unlock()
	index := filepath.Join(req.Dir, "public", "index.html")
	grow := func(with string) error {
		b, err := os.ReadFile(index)
		if err != nil {
			return err
		}
		return os.WriteFile(index, append(b, []byte(with)...), 0o644)
	}
	res := DelegateResult{Agent: "codex:fake-intent", Identity: "genie-delegate:codex-fake", Receipt: "r-intent"}
	switch {
	case strings.Contains(req.Task, "just created, blank"):
		if err := grow(`<form id="signup"><input name="name"> <input name="email"> <button>Sign up</button></form>`); err != nil {
			return res, err
		}
		res.Output = "Made the sign-up form.\n\nCOVERED:\n- sign-up form\nNOT COVERED:\n- (none)\n"
	case strings.Contains(req.Task, "made a request from one of its protected pages"):
		// The intent brief: no change, one line.
		if plain {
			res.Output = "FILED\n"
		} else {
			res.Output = "NEW-APP: signup | Sign-up sheet\n"
		}
	case plain:
		if err := grow("<!-- plain change -->\n"); err != nil {
			return res, err
		}
		res.Output = "Changed the page.\n\nCOVERED:\n- the change\nNOT COVERED:\n- (none)\n"
	default:
		if ambiguous {
			if err := grow("<!-- and a change here too -->\n"); err != nil {
				return res, err
			}
		}
		res.Output = "This asks for an app of its own.\n\n**NEW-APP:** signup | Sign-up sheet\n\nCOVERED:\n- (none)\nNOT COVERED:\n- (none)\n"
	}
	return res, nil
}

// newAppEnv is a console reading registered apps live (as the production
// console does), with the notes app registered as a shaped app and the
// loopback operator as principal.
type newAppEnv struct {
	t      *testing.T
	h      http.Handler
	shaper *intentShaper
}

func newNewAppEnv(t *testing.T) *newAppEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	source := newNotesSource(t)
	if err := fleet.New(fleet.WithAppValidate(ValidateRegisteredApp)).SaveApp(fleet.App{Name: "notes", Label: "Notes", Type: fleet.AppTypeShaped, Source: source}); err != nil {
		t.Fatal(err)
	}
	e := &newAppEnv{t: t, shaper: &intentShaper{}}
	e.h = newTestHandler(t, Options{Shape: ShapeOptions{
		Home: home, WorkRoot: filepath.Join(t.TempDir(), "shape-work"), Runner: []string{"/bin/sh"},
		Level: contain.LevelProcess, Delegate: e.shaper, Verifier: &fakeVerifier{}, Activator: &fakeActivator{home: home},
	}})
	return e
}

func (e *newAppEnv) get(path string) (int, string) {
	w := do(e.h, http.MethodGet, path, draftPeer, nil)
	return w.Code, w.Body.String()
}

func (e *newAppEnv) post(path, body string) (int, string) {
	hdr := http.Header{}
	if body != "" {
		hdr.Set("Content-Type", "application/json")
	}
	w := send(e.h, http.MethodPost, path, draftPeer, hdr, body)
	return w.Code, w.Body.String()
}

func (e *newAppEnv) mustJSON(path string, want int, into any) {
	e.t.Helper()
	code, body := e.get(path)
	if code != want {
		e.t.Fatalf("GET %s = %d, want %d: %s", path, code, want, body)
	}
	if err := json.Unmarshal([]byte(body), into); err != nil {
		e.t.Fatalf("GET %s: %v: %s", path, err, body)
	}
}

// wish posts a wish on the notes app and returns its settled candidate.
func (e *newAppEnv) wish(text string, protected bool) Candidate {
	e.t.Helper()
	body := `{"request":` + jsonQuote(text) + `,"page":"/","element":"","protected":` + map[bool]string{true: "true", false: "false"}[protected] + `}`
	code, out := e.post("/api/shape/notes/candidates", body)
	if code != http.StatusAccepted {
		e.t.Fatalf("wish = %d: %s", code, out)
	}
	var c Candidate
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		e.t.Fatal(err)
	}
	return e.settleCandidate(c.ID)
}

func (e *newAppEnv) settleCandidate(id string) Candidate {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var c Candidate
		e.mustJSON("/api/shape/notes/candidates/"+id, http.StatusOK, &c)
		if c.Status != candRunning && !c.Deciding {
			return c
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("candidate never settled: %+v", c)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settleDraft waits for the app's draft to have no wish in flight.
func (e *newAppEnv) settleDraft(app string) draftView {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var v draftView
		e.mustJSON("/api/shape/"+app+"/draft", http.StatusOK, &v)
		if !v.Busy {
			return v
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("draft of %s never settled: %+v", app, v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *newAppEnv) listed(app string) bool {
	e.t.Helper()
	_, body := e.get("/api/apps")
	return strings.Contains(body, `"name":"`+app+`"`)
}

const signupWish = "make a sign-up sheet app for Saturday's class, name and email, waitlist after 20"

// TestNewAppFromWishCandidate: Genie answers NEW-APP with no change → the
// server registers signup, opens the principal's draft on it, runs the
// wish there as the first step, and the candidate says new_app. A second
// such wish gets the next free name.
func TestNewAppFromWishCandidate(t *testing.T) {
	e := newNewAppEnv(t)
	c := e.wish(signupWish, false)
	if c.Status != candNewApp || c.NewApp == nil {
		t.Fatalf("candidate = %+v, want status new_app with the app", c)
	}
	if c.NewApp.App != "signup" || c.NewApp.Path != "/signup/" || c.NewApp.Label != "Sign-up sheet" || c.NewApp.Draft == nil {
		t.Fatalf("new_app = %+v", c.NewApp)
	}
	if c.Rev != "" || c.Error != "" {
		t.Fatalf("a new_app candidate has no revision and no error: %+v", c)
	}
	if !e.listed("signup") {
		t.Fatal("the new app is not listed by the launcher")
	}
	d := e.settleDraft("signup")
	if d.Wishes != 1 || len(d.Steps) != 1 || d.Steps[0].Request != signupWish || d.LastError != "" || d.Principal == "" {
		t.Fatalf("new app's draft = %+v", d)
	}
	if code, body := e.get("/shape/signup/draft/"); code != http.StatusOK || !strings.Contains(body, `id="signup"`) {
		t.Fatalf("draft page = %d %s", code, body)
	}
	// The wish re-run on the new app was briefed as its first wish.
	e.shaper.mu.Lock()
	tasks := append([]string(nil), e.shaper.tasks...)
	e.shaper.mu.Unlock()
	if len(tasks) != 2 || !strings.Contains(tasks[0], "NEW-APP:") || strings.Contains(tasks[0], "just created, blank") ||
		!strings.Contains(tasks[1], "just created, blank") || !strings.Contains(tasks[1], `"signup"`) {
		t.Fatalf("briefs = %d: first NEW-APP offer %v / second first-wish %v", len(tasks),
			len(tasks) > 0 && strings.Contains(tasks[0], "NEW-APP:"), len(tasks) > 1 && strings.Contains(tasks[1], "just created, blank"))
	}
	// Nothing changed on notes: no candidate revision, no draft.
	if code, _ := e.get("/api/shape/notes/draft"); code != http.StatusNotFound {
		t.Fatalf("notes got a draft: %d", code)
	}

	// The next new app of the same name gets a numeric suffix.
	c2 := e.wish("make another sign-up sheet app", false)
	if c2.Status != candNewApp || c2.NewApp == nil || c2.NewApp.App != "signup-2" || c2.NewApp.Path != "/signup-2/" {
		t.Fatalf("second new app = %+v", c2.NewApp)
	}
	if !e.listed("signup-2") {
		t.Fatal("signup-2 is not listed")
	}
	e.settleDraft("signup-2")
}

// TestNewAppFromWishPlainAnswerIsACandidate: an ordinary answer is still a
// candidate on the current app, and no app is made.
func TestNewAppFromWishPlainAnswerIsACandidate(t *testing.T) {
	e := newNewAppEnv(t)
	e.shaper.plain = true
	c := e.wish("add a back button", false)
	if c.Status != candReady || c.Rev == "" || c.NewApp != nil {
		t.Fatalf("candidate = %+v, want ready on notes", c)
	}
	if e.listed("signup") {
		t.Fatal("an app was made from a plain answer")
	}
}

// TestNewAppFromWishAmbiguousIsRefused: NEW-APP together with a change to
// the current app is refused — nothing is created and the wish fails with
// the reason.
func TestNewAppFromWishAmbiguousIsRefused(t *testing.T) {
	e := newNewAppEnv(t)
	e.shaper.ambiguous = true
	c := e.wish(signupWish, false)
	if c.Status != candFailed || !strings.Contains(c.Error, "ambiguous") || c.NewApp != nil {
		t.Fatalf("candidate = %+v, want failed as ambiguous", c)
	}
	if e.listed("signup") {
		t.Fatal("an app was made from an ambiguous answer")
	}
}

// TestNewAppFromWishDraft: the same decision on the draft-wish path — the
// wish makes no step on the notes draft; the draft points at the new app,
// whose draft carries the wish as its first step. The next wish clears it.
func TestNewAppFromWishDraft(t *testing.T) {
	e := newNewAppEnv(t)
	if code, body := e.post("/api/shape/notes/draft", ""); code != http.StatusCreated {
		t.Fatalf("start draft = %d %s", code, body)
	}
	if code, body := e.post("/api/shape/notes/draft/wishes", `{"request":`+jsonQuote(signupWish)+`,"page":"/","element":""}`); code != http.StatusAccepted {
		t.Fatalf("wish = %d %s", code, body)
	}
	d := e.settleDraft("notes")
	if d.NewApp == nil || d.NewApp.App != "signup" || d.NewApp.Draft == nil {
		t.Fatalf("notes draft = %+v, want new_app signup", d)
	}
	if d.Total != 0 || d.Wishes != 0 || d.LastError != "" {
		t.Fatalf("the wish left a step or an error on notes: %+v", d)
	}
	nd := e.settleDraft("signup")
	if nd.Wishes != 1 || nd.Steps[0].Request != signupWish || nd.LastError != "" {
		t.Fatalf("signup draft = %+v", nd)
	}
	// Ambiguous on the draft path: no step, the reason, no second app.
	e.shaper.mu.Lock()
	e.shaper.ambiguous = true
	e.shaper.mu.Unlock()
	if code, body := e.post("/api/shape/notes/draft/wishes", `{"request":"make a second app","page":"/","element":""}`); code != http.StatusAccepted {
		t.Fatalf("wish = %d %s", code, body)
	}
	d = e.settleDraft("notes")
	if d.NewApp != nil || !strings.Contains(d.LastError, "ambiguous") || d.Total != 0 {
		t.Fatalf("ambiguous wish on the draft = %+v", d)
	}
	if e.listed("signup-2") {
		t.Fatal("an app was made from an ambiguous answer")
	}
}

// TestNewAppFromProtectedWish: a wish filed from a protected page is still
// read for the one intent it can act on; NEW-APP makes the app, anything
// else leaves the request filed.
func TestNewAppFromProtectedWish(t *testing.T) {
	e := newNewAppEnv(t)
	e.shaper.plain = true
	c := e.wish("a back button on this page", true)
	if c.Status != candRequested || c.Kind != candKindRequest || c.Deciding || c.Error != "" || c.NewApp != nil {
		t.Fatalf("filed request = %+v", c)
	}
	e.shaper.mu.Lock()
	e.shaper.plain = false
	e.shaper.mu.Unlock()
	c = e.wish(signupWish, true)
	if c.Status != candNewApp || c.NewApp == nil || c.NewApp.App != "signup" || c.Kind != candKindRequest {
		t.Fatalf("protected new-app wish = %+v", c)
	}
	e.settleDraft("signup")
	e.shaper.mu.Lock()
	tasks := append([]string(nil), e.shaper.tasks...)
	e.shaper.mu.Unlock()
	for _, task := range tasks[:1] {
		if !strings.Contains(task, "Make NO change") {
			t.Fatalf("a protected wish was briefed as a change: %q", task)
		}
	}
}

func TestParseNewAppIntent(t *testing.T) {
	cases := []struct {
		in, name, label string
		ok              bool
	}{
		{"NEW-APP: signup | Sign-up sheet\n\nCOVERED:\n- (none)\n", "signup", "Sign-up sheet", true},
		{"Some words.\n**NEW-APP:** Sign-up Sheet | Sign-up sheet\n", "sign-up-sheet", "Sign-up sheet", true},
		{"- NEW-APP: `Saturday Class`\n", "saturday-class", "Saturday Class", true},
		{"NEW-APP: | Nothing\n", "", "", false},
		{"Changed the list.\nCOVERED:\n- newest first\n", "", "", false},
		{"the user said new-app: foo in passing\n", "", "", false},
	}
	for _, c := range cases {
		name, label, ok := parseNewAppIntent(c.in)
		if name != c.name || label != c.label || ok != c.ok {
			t.Errorf("parseNewAppIntent(%q) = %q, %q, %v; want %q, %q, %v", c.in, name, label, ok, c.name, c.label, c.ok)
		}
	}
	if got := appSlug("  Sign-up Sheet — Saturday's class!! "); got != "sign-up-sheet-saturday-s-class" {
		t.Errorf("appSlug = %q", got)
	}
}
