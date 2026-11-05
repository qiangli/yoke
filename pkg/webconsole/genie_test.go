// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/websession"
)

// genieTagOf extracts the common Genie button's tag from a served page, or
// "" when the page carries none.
func genieTagOf(body string) string {
	i := strings.Index(body, "<script src=")
	for i >= 0 {
		j := strings.Index(body[i:], "</script>")
		if j < 0 {
			return ""
		}
		tag := body[i : i+j+len("</script>")]
		if strings.Contains(tag, "data-bashy-genie") {
			return tag
		}
		next := strings.Index(body[i+1:], "<script src=")
		if next < 0 {
			return ""
		}
		i += 1 + next
	}
	return ""
}

func wantTag(t *testing.T, where, tag string, attrs ...string) {
	t.Helper()
	if tag == "" {
		t.Fatalf("%s: page carries no Genie button", where)
	}
	for _, a := range attrs {
		if !strings.Contains(tag, a) {
			t.Errorf("%s: tag lacks %s:\n%s", where, a, tag)
		}
	}
}

// TestGenieButtonOnEveryShapedAppPage is the S526.B1g proof. (a) The one
// component is injected into every page the console serves for a shaped
// app — its own launcher pages, a registered app's pages, the sign-in page
// — addressing the launcher's root from under any mount; the trust-surface
// pages get protected mode and a preview gets preview mode. (b) The script
// is served from the embedded bytes at a fixed open route, is a protected
// manifest row, and a revision or candidate that changes it is refused.
// (c) A wish carries the app, the page and the pointed-at element to the
// agent's brief and the candidate record; from a protected page it is filed
// as a request and nothing is built. (d) Placement is the app's shaped
// config: a config-only candidate (shaped.json genie key) passes the real
// verifier, moves and hides the button, and the component is unchanged.
func TestGenieButtonOnEveryShapedAppPage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source := newNotesSource(t)
	shaper := &fakeShaper{}
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	s, h, closer, err := newHandler(Options{
		Ctx: context.Background(), RequireLogin: true,
		Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Panels:   []Panel{notes},
		Shape: ShapeOptions{
			Home: home, WorkRoot: filepath.Join(t.TempDir(), "shape-work"), Runner: shapedFixtureRunner(t),
			Level: contain.LevelProcess, Delegate: shaper,
			Verifier: productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: shapedFixtureRunner(t)[0], Effects: []string{"read"}}},
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
	get := func(who http.Header, path string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, who)
		if w.Code != want {
			t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w
	}
	post := func(who http.Header, path, body string, want int) Candidate {
		t.Helper()
		hdr := who.Clone()
		hdr.Set("Content-Type", "application/json")
		w := send(h, http.MethodPost, path, peer, hdr, body)
		if w.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		var c Candidate
		_ = json.Unmarshal(w.Body.Bytes(), &c)
		return c
	}
	embedded, err := fs.ReadFile(spaFS, genieAsset)
	if err != nil {
		t.Fatal(err)
	}
	const baseAttr = `data-base="/cloud/apps/"`
	const srcAttr = `<script src="/cloud/apps/_shape/genie.js?v=`

	// (a) Injection, page by page. The launcher's own pages anchor the
	// button in the header bar; the trust-surface pages are protected.
	wantTag(t, "launcher", genieTagOf(get(alice, "/", 200).Body.String()), srcAttr, baseAttr,
		`data-app="console"`, `data-page="/"`, `data-mode="live"`, `data-position="top-right"`,
		`data-anchor="#bar #nav, #bar"`, `data-hidden="0"`, `data-landing="1"`, `data-candidate=""`, `data-mount=""`)
	wantTag(t, "board", genieTagOf(get(alice, "/sprint/?x=1", 200).Body.String()), baseAttr,
		`data-app="console"`, `data-page="/sprint/"`, `data-mode="live"`, `data-landing="0"`)
	wantTag(t, "desktop", genieTagOf(get(alice, "/desktop/"+consoleTestPeer+"/", 200).Body.String()), baseAttr,
		`data-app="console"`, `data-page="/desktop/`+consoleTestPeer+`/"`, `data-mode="live"`)
	shapePage := get(alice, "/shape/console/", 200).Body.String()
	wantTag(t, "shape page", genieTagOf(shapePage), srcAttr, baseAttr, `data-app="console"`, `data-page="/shape/console/"`, `data-mode="protected"`)
	if strings.Contains(shapePage, `src="base/_shape`) {
		t.Fatalf("the protected page's asset rewrite must not touch the Genie tag:\n%s", shapePage)
	}
	login := do(h, http.MethodGet, "/login", "192.0.2.10:5555", nil)
	if login.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", login.Code)
	}
	wantTag(t, "sign-in", genieTagOf(login.Body.String()), `data-app="console"`, `data-page="/login"`, `data-mode="protected"`)
	// A registered shaped app's pages: the tag addresses the LAUNCHER's
	// root, not the app's mount, and the page is app-relative.
	wantTag(t, "notes", genieTagOf(get(alice, "/notes/", 200).Body.String()), srcAttr, baseAttr,
		`data-app="notes"`, `data-page="/"`, `data-mode="live"`, `data-position="top-right"`, `data-anchor=""`, `data-landing="1"`,
		`data-candidate=""`, `data-mount="notes/"`)
	wantTag(t, "notes index", genieTagOf(get(alice, "/notes/index.html?n=2", 200).Body.String()), baseAttr,
		`data-app="notes"`, `data-page="/index.html?n=2"`, `data-landing="1"`)
	if got := get(alice, "/notes/notes.js", 200).Body.String(); strings.Contains(got, "data-bashy-genie") {
		t.Fatal("a non-page asset received the Genie tag")
	}

	// (b) The script: embedded bytes at the fixed route, open to the
	// sign-in page's visitor too, nothing else under it.
	w := do(h, http.MethodGet, "/_shape/genie.js", "192.0.2.10:5555", nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), embedded) || w.Header().Get("ETag") == "" {
		t.Fatalf("/_shape/genie.js = %d, etag %q, embedded bytes %v", w.Code, w.Header().Get("ETag"), bytes.Equal(w.Body.Bytes(), embedded))
	}
	if w := do(h, http.MethodGet, "/_shape/shape.js", peer, alice); w.Code != http.StatusNotFound {
		t.Fatalf("/_shape/shape.js = %d, want 404", w.Code)
	}
	if got := src.PathClass("yoke", "pkg/webconsole/artifact/genie.js"); got != src.ClassProtected {
		t.Fatalf("PathClass(artifact/genie.js) = %s, want protected", got)
	}
	console := s.consoleShaped()
	repo, err := console.open()
	if err != nil {
		t.Fatal(err)
	}
	baseCommit, err := base(repo)
	if err != nil {
		t.Fatal(err)
	}
	hostile := consoleRevision(t, console, baseCommit.Hash.String(), map[string]string{
		"public/genie.js": "document.querySelector('[data-bashy-genie-btn]')?.remove();\n",
	})
	if err := console.guardProtected(repo, mustCommit(t, repo, hostile)); err == nil || !strings.Contains(err.Error(), "public/genie.js (overridden)") {
		t.Fatalf("guardProtected(genie override) = %v, want a refusal naming public/genie.js", err)
	}
	aliceState, _ := shape.OpenAt(home, consoleApp, alicePrincipal)
	if err := aliceState.Write(shape.State{ActiveRevision: hostile}); err != nil {
		t.Fatal(err)
	}
	if got := get(alice, "/genie.js", 200).Body.Bytes(); !bytes.Equal(got, embedded) {
		t.Fatal("a revision overriding genie.js was served")
	}
	if got := get(alice, "/_shape/genie.js", 200).Body.Bytes(); !bytes.Equal(got, embedded) {
		t.Fatal("the fixed route served a shaped genie.js")
	}
	if err := aliceState.Write(shape.State{}); err != nil {
		t.Fatal(err)
	}
	cand := consoleCandidate(t, console, alicePrincipal, "0123456789abcdef", baseCommit.Hash.String(), func(work string) {
		if err := os.WriteFile(filepath.Join(work, "public", "genie.js"), []byte("// shaped\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	verified := post(alice, "/api/shape/console/candidates/"+cand.ID+"/verify", "", http.StatusOK)
	if verified.Verdict == nil || verified.Verdict.Pass || !strings.Contains(verified.Verdict.Detail, "public/genie.js (overridden)") {
		t.Fatalf("verify of a candidate changing genie.js = %+v, want a failing verdict naming public/genie.js", verified.Verdict)
	}
	post(alice, "/api/shape/console/candidates/"+cand.ID+"/approve", "", http.StatusConflict)
	// A preview of the console: the component is there in preview mode and
	// names the candidate it reviews in place (the review bar).
	clean := consoleCandidate(t, console, alicePrincipal, "fedcba9876543210", baseCommit.Hash.String(), func(work string) {
		if err := os.WriteFile(filepath.Join(work, "public", "app.css"), []byte("/* candidate */\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	preview := get(alice, "/shape/console/preview/"+clean.ID+"/sprint/", 200).Body.String()
	wantTag(t, "console preview", genieTagOf(preview), srcAttr, baseAttr, `data-app="console"`, `data-page="/sprint/"`, `data-mode="preview"`,
		`data-candidate="`+clean.ID+`"`, `data-mount=""`)

	// (c) A wish from the notes app carries its page and element into the
	// record and the agent's brief; the wish still targets the app.
	c := post(alice, "/api/shape/notes/candidates",
		`{"request":"make the add button bigger","page":"/index.html?n=2","element":"form#add > button \"Add\"","protected":false}`, http.StatusAccepted)
	if c.Status != candRunning || c.Page != "/index.html?n=2" || c.Element != `form#add > button "Add"` || c.Kind != "" {
		t.Fatalf("wish record = %+v", c)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var cur Candidate
		_ = json.Unmarshal(get(alice, "/api/shape/notes/candidates/"+c.ID, 200).Body.Bytes(), &cur)
		if cur.Status == candReady {
			break
		}
		if cur.Status != candRunning || time.Now().After(deadline) {
			t.Fatalf("the wish never became ready: %+v", cur)
		}
		time.Sleep(50 * time.Millisecond)
	}
	shaper.mu.Lock()
	task := strings.Join(shaper.tasks, "\n")
	shaper.mu.Unlock()
	for _, want := range []string{"<<<CONTEXT", "page: /index.html?n=2", `element the user pointed at: form#add > button "Add"`, "CONTEXT>>>", "<<<REQUEST\nmake the add button bigger\nREQUEST>>>"} {
		if !strings.Contains(task, want) {
			t.Errorf("agent brief lacks %q:\n%s", want, task)
		}
	}
	if !strings.Contains(get(alice, "/api/shape/notes", 200).Body.String(), `"page":"/index.html?n=2"`) {
		t.Fatal("the Shape page's state does not carry the wish's page")
	}
	// From a protected page: filed as a request, nothing built, no
	// workspace, nothing to verify or approve; discardable. Genie is asked
	// one thing only — whether the wish is for a new app (S526.B1i) — under
	// a brief that permits no change; an ordinary answer leaves it filed.
	shaper.mu.Lock()
	before := len(shaper.tasks)
	shaper.mu.Unlock()
	req := post(alice, "/api/shape/console/candidates",
		`{"request":"a back button on the wish form","page":"/shape/console/","element":"","protected":true}`, http.StatusAccepted)
	if req.Status != candRequested || req.Kind != candKindRequest || req.Rev != "" || req.Page != "/shape/console/" || !req.Deciding {
		t.Fatalf("protected wish = %+v, want a filed request being read for intent", req)
	}
	deadline = time.Now().Add(20 * time.Second)
	for req.Deciding {
		if time.Now().After(deadline) {
			t.Fatalf("the filed request was never decided: %+v", req)
		}
		time.Sleep(50 * time.Millisecond)
		var cur Candidate
		_ = json.Unmarshal(get(alice, "/api/shape/console/candidates/"+req.ID, 200).Body.Bytes(), &cur)
		req = cur
	}
	if req.Status != candRequested || req.Rev != "" || req.NewApp != nil || req.Error != "" {
		t.Fatalf("decided filed request = %+v, want still filed, nothing built", req)
	}
	shaper.mu.Lock()
	asked := append([]string(nil), shaper.tasks[before:]...)
	shaper.mu.Unlock()
	if len(asked) != 1 || !strings.Contains(asked[0], "Make NO change") || strings.Contains(asked[0], "Make the smallest change") {
		t.Fatalf("a filed request was briefed as a change: %q", asked)
	}
	if dir, _ := console.candidateDir(alicePrincipal, req.ID); dir == "" || fileExists(filepath.Join(dir, "work")) {
		t.Fatal("a filed request has a workspace")
	}
	if !strings.Contains(get(alice, "/api/shape/console", 200).Body.String(), `"status":"requested"`) {
		t.Fatal("the Shape page's state does not list the request")
	}
	post(alice, "/api/shape/console/candidates/"+req.ID+"/verify", "", http.StatusConflict)
	post(alice, "/api/shape/console/candidates/"+req.ID+"/approve", "", http.StatusConflict)
	post(alice, "/api/shape/console/candidates/"+req.ID+"/reject", "", http.StatusOK)
	get(alice, "/api/shape/console/candidates/"+req.ID, http.StatusNotFound)
	if w := send(h, http.MethodPost, "/api/shape/notes/candidates", peer, alice, `{"request":"x","shortcut":"g"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown wish field accepted: %d", w.Code)
	}

	// (d) Placement is shaped config. A config-only candidate for notes
	// passes the real verifier; activated, it moves and hides the button
	// everywhere but the landing page; the script itself is unchanged.
	notesApp := s.shapedApp(notes)
	notesRepo, err := notesApp.open()
	if err != nil {
		t.Fatal(err)
	}
	notesBase, err := base(notesRepo)
	if err != nil {
		t.Fatal(err)
	}
	placed := consoleCandidate(t, notesApp, alicePrincipal, "abcdef0123456789", notesBase.Hash.String(), func(work string) {
		if err := os.WriteFile(filepath.Join(work, "shaped.json"), []byte(`{"genie": {"position": "bottom-left", "hidden": true}}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "public", "about.html"), []byte("<!doctype html><html><body><h1>Notes</h1>About</body></html>\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	placedV := post(alice, "/api/shape/notes/candidates/"+placed.ID+"/verify", "", http.StatusOK)
	if placedV.Verdict == nil || !placedV.Verdict.Pass {
		t.Fatalf("config-only candidate verdict = %+v, want pass", placedV.Verdict)
	}
	notesPreview := get(alice, "/shape/notes/preview/"+placed.ID+"/about.html", 200).Body.String()
	wantTag(t, "notes preview", genieTagOf(notesPreview), srcAttr, baseAttr, `data-app="notes"`, `data-page="/about.html"`,
		`data-mode="preview"`, `data-position="bottom-left"`, `data-hidden="1"`, `data-landing="0"`,
		`data-candidate="`+placed.ID+`"`, `data-mount="notes/"`)
	placedDir, _ := notesApp.candidateDir(alicePrincipal, placed.ID)
	if err := (&fakeActivator{home: home}).Activate(context.Background(), ActivateRequest{
		App: "notes", Principal: alicePrincipal, SourceDir: source, CandidateDir: filepath.Join(placedDir, "work"),
		BaseRev: placed.BaseRev, CandidateRev: placed.Rev,
	}); err != nil {
		t.Fatal(err)
	}
	wantTag(t, "notes placed landing", genieTagOf(get(alice, "/notes/", 200).Body.String()),
		`data-mode="live"`, `data-position="bottom-left"`, `data-hidden="1"`, `data-landing="1"`)
	wantTag(t, "notes placed page", genieTagOf(get(alice, "/notes/about.html", 200).Body.String()),
		`data-mode="live"`, `data-position="bottom-left"`, `data-hidden="1"`, `data-landing="0"`)
	if got := get(alice, "/_shape/genie.js", 200).Body.Bytes(); !bytes.Equal(got, embedded) {
		t.Fatal("placing the button changed the component")
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestGenieEarlyBootstrapFirstInHead pins the S549.R1e rework's early hook:
// the synchronous fetch wrapper is the first child of <head> — before any
// app script — while the component tag stays at </body>; a double pass
// injects neither twice, and a page with no head still gets the tag.
func TestGenieEarlyBootstrapFirstInHead(t *testing.T) {
	tag := genieTag{Base: "/", App: "notes", Page: "/", Mode: genieLive, Position: "top-right", Landing: true}
	page := `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="a.css"></head><body><script>fetch('api/list')</script></body></html>`
	out := string(injectGenie([]byte(page), tag))
	boot := strings.Index(out, genieEarlyBootstrap)
	if boot < 0 {
		t.Fatalf("no early bootstrap in:\n%s", out)
	}
	for name, want := range map[string]int{
		"<head>":            strings.Index(out, "<head>"),
		"<meta":             strings.Index(out, "<meta"),
		"fetch('api/list')": strings.Index(out, "fetch('api/list')"),
		"data-bashy-genie":  strings.Index(out, "data-bashy-genie"),
		"</body>":           strings.Index(out, "</body>"),
	} {
		if want < 0 {
			t.Fatalf("%s missing from:\n%s", name, out)
		}
	}
	head := strings.Index(out, "<head>")
	meta := strings.Index(out, "<meta")
	app := strings.Index(out, "fetch('api/list')")
	btn := strings.Index(out, "data-bashy-genie")
	if !(head < boot && boot < meta && boot < app && app < btn) {
		t.Fatalf("bootstrap is not first in head before the app script:\n%s", out)
	}
	again := string(injectGenie([]byte(out), tag))
	if strings.Count(again, genieEarlyBootstrap) != 1 || strings.Count(again, "data-bashy-genie") != 1 {
		t.Fatalf("double pass duplicated the injection:\n%s", again)
	}
	headless := string(injectGenie([]byte(`<p>bare</p></body>`), tag))
	if strings.Contains(headless, genieEarlyBootstrap) {
		t.Fatalf("bootstrap injected with no head:\n%s", headless)
	}
	if !strings.Contains(headless, "data-bashy-genie") {
		t.Fatalf("tag missing with no head:\n%s", headless)
	}
	if got := string(injectGenie([]byte(page), genieTag{})); got != page {
		t.Fatalf("empty app rewritten:\n%s", got)
	}
}

// TestGenieProxiedPageInjection: a declared server's HTML page gets the tag
// through the reverse proxy, with the length corrected; anything that is
// not a complete identity-encoded HTML success response passes untouched.
func TestGenieProxiedPageInjection(t *testing.T) {
	tag := func(page string) genieTag {
		return genieTag{Base: "/cloud/apps/", App: "notepad", Page: page, Mode: genieLive, Position: "top-right", Landing: genieLanding(page)}
	}
	resp := func(status int, ct, enc, body string, method string) *http.Response {
		req := httptest.NewRequest(method, "http://127.0.0.1:1/notes/42?x=1", nil)
		r := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}
		r.Header.Set("Content-Type", ct)
		if enc != "" {
			r.Header.Set("Content-Encoding", enc)
		}
		return r
	}
	page := "<!doctype html><html><body><h1>notepad</h1></body></html>"
	r := resp(200, "text/html; charset=utf-8", "", page, http.MethodGet)
	if err := injectGenieResponse(r, tag); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r.Body)
	got := genieTagOf(string(out))
	wantTag(t, "proxied page", got, `data-app="notepad"`, `data-page="/notes/42?x=1"`, `data-base="/cloud/apps/"`, `data-landing="0"`)
	if !strings.HasSuffix(strings.TrimSpace(string(out)), "</script>\n</body></html>") && !strings.Contains(string(out), got+"</body></html>") {
		t.Fatalf("tag is not at the end of the body:\n%s", out)
	}
	if r.ContentLength != int64(len(out)) || r.Header.Get("Content-Length") != strconv.Itoa(len(out)) {
		t.Fatalf("length not corrected: %d / %s for %d bytes", r.ContentLength, r.Header.Get("Content-Length"), len(out))
	}
	for name, r := range map[string]*http.Response{
		"gzip":   resp(200, "text/html", "gzip", page, http.MethodGet),
		"json":   resp(200, "application/json", "", `{"a":1}`, http.MethodGet),
		"head":   resp(200, "text/html", "", page, http.MethodHead),
		"status": resp(404, "text/html", "", page, http.MethodGet),
	} {
		if err := injectGenieResponse(r, tag); err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(r.Body)
		if strings.Contains(string(out), "data-bashy-genie") {
			t.Errorf("%s response was rewritten", name)
		}
	}
	// A page with no </body> still gets the tag, appended.
	r = resp(200, "text/html", "", "<p>bare", http.MethodGet)
	_ = injectGenieResponse(r, tag)
	out, _ = io.ReadAll(r.Body)
	if !strings.HasPrefix(string(out), "<p>bare<script src=") {
		t.Fatalf("bare page: %s", out)
	}
}

// TestGenieOnShapedServerPage: through the console's front door, a shaped
// app that declares a server gets the tag on its HTML page and nothing on
// its plain responses. Skipped without a go toolchain (the fixture builds).
func TestGenieOnShapedServerPage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, _, _, _, _ := serverAppSource(t, home)
	p := serverPanel(source)
	h := newServerAppHarness(t, home, p)
	root := http.Handler(h.s.root)
	w := do(root, http.MethodGet, "/serverapp/page", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /serverapp/page = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<h1>version=v0</h1>") {
		t.Fatalf("not the server's page: %s", body)
	}
	wantTag(t, "server page", genieTagOf(body), `<script src="/_shape/genie.js?v=`, `data-base="/"`, `data-app="serverapp"`, `data-page="/page"`, `data-mode="live"`)
	if cl := w.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(w.Body.Len()) {
		t.Fatalf("Content-Length %s for %d bytes", cl, w.Body.Len())
	}
	if got := do(root, http.MethodGet, "/serverapp/hello", "127.0.0.1:5555", nil).Body.String(); strings.Contains(got, "data-bashy-genie") || !strings.Contains(got, "version=v0 ") {
		t.Fatalf("plain response rewritten or lost: %q", got)
	}
}

// TestGenieAssetOpenPathIsOnlyTheScript pins the one gate change the
// component needs: with login required and no identity, the embedded
// script is reachable (the sign-in page carries it) and nothing else is —
// the launcher, the shaping API and pages, a shaped app, a sibling path
// under the same prefix, and anything that merely starts with it.
func TestGenieAssetOpenPathIsOnlyTheScript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source := newNotesSource(t)
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	h := newTestHandler(t, Options{
		RequireLogin: true,
		Sessions:     websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Panels:       []Panel{notes}, Shape: ShapeOptions{Home: home},
	})
	const stranger = "192.0.2.10:5555"
	if w := do(h, http.MethodGet, "/_shape/genie.js", stranger, nil); w.Code != http.StatusOK {
		t.Fatalf("/_shape/genie.js for a stranger = %d, want 200", w.Code)
	}
	for _, p := range []string{
		"/", "/index.html", "/app.js", "/notes/", "/notes/api/list", "/shape/console/", "/api/shape/console",
		"/api/shape/notes/candidates", "/_shape/shape.js", "/_shape/", "/_shape", "/_shapex/genie.js", "/base/app.js", "/sprint/",
	} {
		w := do(h, http.MethodGet, p, stranger, nil)
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "data-bashy-console") {
			t.Errorf("GET %s for a stranger = %d; the gate must refuse it", p, w.Code)
		}
		if w.Code != http.StatusFound && w.Code != http.StatusForbidden && w.Code != http.StatusNotFound && w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s for a stranger = %d, want a refusal", p, w.Code)
		}
	}
	if w := send(h, http.MethodPost, "/api/shape/console/candidates", stranger, http.Header{"Content-Type": {"application/json"}}, `{"request":"x","protected":true}`); w.Code == http.StatusAccepted {
		t.Fatal("a stranger filed a request")
	}
}

// TestGenieReviewTagNamesTheCandidate pins the server half of the in-place
// review: only a preview request carries a candidate, the tag names it and
// the app's live mount, and a panel's mount is carried relative with a
// trailing slash ("" for the launcher's root).
func TestGenieReviewTagNamesTheCandidate(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/about.html?x=1", nil)
	if id, ok := previewOf(r); ok || id != "" {
		t.Fatalf("a plain request previews %q", id)
	}
	live := genieTagFor(r, "notes", "notes/", nil, "/about.html?x=1", "")
	if live.Mode != genieLive || live.Candidate != "" || live.Mount != "notes/" {
		t.Fatalf("live tag = %+v", live)
	}
	pr := withPreview(r, "0123456789abcdef")
	if id, ok := previewOf(pr); !ok || id != "0123456789abcdef" {
		t.Fatalf("previewOf = %q, %v", id, ok)
	}
	tag := genieTagFor(pr, "notes", "notes/", nil, "/about.html?x=1", "")
	if tag.Mode != geniePreview || tag.Candidate != "0123456789abcdef" {
		t.Fatalf("preview tag = %+v", tag)
	}
	for _, want := range []string{`data-mode="preview"`, `data-candidate="0123456789abcdef"`, `data-mount="notes/"`} {
		if !strings.Contains(tag.html(), want) {
			t.Errorf("tag lacks %s: %s", want, tag.html())
		}
	}
	if withPreview(r, "").Context().Value(previewKey{}) != "" {
		t.Fatal("an empty candidate id is a preview")
	}
	if _, ok := previewOf(withPreview(r, "")); ok {
		t.Fatal("an empty candidate id counts as a preview")
	}
	for in, want := range map[string]string{"": "", "/": "", "/notes/": "notes/", "notes": "notes/", "/a/b/": "a/b/"} {
		if got := panelMount(in); got != want {
			t.Errorf("panelMount(%q) = %q, want %q", in, got, want)
		}
	}
}
